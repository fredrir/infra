package objectstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
)

var immediately = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })

func TestObjectUploadSignsPayloadAndKeepsCredentialsOutOfURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/bucket/target/file" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		data, _ := io.ReadAll(r.Body)
		if string(data) != "artifact" {
			t.Error("payload changed")
		}
		if r.Header.Get("X-Amz-Content-Sha256") != fmt.Sprintf("%x", sha256.Sum256(data)) {
			t.Error("payload hash mismatch")
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=access/") {
			t.Error("missing signature")
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	client := Client{Endpoint: server.URL, Region: "garage", AccessKey: "access", SecretKey: "secret", HTTP: server.Client()}
	if err := client.Upload(context.Background(), "bucket", "target/file", strings.NewReader("artifact")); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyUnconditionalWritesReplayOnStaleConnection(t *testing.T) {
	for _, test := range []struct {
		name     string
		headers  http.Header
		replayed bool
	}{
		{"unconditional", nil, true},
		{"conditional", http.Header{"If-Match": {`"etag"`}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var puts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if len(r.Header.Values("Idempotency-Key")) > 0 {
					t.Error("idempotency marker sent")
				}
				io.Copy(io.Discard, r.Body)
				if r.Method == http.MethodPut && puts.Add(1) == 1 {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close()
					return
				}
				w.WriteHeader(200)
			}))
			defer server.Close()
			client := Client{Endpoint: server.URL, Region: "garage", AccessKey: "access", SecretKey: "secret", HTTP: server.Client(), Backoff: immediately}
			if err := client.Download(context.Background(), "bucket", "warm", io.Discard); err != nil {
				t.Fatal(err)
			}
			response, err := client.RequestHeaders(context.Background(), http.MethodPut, "bucket", "status.json", strings.NewReader("{}"), test.headers)
			if err == nil {
				response.Body.Close()
			}
			if (err == nil) != test.replayed || puts.Load() != map[bool]int32{true: 2, false: 1}[test.replayed] {
				t.Fatalf("replayed=%v attempts=%d err=%v", test.replayed, puts.Load(), err)
			}
		})
	}
}

func TestObjectStoreErrorsDoNotExposeResponseSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); io.WriteString(w, "secret-token") }))
	defer server.Close()
	client := Client{Endpoint: server.URL, Region: "garage", AccessKey: "access", SecretKey: "secret", HTTP: server.Client()}
	err := client.Download(context.Background(), "bucket", "object", io.Discard)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}

type failure func(http.ResponseWriter)

func dropConnection(reset bool) failure {
	return func(w http.ResponseWriter) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		if tcp, ok := connection.(*net.TCPConn); ok && reset {
			tcp.SetLinger(0)
		}
		connection.Close()
	}
}

func answer(status int, code string) failure {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		fmt.Fprintf(w, "<Error><Code>%s</Code><Message>secret-detail</Message></Error>", code)
	}
}

type flakyStore struct {
	t        *testing.T
	failures []failure
	attempts atomic.Int32
}

func (s *flakyStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("X-Amz-Content-Sha256") != fmt.Sprintf("%x", sha256.Sum256(body)) {
		s.t.Errorf("attempt %d sent a body that does not match its signature", s.attempts.Load()+1)
	}
	if attempt := int(s.attempts.Add(1)); attempt <= len(s.failures) {
		s.failures[attempt-1](w)
		return
	}
	w.Header().Set("ETag", `"stored"`)
	w.Write(body)
}

func flakyClient(t *testing.T, failures ...failure) (Client, *flakyStore) {
	t.Helper()
	store := &flakyStore{t: t, failures: failures}
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	return Client{Endpoint: server.URL, Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", HTTP: server.Client(), Backoff: immediately}, store
}

func TestTransientFailuresAreRetried(t *testing.T) {
	failures := map[string]failure{"dropped connection": dropConnection(false), "reset connection": dropConnection(true), "slow down": answer(http.StatusServiceUnavailable, "SlowDown"), "internal error": answer(http.StatusInternalServerError, "InternalError"), "bad gateway": answer(http.StatusBadGateway, "")}
	for name, fail := range failures {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
			t.Run(name+" "+method, func(t *testing.T) {
				client, store := flakyClient(t, fail)
				response, err := client.Request(context.Background(), method, "bucket", "status.json", strings.NewReader("payload"))
				if err != nil {
					t.Fatalf("%s after %d attempts: %v", method, store.attempts.Load(), err)
				}
				response.Body.Close()
				if attempts := store.attempts.Load(); attempts != 2 {
					t.Fatalf("%s took %d attempts", method, attempts)
				}
			})
		}
	}
}

