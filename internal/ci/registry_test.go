package ci

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

func TestTagImageVerifiesManifestBeforePublishing(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			published := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					user, password, ok := r.BasicAuth()
					if !ok || user != "actor" || password != "secret" || r.URL.Query().Get("scope") != "repository:fredrir/example:pull,push" {
						t.Error("incorrect authentication")
					}
					io.WriteString(w, `{"token":"token"}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer token" {
					t.Error("missing token")
				}
				if r.Method == "PUT" {
					data, _ := io.ReadAll(r.Body)
					if string(data) != string(manifest) {
						t.Error("manifest changed")
					}
					published = true
					w.WriteHeader(201)
					return
				}
				w.Write(manifest)
				if corrupt {
					io.WriteString(w, " ")
				}
			}))
			defer server.Close()
			err := TagImage(context.Background(), server.Client(), TagOptions{Image: fmt.Sprintf("ghcr.io/fredrir/example@sha256:%x", sha256.Sum256(manifest)), Tag: "inputs-abc", Registry: server.URL, Actor: "actor", Token: "secret"})
			if corrupt {
				if err == nil || published {
					t.Fatal("published corrupt manifest")
				}
			} else if err != nil || !published {
				t.Fatalf("publish=%v error=%v", published, err)
			}
		})
	}
}

func TestTagImageRejectsUnsafeReferencesBeforeRequests(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(500) }))
	defer server.Close()
	for _, reference := range []string{"ghcr.io/other/example@sha256:" + strings.Repeat("a", 64), "ghcr.io/fredrir/example:latest"} {
		if err := TagImage(context.Background(), server.Client(), TagOptions{Image: reference, Tag: "tag", Registry: server.URL, Actor: "actor", Token: "secret"}); err == nil {
			t.Error("accepted unsafe reference")
		}
	}
	if requests != 0 {
		t.Fatal("requested registry for invalid inputs")
	}
}
