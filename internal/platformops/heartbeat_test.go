package platformops

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHeartbeatKeepsTokenInHeaderAndPreservesHTTPFailure(t *testing.T) {
	token := strings.Repeat("dummy_backup_token_", 4)
	for _, status := range []int{204, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/endpoints/backups_parser/external" || r.URL.RawQuery != "success=true" {
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

func TestBackupHeartbeatRejectsUndeclaredTargets(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	token := strings.Repeat("dummy_backup_token_", 4)
	for _, test := range []struct{ project, token string }{{"unknown", token}, {"parser", "short"}} {
		if err := Heartbeat(context.Background(), server.URL, test.project, test.token); err == nil {
			t.Errorf("heartbeat accepted project %q with a %d-byte token", test.project, len(test.token))
		}
	}
	if err := Heartbeat(context.Background(), server.URL, "parser", ""); err != nil || requests != 0 {
		t.Fatalf("unconfigured heartbeat returned %v after %d requests", err, requests)
	}
}

func TestReportedFailureCarriesItsError(t *testing.T) {
	token := strings.Repeat("dummy_verify_token_", 4)
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/endpoints/reconciliation_verification/external" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("heartbeat sent to %s", r.URL.Path)
		}
		query = r.URL.Query()
	}))
	defer server.Close()
	if err := ReportHeartbeat(context.Background(), server.URL, "reconciliation_verification", token, errors.New("run 7 concluded failure & more")); err != nil {
		t.Fatal(err)
	}
	if query.Get("success") != "false" || query.Get("error") != "run 7 concluded failure & more" {
		t.Fatalf("failure reported as %v", query)
	}
	if err := ReportHeartbeat(context.Background(), server.URL, "reconciliation_verification", "short", nil); err == nil {
		t.Fatal("invalid token accepted")
	}
}
