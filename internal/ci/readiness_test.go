package ci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadinessRequiresExpectedRevision(t *testing.T) {
	want := strings.Repeat("a", 40)
	for _, body := range []string{want, strings.Repeat("b", 40), "healthy"} {
		t.Run(body[:4], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Cache-Control") == "" {
					t.Error("missing cache bypass")
				}
				w.Write([]byte(body))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			err := WaitRevision(ctx, server.Client(), server.URL, want, time.Millisecond)
			if (err == nil) != (body == want) {
				t.Fatalf("body %q readiness %v", body, err)
			}
		})
	}
}