type stallingListener struct {
	net.Listener
	mu      sync.Mutex
	stalled []net.Conn
}

func (l *stallingListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		first := len(l.stalled) == 0
		if first {
			l.stalled = append(l.stalled, connection)
		}
		l.mu.Unlock()
		if !first {
			return connection, nil
		}
	}
}

func TestTLSHandshakeTimeoutIsRetriedForConditionalRequests(t *testing.T) {
	store := &flakyStore{t: t}
	server := httptest.NewUnstartedServer(store)
	listener := &stallingListener{Listener: server.Listener}
	server.Listener = listener
	server.StartTLS()
	t.Cleanup(func() {
		server.Close()
		for _, connection := range listener.stalled {
			connection.Close()
		}
	})
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = 100 * time.Millisecond
	client := Client{Endpoint: server.URL, Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", HTTP: &http.Client{Transport: transport}, Backoff: immediately}
	response, err := client.RequestHeaders(context.Background(), http.MethodPut, "bucket", "lock.json", strings.NewReader("lease"), http.Header{"If-None-Match": {"*"}})
	if err != nil {
		t.Fatalf("PUT after a TLS handshake timeout: %v", err)
	}
	response.Body.Close()
	if attempts := store.attempts.Load(); attempts != 1 || len(listener.stalled) != 1 {
		t.Fatalf("%d attempts reached the store after %d stalled handshakes", attempts, len(listener.stalled))
	}
}

func TestSentConditionalRequestsAreNotReplayed(t *testing.T) {
	for _, test := range []struct {
		name        string
		fail        failure
		attempts    int32
		unconfirmed bool
		status      int
	}{
		{name: "dropped after the request", fail: dropConnection(false), attempts: 1, unconfirmed: true},
		{name: "reset after the request", fail: dropConnection(true), attempts: 1, unconfirmed: true},
		{name: "internal error", fail: answer(http.StatusInternalServerError, "InternalError"), attempts: 1, unconfirmed: true, status: http.StatusInternalServerError},
		{name: "slow down", fail: answer(http.StatusServiceUnavailable, "SlowDown"), attempts: 2},
		{name: "precondition failed", fail: answer(http.StatusPreconditionFailed, "PreconditionFailed"), attempts: 1, status: http.StatusPreconditionFailed},
		{name: "conflict", fail: answer(http.StatusConflict, "ConditionalRequestConflict"), attempts: 1, status: http.StatusConflict},
	} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			t.Run(test.name+" "+method, func(t *testing.T) {
				client, store := flakyClient(t, test.fail)
				response, err := client.RequestHeaders(context.Background(), method, "bucket", "lock.json", strings.NewReader("lease"), http.Header{"If-Match": {`"held"`}})
				if err == nil {
					response.Body.Close()
				}
				var status *StatusError
				if attempts := store.attempts.Load(); attempts != test.attempts || errors.Is(err, ErrUnconfirmed) != test.unconfirmed || (test.status != 0) != errors.As(err, &status) || test.status != 0 && status.Status != test.status {
					t.Fatalf("%d attempts returned %v", attempts, err)
				}
			})
		}
	}
}

func TestRetriesAreBoundedAndStopOnDefinitiveAnswers(t *testing.T) {
	for _, test := range []struct {
		name     string
		fail     failure
		attempts int32
	}{
		{name: "persistent outage", fail: answer(http.StatusServiceUnavailable, "ServiceUnavailable"), attempts: int32(retry.DefaultMaxAttempts)},
		{name: "persistent drops", fail: dropConnection(true), attempts: int32(retry.DefaultMaxAttempts)},
		{name: "access denied", fail: answer(http.StatusForbidden, "AccessDenied"), attempts: 1},
		{name: "missing", fail: answer(http.StatusNotFound, "NoSuchKey"), attempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, store := flakyClient(t, test.fail, test.fail, test.fail, test.fail)
			err := client.Download(context.Background(), "bucket", "status.json", io.Discard)
			if attempts := store.attempts.Load(); err == nil || attempts != test.attempts || strings.Contains(err.Error(), "secret-detail") {
				t.Fatalf("%d attempts returned %v", attempts, err)
			}
		})
	}
}

func TestCancellationInterruptsBackoff(t *testing.T) {
	client, store := flakyClient(t, answer(http.StatusServiceUnavailable, "SlowDown"))
	client.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return time.Hour, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := client.Download(ctx, "bucket", "status.json", io.Discard)
	var status *StatusError
	if waited := time.Since(started); !errors.As(err, &status) || status.Code != "SlowDown" || waited > 10*time.Second || store.attempts.Load() != 1 {
		t.Fatalf("cancelled backoff returned %v after %s and %d attempts", err, waited, store.attempts.Load())
	}
}
