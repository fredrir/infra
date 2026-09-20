package platformops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHeartbeatKeepsTokenInHeaderAndPreservesHTTPFailure(t *testing.T) {
	token := strings.Repeat("dummy_backup_token_", 4)
	for _, status := range []int{204, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/endpoints/backups_parser/external" || r.URL.Query().Get("success") != "true" {
					t.Error("invalid heartbeat request")
				}
				if r.Header.Get("Authorization") != "Bearer "+token || strings.Contains(r.URL.String(), token) {
					t.Error("invalid token placement")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			e := Heartbeat(context.Background(), server.URL, "parser", token)
			if (e == nil) != (status == 204) {
				t.Fatalf("status %d: %v", status, e)
			}
			if e != nil && strings.Contains(e.Error(), token) {
				t.Fatal("token leaked")
			}
		})
	}
}
