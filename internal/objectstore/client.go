package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

var ErrUnconfirmed = errors.New("conditional request may have been applied")

type Client struct {
	Endpoint, Region, AccessKey, SecretKey string
	SessionToken                           string
	HTTP                                   *http.Client
	Backoff                                retry.BackoffDelayer
}

var (
	bucketPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	errorCodePattern = regexp.MustCompile(`^[A-Za-z]{1,64}$`)
	throttles        = retry.IsErrorThrottles(retry.DefaultThrottles)
	endOfStream      = retry.IsErrorRetryableFunc(func(err error) aws.Ternary {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return aws.TrueTernary
		}
		return aws.UnknownTernary
	})
)

func (client Client) Request(ctx context.Context, method, bucket, key string, body io.ReadSeeker) (*http.Response, error) {
	return client.RequestHeaders(ctx, method, bucket, key, body, nil)
}

// RequestHeaders retries transport failures and 5xx responses with jittered backoff, except that a conditional request which may have been applied returns ErrUnconfirmed instead.
func (client Client) RequestHeaders(ctx context.Context, method, bucket, key string, body io.ReadSeeker, headers http.Header) (*http.Response, error) {
	base, err := url.Parse(client.Endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || client.Region == "" || client.AccessKey == "" || client.SecretKey == "" {
		return nil, fmt.Errorf("invalid object store configuration")
	}
	if !bucketPattern.MatchString(bucket) || key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\x00\r\n") {
		return nil, fmt.Errorf("invalid object store path")
	}
	for _, component := range strings.Split(key, "/") {
		if component == ".." || component == "." || component == "" {
			return nil, fmt.Errorf("invalid object store key")
		}
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + bucket + "/" + key
	if body == nil {
		body = bytes.NewReader(nil)
	}
	digest := sha256.New()
	size, err := io.Copy(digest, body)
	if err != nil {
		return nil, err
	}
	payload := request{method: method, target: base.String(), body: body, size: size, hash: hex.EncodeToString(digest.Sum(nil)), headers: headers}
	conditional := headers.Get("If-Match") != "" || headers.Get("If-None-Match") != ""
	retryer := retry.NewStandard(func(options *retry.StandardOptions) {
		options.Retryables = append(options.Retryables, endOfStream)
		if client.Backoff != nil {
			options.Backoff = client.Backoff
		}
	})
	var released <-chan struct{}
	for attempt := 1; ; attempt++ {
		if released != nil {
			<-released
		}
		var response *http.Response
		var reached bool
		response, reached, released, err = client.send(ctx, payload)
		if err == nil {
			return response, nil
		}
		var status *StatusError
		answered := errors.As(err, &status)
		switch {
		case conditional && (reached && !answered || answered && status.Status >= 500 && !throttles.IsErrorThrottle(err).Bool()):
			return nil, fmt.Errorf("%w: %w", ErrUnconfirmed, err)
		case ctx.Err() != nil || !retryer.IsErrorRetryable(err) || attempt >= retryer.MaxAttempts():
			return nil, err
		}
		delay, delayErr := retryer.RetryDelay(attempt, err)
		if delayErr != nil {
			return nil, errors.Join(err, delayErr)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
	}
}

type request struct {
	method, target, hash string
	body                 io.ReadSeeker
	size                 int64
	headers              http.Header
}

type attemptBody struct {
	io.Reader
	close func()
}

func (body attemptBody) Close() error {
	body.close()
	return nil
}

func (client Client) send(ctx context.Context, payload request) (*http.Response, bool, <-chan struct{}, error) {
	if _, err := payload.body.Seek(0, io.SeekStart); err != nil {
		return nil, false, nil, err
	}
	var reached atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { reached.Store(true) }})
	var body io.ReadCloser = http.NoBody
	var released chan struct{}
	if payload.size > 0 {
		released = make(chan struct{})
		body = attemptBody{Reader: payload.body, close: sync.OnceFunc(func() { close(released) })}
	}
	request, err := http.NewRequestWithContext(ctx, payload.method, payload.target, body)
	if err != nil {
		return nil, false, nil, err
	}
	request.ContentLength = payload.size
	for name, values := range payload.headers {
		request.Header[name] = append([]string(nil), values...)
	}
	request.Header.Set("X-Amz-Content-Sha256", payload.hash)
	if err := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: client.AccessKey, SecretAccessKey: client.SecretKey, SessionToken: client.SessionToken}, request, payload.hash, "s3", client.Region, time.Now().UTC()); err != nil {
		return nil, false, nil, fmt.Errorf("sign object store request: %w", err)
	}
	if request.Header.Get("If-Match") == "" && request.Header.Get("If-None-Match") == "" {
		// A nil Idempotency-Key lets net/http replay the request on a stale keep-alive connection without sending the header.
		request.Header["Idempotency-Key"] = nil
	}
	httpClient := client.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, reached.Load(), released, fmt.Errorf("object store request failed: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		var failure struct{ Code string }
		if xml.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&failure) != nil || !errorCodePattern.MatchString(failure.Code) {
			failure.Code = ""
		}
		return nil, true, released, &StatusError{Status: response.StatusCode, Code: failure.Code}
	}
	return response, true, released, nil
}

type StatusError struct {
	Status int
	Code   string
}

func (err *StatusError) Error() string {
	return strings.TrimSpace(fmt.Sprintf("object store returned HTTP %d %s", err.Status, err.Code))
}

func (err *StatusError) HTTPStatusCode() int { return err.Status }

func (err *StatusError) ErrorCode() string { return err.Code }

func (client Client) Download(ctx context.Context, bucket, key string, destination io.Writer) error {
	response, err := client.Request(ctx, http.MethodGet, bucket, key, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(destination, response.Body)
	return err
}
func (client Client) Upload(ctx context.Context, bucket, key string, source io.ReadSeeker) error {
	response, err := client.Request(ctx, http.MethodPut, bucket, key, source)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return err
}
func (client Client) Delete(ctx context.Context, bucket, key string) error {
	response, err := client.Request(ctx, http.MethodDelete, bucket, key, nil)
	if err != nil {
		return err
	}
	return response.Body.Close()
}
