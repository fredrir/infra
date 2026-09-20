package enrollment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOnlySuccessfulKeyDeletionAcceptsNull(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		status             int
		accepted           bool
	}{
		{"DELETE", "/tailnet/-/keys/kFixture123", "null", 200, true},
		{"DELETE", "/tailnet/-/keys/kFixture123", "", 204, true},
		{"GET", "/tailnet/-/keys/kFixture123", "null", 200, false},
		{"DELETE", "/unrelated", "null", 200, false},
		{"DELETE", "/tailnet/-/keys/kFixture123", "[]", 200, false},
		{"GET", "/tailnet/-/keys/kFixture123", "private-body", 404, true},
		{"GET", "/tailnet/-/keys/kFixture123", "{} {}", 200, false},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			_, err := HTTPAPI(server.URL, server.Client())(context.Background(), Request{Method: tc.method, Path: tc.path, Token: "private-token"})
			if (err == nil) != tc.accepted {
				t.Fatalf("response accepted=%v error=%v", tc.accepted, err)
			}
		})
	}
}

func TestAPIRefusesRedirectAndWithholdsResponse(t *testing.T) {
	for _, status := range []int{302, 500} {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			w.Header().Set("Location", "/private-token")
			w.WriteHeader(status)
			fmt.Fprint(w, "private-response")
		}))
		_, err := HTTPAPI(server.URL, server.Client())(context.Background(), Request{Method: "GET", Path: "/tailnet/-/keys", Token: "private-token"})
		server.Close()
		if err == nil || strings.Contains(err.Error(), "private-") || requests != 1 {
			t.Fatalf("unsafe API failure: %v requests=%d", err, requests)
		}
	}
}

func TestOAuthRejectsScopeWideningAndMalformedExpiry(t *testing.T) {
	for _, change := range []map[string]any{{"scope": "auth_keys devices"}, {"expires_in": json.Number("3601")}, {"expires_in": json.Number("60.0")}, {"expires_in": "3600"}, {"token_type": "Basic"}} {
		api := func(context.Context, Request) (map[string]any, error) {
			d := map[string]any{"access_token": "secret", "token_type": "Bearer", "scope": "auth_keys", "expires_in": json.Number("3600")}
			for k, v := range change {
				d[k] = v
			}
			return d, nil
		}
		_, err := accessToken(context.Background(), api, "control", map[string]string{"TS_API_CLIENT_ID": "id", "TS_API_CLIENT_SECRET": "secret"})
		if err == nil {
			t.Fatalf("accepted widened token: %v", change)
		}
	}
}

func TestInventoryRejectsMalformedEntries(t *testing.T) {
	for _, document := range []map[string]any{{}, nil, {"keys": map[string]any{}}, {"keys": ""}, {"keys": []any{map[string]any{"id": 1}}}} {
		_, err := keyIDs(context.Background(), func(context.Context, Request) (map[string]any, error) { return document, nil }, "token")
		if err == nil {
			t.Fatalf("accepted malformed inventory: %v", document)
		}
	}
	for _, value := range []any{nil, []any{}} {
		ids, err := keyIDs(context.Background(), func(context.Context, Request) (map[string]any, error) { return map[string]any{"keys": value}, nil }, "token")
		if err != nil || len(ids) != 0 {
			t.Fatal("empty inventory rejected")
		}
	}
}

func TestRevocationIsBoundToNodeAndRole(t *testing.T) {
	f := &fakeAPI{keys: map[string]map[string]any{}}
	client := fixtureClient(f, func(context.Context, string, Target, map[string]any) error { return nil })
	if _, err := client.CreateDeliver(context.Background(), fixtureTarget()); err != nil {
		t.Fatal(err)
	}
	target := fixtureTarget()
	target.KeyID = "kFixture123"
	target.Role = "worker"
	if _, err := client.RevokeUnused(context.Background(), target); err == nil || len(f.keys) != 1 {
		t.Fatal("foreign key revoked")
	}
	target.Role = "control"
	if _, err := client.RevokeUnused(context.Background(), target); err != nil || len(f.keys) != 0 {
		t.Fatalf("owned key retained: %v", err)
	}
}

func TestRevocationRefusesMismatchedResponseIdentity(t *testing.T) {
	f := &fakeAPI{keys: map[string]map[string]any{}}
	client := fixtureClient(f, func(context.Context, string, Target, map[string]any) error { return nil })
	if _, err := client.CreateDeliver(context.Background(), fixtureTarget()); err != nil {
		t.Fatal(err)
	}
	f.keys["kFixture123"]["id"] = "kAnother123"
	target := fixtureTarget()
	target.KeyID = "kFixture123"
	if _, err := client.RevokeUnused(context.Background(), target); err == nil || len(f.keys) != 1 {
		t.Fatal("mismatched key revoked")
	}
	if _, err := (Client{}).RevokeUnused(context.Background(), target); err == nil {
		t.Fatal("missing clients accepted")
	}
}
