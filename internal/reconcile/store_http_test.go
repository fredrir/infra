package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/objectstore"
)

func TestNativeStateLockOwnership(t *testing.T) {
	for _, initial := range []string{"missing", "expired", "active"} {
		t.Run(initial, func(t *testing.T) {
			var mu sync.Mutex
			etag := ""
			var body []byte
			if initial != "missing" {
				expiry := time.Now().Add(time.Hour)
				if initial == "expired" {
					expiry = time.Now().Add(-time.Hour)
				}
				body, _ = json.Marshal(lease{Owner: "previous", Expires: expiry})
				etag = `"previous"`
			}
			writes, deletes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path != "/bucket/production/lock.json" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				if !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") || r.Header.Get("X-Amz-Security-Token") != "session" {
					t.Error("missing request authentication")
				}
				switch r.Method {
				case http.MethodGet:
					if etag == "" {
						w.WriteHeader(404)
						return
					}
					w.Header().Set("ETag", etag)
					w.Write(body)
				case http.MethodPut:
					if r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
						t.Error("missing encryption")
					}
					if (r.Header.Get("If-None-Match") == "*" && etag != "") || (r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != etag) {
						w.WriteHeader(412)
						return
					}
					if r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Match") == "" {
						t.Error("unconditional lease write")
					}
					body, _ = io.ReadAll(r.Body)
					etag = fmt.Sprintf(`"owner-%d"`, writes)
					writes++
					w.Header().Set("ETag", etag)
				case http.MethodDelete:
					if r.Header.Get("If-Match") != etag {
						w.WriteHeader(412)
						return
					}
					deletes++
					etag = ""
					w.WriteHeader(204)
				}
			}))
			defer server.Close()
			store := S3Store{Bucket: "bucket", Prefix: "production", Client: &objectstore.Client{Endpoint: server.URL, Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", SessionToken: "session", HTTP: server.Client()}}
			unlock, err := store.Lock(context.Background())
			if initial == "active" {
				if err == nil || writes != 0 || deletes != 0 {
					t.Fatal("active owner displaced")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Lock(context.Background()); err == nil {
				t.Fatal("second worker acquired occupied lock")
			}
			mu.Lock()
			etag = `"another-owner"`
			mu.Unlock()
			if err = unlock(); err == nil {
				t.Fatal("release deleted another owner's lease")
			}
			mu.Lock()
			etag = `"owner-0"`
			mu.Unlock()
			if err = unlock(); err != nil {
				t.Fatal(err)
			}
			if writes != 1 || deletes != 1 {
				t.Fatalf("writes=%d deletes=%d", writes, deletes)
			}
		})
	}
}

func TestNativeStateReadFailsClosed(t *testing.T) {
	for _, code := range []int{403, 500, 200} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				io.WriteString(w, "private-response")
			}))
			defer server.Close()
			store := S3Store{Bucket: "bucket", Prefix: "production", Client: &objectstore.Client{Endpoint: server.URL, Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", HTTP: server.Client()}}
			if _, err := store.Read(context.Background()); err == nil || strings.Contains(err.Error(), "private-response") {
				t.Fatalf("unsafe read result: %v", err)
			}
		})
	}
}

func TestVerificationDefersToHeldReconciliationLock(t *testing.T) {
	status, _ := json.Marshal(Status{Desired: "c", Applied: "b", Stage: "hosts"})
	recovery := []Difference{{System: "reconciliation", Item: "applied b, desired c, stage hosts"}}
	for _, test := range []struct {
		name         string
		lease        *lease
		wantCompared []string
		want         Verification
	}{
		{name: "held", lease: &lease{Owner: "local", Expires: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}, want: Verification{Outcome: OutcomeFailed, Differences: []Difference{}, Errors: []string{"comparisons skipped: reconciliation locked by local until 2099-01-01 00:00:00 +0000 UTC"}}},
		{name: "expired", lease: &lease{Owner: "orphaned", Expires: time.Now().Add(-time.Minute)}, wantCompared: []string{"deep c"}, want: Verification{Revision: "c", Outcome: OutcomeDiffers, Differences: recovery, Errors: []string{}}},
		{name: "absent", wantCompared: []string{"deep c"}, want: Verification{Revision: "c", Outcome: OutcomeDiffers, Differences: recovery, Errors: []string{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("verification sent %s %s", r.Method, r.URL.Path)
					w.WriteHeader(405)
					return
				}
				switch r.URL.Path {
				case "/bucket/production/status.json":
					w.Header().Set("ETag", `"status"`)
					w.Write(status)
				case "/bucket/production/lock.json":
					if test.lease == nil {
						w.WriteHeader(404)
						return
					}
					w.Header().Set("ETag", `"lease"`)
					json.NewEncoder(w).Encode(test.lease)
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			store := S3Store{Bucket: "bucket", Prefix: "production", Client: &objectstore.Client{Endpoint: server.URL, Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", HTTP: server.Client()}}
			ops := &verificationOps{revision: "c", published: "c"}
			verified, err := Verifier{Store: store, Ops: ops, Host: "logs.fredrir.com", Deep: true}.Verify(context.Background())
			if !reflect.DeepEqual(ops.compared, test.wantCompared) {
				t.Errorf("compared %q, want %q", ops.compared, test.wantCompared)
			}
			want := test.want
			want.Deep = true
			if got := VerificationOutcome(verified, true, err); !reflect.DeepEqual(got, want) {
				t.Fatalf("outcome %+v, want %+v", got, want)
			}
		})
	}
}
