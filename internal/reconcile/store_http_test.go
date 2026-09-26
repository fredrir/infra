package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/objectstore"
	"github.com/fredrir/infra/internal/process"
)

type leaseServer struct {
	t        *testing.T
	mu       sync.Mutex
	etag     string
	body     []byte
	versions int
	writes   int
	deletes  int
	failPuts int
	hang     chan struct{}
}

func (s *leaseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	hang := s.hang
	s.mu.Unlock()
	if hang != nil && r.Method == http.MethodPut {
		<-hang
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path != "/bucket/production/lock.json" {
		s.t.Errorf("unexpected path: %s", r.URL.Path)
	}
	if !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") || r.Header.Get("X-Amz-Security-Token") != "session" {
		s.t.Error("missing request authentication")
	}
	switch r.Method {
	case http.MethodGet:
		if s.etag == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", s.etag)
		w.Write(s.body)
	case http.MethodPut:
		if r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
			s.t.Error("missing encryption")
		}
		if r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Match") == "" {
			s.t.Error("unconditional lease write")
		}
		if s.failPuts != 0 {
			w.WriteHeader(s.failPuts)
			return
		}
		if (r.Header.Get("If-None-Match") == "*" && s.etag != "") || (r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != s.etag) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		s.store(io.ReadAll(r.Body))
		s.writes++
		w.Header().Set("ETag", s.etag)
	case http.MethodDelete:
		if r.Header.Get("If-Match") != s.etag {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		s.etag, s.body = "", nil
		s.deletes++
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *leaseServer) store(body []byte, err error) {
	if err != nil {
		s.t.Error(err)
	}
	s.versions++
	s.etag, s.body = fmt.Sprintf(`"version-%d"`, s.versions), body
}

func (s *leaseServer) set(held lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store(json.Marshal(held))
}

func (s *leaseServer) lease() lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	var held lease
	if s.body != nil {
		if err := json.Unmarshal(s.body, &held); err != nil {
			s.t.Error(err)
		}
	}
	return held
}

func (s *leaseServer) failWrites(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failPuts = code
}

func (s *leaseServer) hangWrites() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hang = make(chan struct{})
}

func (s *leaseServer) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hang != nil {
		close(s.hang)
		s.hang = nil
	}
}

func (s *leaseServer) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes, s.deletes
}

