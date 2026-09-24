package objectstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
			client := Client{Endpoint: server.URL, Region: "garage", AccessKey: "access", SecretKey: "secret", HTTP: server.Client()}
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
