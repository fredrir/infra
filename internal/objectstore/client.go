package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

type Client struct {
	Endpoint, Region, AccessKey, SecretKey string
	SessionToken                           string
	HTTP                                   *http.Client
}

func (client Client) Request(ctx context.Context, method, bucket, key string, body io.ReadSeeker) (*http.Response, error) {
	return client.RequestHeaders(ctx, method, bucket, key, body, nil)
}

func (client Client) RequestHeaders(ctx context.Context, method, bucket, key string, body io.ReadSeeker, headers http.Header) (*http.Response, error) {
	base, err := url.Parse(client.Endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || client.Region == "" || client.AccessKey == "" || client.SecretKey == "" {
		return nil, fmt.Errorf("invalid object store configuration")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`).MatchString(bucket) || key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\x00\r\n") {
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
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, base.String(), body)
	if err != nil {
		return nil, err
	}
	request.ContentLength = size
	for name, values := range headers {
		request.Header[name] = append([]string(nil), values...)
	}
	payloadHash := hex.EncodeToString(digest.Sum(nil))
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: client.AccessKey, SecretAccessKey: client.SecretKey, SessionToken: client.SessionToken}, request, payloadHash, "s3", client.Region, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("sign object store request: %w", err)
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
		return nil, fmt.Errorf("object store request failed: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, &StatusError{Code: response.StatusCode}
	}
	return response, nil
}

type StatusError struct{ Code int }

func (err *StatusError) Error() string { return fmt.Sprintf("object store returned HTTP %d", err.Code) }

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
