package objectstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestObjectStoreErrorsDoNotExposeResponseSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); io.WriteString(w, "secret-token") }))
	defer server.Close()
	client := Client{Endpoint: server.URL, Region: "garage", AccessKey: "access", SecretKey: "secret", HTTP: server.Client()}
	err := client.Download(context.Background(), "bucket", "object", io.Discard)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}