type handlerTransport struct{ http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		h.ServeHTTP(recorder, r)
	}()
	select {
	case <-served:
		return recorder.Result(), nil
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

func leaseStore(server *leaseServer) S3Store {
	return S3Store{Bucket: "bucket", Prefix: "production", Client: &objectstore.Client{Endpoint: "http://s3.test", Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", SessionToken: "session", HTTP: &http.Client{Transport: handlerTransport{server}}}}
}

func TestNativeStateLockOwnership(t *testing.T) {
	for _, initial := range []string{"missing", "expired", "active"} {
		t.Run(initial, func(t *testing.T) {
			server := &leaseServer{t: t}
			switch initial {
			case "expired":
				server.set(lease{Owner: "previous", Expires: time.Now().Add(-time.Hour)})
			case "active":
				server.set(lease{Owner: "previous", Expires: time.Now().Add(time.Hour)})
			}
			store := leaseStore(server)
			held, unlock, err := store.Lock(context.Background())
			if initial == "active" {
				var locked ErrLocked
				if writes, deletes := server.counts(); !errors.As(err, &locked) || locked.Owner != "previous" || writes != 0 || deletes != 0 {
					t.Fatalf("active owner displaced: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if owner := server.lease(); !strings.Contains(owner.Owner, "pid") || owner.Expires.After(time.Now().Add(leaseTTL)) {
				t.Fatalf("lease %+v exceeds its TTL", owner)
			}
			if _, _, err = store.Lock(context.Background()); !errors.As(err, new(ErrLocked)) {
				t.Fatalf("second worker acquired occupied lock: %v", err)
			}
			if err = unlock(); err != nil {
				t.Fatal(err)
			}
			if held.Err() == nil {
				t.Fatal("released lease context still active")
			}
			if writes, deletes := server.counts(); writes != 1 || deletes != 1 {
				t.Fatalf("writes=%d deletes=%d", writes, deletes)
			}
		})
	}
}

func TestLeaseRenewsUntilReleased(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &leaseServer{t: t}
		store := leaseStore(server)
		held, unlock, err := store.Lock(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(3*leaseTTL + time.Second)
		if held.Err() != nil {
			t.Fatalf("renewed lease lost: %v", context.Cause(held))
		}
		if expires := server.lease().Expires; !expires.After(time.Now()) {
			t.Fatalf("lease expired at %s while held", expires)
		}
		if _, _, err := store.Lock(t.Context()); !errors.As(err, new(ErrLocked)) {
			t.Fatalf("renewed lease taken over: %v", err)
		}
		if err := unlock(); err != nil {
			t.Fatal(err)
		}
		writes, deletes := server.counts()
		if want := 1 + int(3*leaseTTL/leaseRenewal); writes != want || deletes != 1 {
			t.Fatalf("writes=%d deletes=%d, want %d renewals and one release", writes, deletes, want)
		}
		time.Sleep(leaseTTL)
		if after, _ := server.counts(); after != writes {
			t.Fatal("released lease kept renewing")
		}
	})
}

func TestAbandonedLeaseIsTakenOverAfterItsTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &leaseServer{t: t}
		server.set(lease{Owner: "crashed", Expires: time.Now().Add(leaseTTL)})
		store := leaseStore(server)
		if _, _, err := store.Lock(t.Context()); !errors.As(err, new(ErrLocked)) {
			t.Fatalf("unexpired lease taken over: %v", err)
		}
		time.Sleep(leaseTTL)
		_, unlock, err := store.Lock(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := unlock(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLostLeaseCancelsTheRun(t *testing.T) {
	for _, test := range []struct {
		name     string
		lose     func(*leaseServer)
		deadline time.Duration
	}{
		{name: "taken over", lose: func(s *leaseServer) { s.set(lease{Owner: "thief", Expires: time.Now().Add(time.Hour)}) }, deadline: leaseRenewal},
		{name: "renewals fail", lose: func(s *leaseServer) { s.failWrites(http.StatusServiceUnavailable) }, deadline: leaseTTL},
		{name: "renewals denied", lose: func(s *leaseServer) { s.failWrites(http.StatusForbidden) }, deadline: leaseTTL},
		{name: "renewals hang", lose: func(s *leaseServer) { s.hangWrites() }, deadline: leaseTTL},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server := &leaseServer{t: t}
				held, unlock, err := leaseStore(server).Lock(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				acquired := time.Now()
				test.lose(server)
				select {
				case <-held.Done():
				case <-time.After(leaseTTL):
				}
				if lost := time.Since(acquired); held.Err() == nil || lost > test.deadline || lost >= leaseTTL {
					t.Errorf("run continued %s after losing its lease", lost)
				}
				if cause := context.Cause(held); !errors.Is(cause, errLeaseLost) {
					t.Errorf("cancellation cause %v", cause)
				}
				released := unlock()
				if owner := server.lease().Owner; test.name == "taken over" && (released == nil || owner != "thief") {
					t.Errorf("release of a taken-over lease returned %v and left owner %q", released, owner)
				}
				server.resume()
			})
		})
	}
}

func TestReleaseOutwaitsOnlyOneBoundedRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &leaseServer{t: t}
		_, unlock, err := leaseStore(server).Lock(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		server.hangWrites()
		time.Sleep(leaseRenewal + time.Second)
		started := time.Now()
		if err := unlock(); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(started); waited >= renewalTimeout {
			t.Errorf("release waited %s for a hung renewal", waited)
		}
		server.resume()
	})
}

func TestCLILeaseRenewalDetectsTakeover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		puts := 0
		runner := ci.Runner{Execute: func(_ context.Context, o process.Options) (process.Result, error) {
			switch o.Args[1] {
			case "get-object":
				return process.Result{ExitCode: 254}, errors.New("missing")
			case "list-objects-v2":
				return process.Result{Stdout: []byte(`{"Contents":[]}`)}, nil
			case "put-object":
				if puts++; puts > 1 {
					return process.Result{ExitCode: 254, Stderr: []byte("\nAn error occurred (PreconditionFailed) when calling the PutObject operation: At least one of the pre-conditions you specified did not hold\n")}, errors.New("aws failed: exit status 254")
				}
				return process.Result{Stdout: []byte(`{"ETag":"owner"}`)}, nil
			}
			return process.Result{}, nil
		}}
		held, unlock, err := (S3Store{Runner: runner, Bucket: "bucket", Prefix: "production"}).Lock(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		acquired := time.Now()
		<-held.Done()
		if lost := time.Since(acquired); lost != leaseRenewal || !errors.Is(context.Cause(held), errLeaseLost) {
			t.Errorf("taken-over lease lost after %s: %v", lost, context.Cause(held))
		}
		unlock()
	})
}

func TestTransientRenewalFailureKeepsTheLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &leaseServer{t: t}
		held, unlock, err := leaseStore(server).Lock(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		server.failWrites(http.StatusServiceUnavailable)
		time.Sleep(leaseRenewal + time.Second)
		server.failWrites(0)
		time.Sleep(leaseTTL)
		if held.Err() != nil {
			t.Fatalf("transient renewal failure cancelled the run: %v", context.Cause(held))
		}
		if err := unlock(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLockWaitAcquiresAfterRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &leaseServer{t: t}
		store := leaseStore(server)
		_, release, err := store.Lock(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var log strings.Builder
		start := time.Now()
		if _, _, err := (Reconciler{Store: store}).lock(t.Context()); !errors.As(err, new(ErrLocked)) || time.Since(start) != 0 {
			t.Fatalf("lock without wait did not fail fast: %v", err)
		}
		if _, _, err := (Reconciler{Store: store, LockWait: time.Minute}).lock(t.Context()); !errors.As(err, new(ErrLocked)) || time.Since(start) != time.Minute {
			t.Fatalf("bounded wait gave up after %s: %v", time.Since(start), err)
		}
		released := make(chan time.Time)
		go func() {
			time.Sleep(20 * time.Minute)
			if err := release(); err != nil {
				t.Error(err)
			}
			released <- time.Now()
		}()
		_, unlock, err := (Reconciler{Store: store, LockWait: time.Hour, Log: &log}).lock(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(<-released); waited > lockPoll {
			t.Errorf("acquired %s after release", waited)
		}
		if !strings.Contains(log.String(), "reconciliation locked by") {
			t.Errorf("wait not reported: %q", log.String())
		}
		if err := unlock(); err != nil {
			t.Fatal(err)
		}
	})
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
		lease, taken *lease
		wantCompared []string
		want         Verification
	}{
		{name: "held", lease: &lease{Owner: "local", Expires: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}, want: Verification{Outcome: OutcomeFailed, Differences: []Difference{}, Errors: []string{"comparisons skipped: reconciliation locked by local until 2099-01-01 00:00:00 +0000 UTC"}}},
		{name: "expired", lease: &lease{Owner: "orphaned", Expires: time.Now().Add(-time.Minute)}, wantCompared: []string{"full c"}, want: Verification{Revision: "c", Outcome: OutcomeDiffers, Differences: recovery, Errors: []string{}}},
		{name: "absent", wantCompared: []string{"full c"}, want: Verification{Revision: "c", Outcome: OutcomeDiffers, Differences: recovery, Errors: []string{}}},
		{name: "taken during comparison", taken: &lease{Owner: "local", Expires: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}, wantCompared: []string{"full c"}, want: Verification{Outcome: OutcomeFailed, Differences: []Difference{}, Errors: []string{"comparisons discarded: reconciliation locked by local until 2099-01-01 00:00:00 +0000 UTC"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lockReads := 0
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
					lockReads++
					held := test.lease
					if lockReads > 1 && test.taken != nil {
						held = test.taken
					}
					if held == nil {
						w.WriteHeader(404)
						return
					}
					w.Header().Set("ETag", `"lease"`)
					json.NewEncoder(w).Encode(held)
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			store := S3Store{Bucket: "bucket", Prefix: "production", Client: &objectstore.Client{Endpoint: server.URL, Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", HTTP: server.Client()}}
			ops := &verificationOps{revision: "c", published: "c"}
			verified, err := Verifier{Store: store, Ops: ops, Host: "logs.fredrir.com", Scope: ScopeFull}.Verify(context.Background())
			if !reflect.DeepEqual(ops.compared, test.wantCompared) {
				t.Errorf("compared %q, want %q", ops.compared, test.wantCompared)
			}
			want := test.want
			want.Scope = ScopeFull
			if got := VerificationOutcome(verified, ScopeFull, err); !reflect.DeepEqual(got, want) {
				t.Fatalf("outcome %+v, want %+v", got, want)
			}
		})
	}
}
