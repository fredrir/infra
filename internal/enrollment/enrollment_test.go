package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var fixtureNow = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
var fixtureKey = "tskey-auth-" + strings.Repeat("x", 48)

type fakeAPI struct {
	keys                          map[string]map[string]any
	mutate                        func(map[string]any)
	failCreate, failDelete, drift bool
	calls                         []Request
}

func tailscaleDescription(description any) any {
	if text, ok := description.(string); ok && len(text) > 50 {
		return text[:50]
	}
	return description
}

func (f *fakeAPI) call(_ context.Context, r Request) (map[string]any, error) {
	f.calls = append(f.calls, r)
	if r.Path == "/oauth/token" {
		return map[string]any{"access_token": "private-token", "token_type": "Bearer", "scope": "auth_keys", "expires_in": json.Number("3600")}, nil
	}
	if r.Method == "POST" {
		result := copyDocument(map[string]any{"id": "kFixture123", "key": fixtureKey, "created": fixtureNow.Format(time.RFC3339), "expires": fixtureNow.Add(600 * time.Second).Format(time.RFC3339), "description": tailscaleDescription(r.Document["description"]), "capabilities": r.Document["capabilities"], "invalid": false})
		if f.mutate != nil {
			f.mutate(result)
		}
		id, _ := result["id"].(string)
		f.keys[id] = copyDocument(result)
		if f.failCreate {
			return nil, errors.New("lost private creation response")
		}
		return result, nil
	}
	id := r.Path[strings.LastIndex(r.Path, "/")+1:]
	if r.Method == "DELETE" {
		if f.failDelete {
			return nil, errors.New("private delete error")
		}
		delete(f.keys, id)
		return map[string]any{}, nil
	}
	if r.Path == "/tailnet/-/keys" {
		var keys []any
		for id := range f.keys {
			keys = append(keys, map[string]any{"id": id})
		}
		return map[string]any{"keys": keys}, nil
	}
	result := copyDocument(f.keys[id])
	if result != nil && f.drift {
		result["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)["reusable"] = true
	}
	return result, nil
}

func copyDocument(value map[string]any) map[string]any {
	data, _ := json.Marshal(value)
	var result map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	_ = d.Decode(&result)
	return result
}

func fixtureTarget() Target {
	return Target{Node: "fredrir-07", Role: "control", Host: "fredrir-07", KeyFile: KeyFile}
}
func fixtureClient(f *fakeAPI, transport Transport) Client {
	return Client{API: f.call, Transport: transport, Now: func() time.Time { return fixtureNow }, Environment: map[string]string{"TS_API_CLIENT_ID": "client", "TS_API_CLIENT_SECRET": "oauth-secret"}}
}

func TestEnrollmentNarrowsRoleAndDeliversOnlyAfterVerification(t *testing.T) {
	for role, tag := range roles {
		f := &fakeAPI{keys: map[string]map[string]any{}}
		var modes []string
		client := fixtureClient(f, func(_ context.Context, mode string, target Target, payload map[string]any) error {
			modes = append(modes, mode)
			if mode == "deliver" && payload["key"] != fixtureKey {
				t.Fatal("key missing")
			}
			return nil
		})
		target := fixtureTarget()
		target.Role = role
		result, err := client.CreateDeliver(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(modes) != "[preflight deliver]" || len(f.keys) != 1 {
			t.Fatalf("bad sequence: %v", modes)
		}
		if f.calls[0].Form.Get("scope") != "auth_keys" || f.calls[0].Form.Get("tags") != tag {
			t.Fatal("wide OAuth request")
		}
		data, _ := json.Marshal(result)
		if bytes.Contains(data, []byte(fixtureKey)) || bytes.Contains(data, []byte("private-token")) {
			t.Fatal("receipt leaked secret")
		}
	}
}

func TestUnsafeMetadataIsRevokedWithoutDelivery(t *testing.T) {
	mutations := map[string]func(map[string]any){
		"reusable": func(d map[string]any) {
			d["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)["reusable"] = true
		},
		"wrong role": func(d map[string]any) { d["capabilities"] = capabilities("worker") },
		"numeric flag": func(d map[string]any) {
			d["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)["reusable"] = 0
		},
		"long expiry": func(d map[string]any) { d["expires"] = fixtureNow.Add(time.Hour).Format(time.RFC3339) },
		"stale": func(d map[string]any) {
			d["created"] = fixtureNow.Add(-2 * time.Minute).Format(time.RFC3339)
			d["expires"] = fixtureNow.Add(8 * time.Minute).Format(time.RFC3339)
		},
		"invalid":           func(d map[string]any) { d["invalid"] = true },
		"wrong description": func(d map[string]any) { d["description"] = "other-node" },
		"invalid key":       func(d map[string]any) { d["key"] = "private-token" },
		"timezone missing":  func(d map[string]any) { d["created"] = "2026-09-20T12:00:00" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f := &fakeAPI{keys: map[string]map[string]any{}, mutate: mutate}
			client := fixtureClient(f, func(_ context.Context, mode string, _ Target, _ map[string]any) error {
				if mode != "preflight" {
					t.Fatal("unsafe key delivered")
				}
				return nil
			})
			_, err := client.CreateDeliver(context.Background(), fixtureTarget())
			if err == nil || len(f.keys) != 0 || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe metadata: %v, %v", err, f.keys)
			}
		})
	}
}

func TestFailedDeliveryRevokesAndCleansWithUncancelledContext(t *testing.T) {
	f := &fakeAPI{keys: map[string]map[string]any{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var modes []string
	client := fixtureClient(f, func(ctx context.Context, mode string, target Target, _ map[string]any) error {
		modes = append(modes, mode)
		if mode == "deliver" {
			cancel()
			return errors.New("transport secret")
		}
		if mode == "cleanup" && (ctx.Err() != nil || target.KeyID != "kFixture123") {
			t.Fatal("cleanup lost identity or context")
		}
		return nil
	})
	_, err := client.CreateDeliver(ctx, fixtureTarget())
	if err == nil || len(f.keys) != 0 || fmt.Sprint(modes) != "[preflight deliver cleanup]" || strings.Contains(err.Error(), "transport secret") {
		t.Fatalf("cleanup failed: %v %v", modes, err)
	}
}

func TestLostCreationResponseRecoversOnlyThisRequest(t *testing.T) {
	f := &fakeAPI{keys: map[string]map[string]any{"kExisting": {"description": "unrelated"}}, failCreate: true}
	client := fixtureClient(f, func(context.Context, string, Target, map[string]any) error { return nil })
	_, err := client.CreateDeliver(context.Background(), fixtureTarget())
	if err == nil || len(f.keys) != 1 || f.keys["kExisting"] == nil || !strings.Contains(err.Error(), "created key revoked") {
		t.Fatalf("wrong recovery: %v %v", f.keys, err)
	}
}

func TestCleanupFailuresDoNotClaimSuccess(t *testing.T) {
	f := &fakeAPI{keys: map[string]map[string]any{}, failDelete: true}
	client := fixtureClient(f, func(_ context.Context, mode string, _ Target, _ map[string]any) error {
		if mode != "preflight" {
			return errors.New("secret")
		}
		return nil
	})
	_, err := client.CreateDeliver(context.Background(), fixtureTarget())
	if err == nil || !strings.Contains(err.Error(), "API revocation unconfirmed") || !strings.Contains(err.Error(), "remote cleanup unconfirmed") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("false cleanup claim: %v", err)
	}
}

func TestMetadataDriftAndPreflightFailurePreventDelivery(t *testing.T) {
	for _, preflightFail := range []bool{false, true} {
		f := &fakeAPI{keys: map[string]map[string]any{}, drift: true}
		client := fixtureClient(f, func(_ context.Context, mode string, _ Target, _ map[string]any) error {
			if mode != "preflight" {
				t.Fatal("delivery attempted")
			}
			if preflightFail {
				return errors.New("existing key")
			}
			return nil
		})
		_, err := client.CreateDeliver(context.Background(), fixtureTarget())
		if err == nil || len(f.keys) != 0 {
			t.Fatal("unsafe delivery")
		}
	}
}
