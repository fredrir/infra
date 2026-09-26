package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/objectstore"
	"github.com/fredrir/infra/internal/process"
)

type leaseFault struct {
	status  int
	applied bool
	winner  *lease
}

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
	faults   []leaseFault
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
	if r.Method == http.MethodGet {
		if s.etag == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", s.etag)
		w.Write(s.body)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Error(err)
	}
	if r.Method == http.MethodPut && r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		s.t.Error("missing encryption")
	}
	if r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Match") == "" {
		s.t.Errorf("unconditional lease %s", r.Method)
	}
	var fault *leaseFault
	if len(s.faults) > 0 {
		fault, s.faults = &s.faults[0], s.faults[1:]
		if fault.winner != nil {
			s.store(json.Marshal(*fault.winner))
		}
		if !fault.applied {
			s.fail(w, *fault)
			return
		}
	}
	status := s.apply(r, body)
	if fault != nil {
		s.fail(w, *fault)
		return
	}
	w.Header().Set("ETag", s.etag)
	w.WriteHeader(status)
}

func (s *leaseServer) apply(r *http.Request, body []byte) int {
	switch {
	case r.Method == http.MethodPut && s.failPuts != 0:
		return s.failPuts
	case r.Header.Get("If-None-Match") == "*" && s.etag != "", r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != s.etag:
		return http.StatusPreconditionFailed
	case r.Method == http.MethodPut:
		s.store(body, nil)
		s.writes++
		return http.StatusOK
	}
	s.etag, s.body = "", nil
	s.deletes++
	return http.StatusNoContent
}

func (s *leaseServer) fail(w http.ResponseWriter, fault leaseFault) {
	if fault.status != 0 {
		w.WriteHeader(fault.status)
		return
	}
	connection, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		s.t.Errorf("reset needs a network connection: %v", err)
		return
	}
	connection.(*net.TCPConn).SetLinger(0)
	connection.Close()
}

func (s *leaseServer) inject(faults ...leaseFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, faults...)
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

var immediately = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })

type handlerTransport struct{ http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer r.Body.Close()
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
	return S3Store{Bucket: "bucket", Prefix: "production", Client: &objectstore.Client{Endpoint: "http://s3.test", Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", SessionToken: "session", HTTP: &http.Client{Transport: handlerTransport{server}}, Backoff: immediately}}
}

func networkLeaseStore(t *testing.T, server *leaseServer) S3Store {
	t.Helper()
	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)
	return S3Store{Bucket: "bucket", Prefix: "production", Client: &objectstore.Client{Endpoint: listener.URL, Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", SessionToken: "session", HTTP: listener.Client(), Backoff: immediately}}
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
		{name: "taken over during a renewal", lose: func(s *leaseServer) {
			s.inject(leaseFault{status: http.StatusConflict, winner: &lease{Owner: "thief", Expires: time.Now().Add(time.Hour)}})
		}, deadline: leaseRenewal},
		{name: "renewals keep conflicting", lose: func(s *leaseServer) {
			s.inject(leaseFault{status: http.StatusConflict}, leaseFault{status: http.StatusConflict})
		}, deadline: leaseRenewal},
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
				if owner := server.lease().Owner; strings.HasPrefix(test.name, "taken over") && (released == nil || owner != "thief") {
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

func awsFailure(code, operation string) (process.Result, error) {
	if code == "" {
		return process.Result{ExitCode: 255, Stderr: []byte("\nConnection was closed before we received a valid response from endpoint URL\n")}, errors.New("aws failed: exit status 255")
	}
	return process.Result{ExitCode: 254, Stderr: []byte("\nAn error occurred (" + code + ") when calling the " + operation + " operation: rejected\n")}, errors.New("aws failed: exit status 254")
}

type cliFault struct {
	code    string
	applied bool
	winner  *lease
}

type fakeAWS struct {
	t        *testing.T
	mu       sync.Mutex
	body     []byte
	etag     string
	versions int
	calls    map[string]int
	faults   map[string][]cliFault
}

func newFakeAWS(t *testing.T) *fakeAWS {
	return &fakeAWS{t: t, calls: map[string]int{}, faults: map[string][]cliFault{}}
}

func (a *fakeAWS) store() S3Store {
	return S3Store{Runner: ci.Runner{Execute: a.execute}, Bucket: "bucket", Prefix: "production"}
}

func (a *fakeAWS) write(body []byte) {
	a.versions++
	a.body, a.etag = body, fmt.Sprintf(`"cli-%d"`, a.versions)
}

func (a *fakeAWS) take(winner lease) {
	a.mu.Lock()
	defer a.mu.Unlock()
	body, err := json.Marshal(winner)
	if err != nil {
		a.t.Fatal(err)
	}
	a.write(body)
}

func (a *fakeAWS) inject(operation string, faults ...cliFault) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.faults[operation] = append(a.faults[operation], faults...)
}

func (a *fakeAWS) count(operation string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[operation]
}

func (a *fakeAWS) execute(_ context.Context, o process.Options) (process.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	operation := o.Args[1]
	a.calls[operation]++
	flag := func(name string) string {
		if index := slices.Index(o.Args, name); index >= 0 {
			return o.Args[index+1]
		}
		return ""
	}
	switch operation {
	case "get-object":
		if a.etag == "" {
			return awsFailure("NoSuchKey", "GetObject")
		}
		if err := os.WriteFile(o.Args[6], a.body, 0o600); err != nil {
			a.t.Fatal(err)
		}
		return process.Result{Stdout: fmt.Appendf(nil, `{"ETag":%q}`, a.etag)}, nil
	case "list-objects-v2":
		if a.etag == "" {
			return process.Result{Stdout: []byte(`{"Contents":[]}`)}, nil
		}
		return process.Result{Stdout: []byte(`{"Contents":[{"Key":"production/lock.json"}]}`)}, nil
	}
	var fault *cliFault
	if queued := a.faults[operation]; len(queued) > 0 {
		fault, a.faults[operation] = &queued[0], queued[1:]
		if fault.winner != nil {
			body, _ := json.Marshal(*fault.winner)
			a.write(body)
		}
		if !fault.applied {
			return awsFailure(fault.code, operation)
		}
	}
	match, absent := flag("--if-match"), flag("--if-none-match") == "*"
	if match == "" && !absent {
		a.t.Errorf("unconditional lease %s", operation)
	}
	if absent && a.etag != "" || match != "" && match != a.etag {
		return awsFailure("PreconditionFailed", operation)
	}
	if operation == "delete-object" {
		a.body, a.etag = nil, ""
	} else {
		body, err := os.ReadFile(flag("--body"))
		if err != nil {
			a.t.Fatal(err)
		}
		a.write(body)
	}
	if fault != nil {
		return awsFailure(fault.code, operation)
	}
	return process.Result{Stdout: fmt.Appendf(nil, `{"ETag":%q}`, a.etag)}, nil
}

func TestCLILeaseRenewalDetectsTakeover(t *testing.T) {
	thief := lease{Owner: "thief", Expires: time.Now().Add(time.Hour)}
	for _, test := range []struct {
		name string
		lose func(*fakeAWS)
	}{
		{name: "precondition failed", lose: func(a *fakeAWS) { a.take(thief) }},
		{name: "conflict", lose: func(a *fakeAWS) { a.inject("put-object", cliFault{code: "ConditionalRequestConflict", winner: &thief}) }},
		{name: "unconfirmed", lose: func(a *fakeAWS) { a.inject("put-object", cliFault{winner: &thief}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				aws := newFakeAWS(t)
				held, unlock, err := aws.store().Lock(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				acquired := time.Now()
				test.lose(aws)
				<-held.Done()
				if lost := time.Since(acquired); lost != leaseRenewal || !errors.Is(context.Cause(held), errLeaseLost) {
					t.Errorf("taken-over lease lost after %s: %v", lost, context.Cause(held))
				}
				if err := unlock(); err == nil || !strings.Contains(string(aws.body), "thief") {
					t.Errorf("release of a taken-over lease returned %v and left %s", err, aws.body)
				}
			})
		})
	}
}

func TestCLILeaseSettlesRefusedAndUnconfirmedRequests(t *testing.T) {
	for _, test := range []struct {
		name          string
		operation     string
		fault         cliFault
		puts, deletes int
	}{
		{name: "acquisition conflicted", operation: "put-object", fault: cliFault{code: "ConditionalRequestConflict"}, puts: 2, deletes: 1},
		{name: "acquisition lost its response", operation: "put-object", fault: cliFault{applied: true}, puts: 1, deletes: 1},
		{name: "acquisition refused after a retry of an applied write", operation: "put-object", fault: cliFault{applied: true, code: "PreconditionFailed"}, puts: 1, deletes: 1},
		{name: "acquisition failed before it was sent", operation: "put-object", fault: cliFault{}, puts: 2, deletes: 1},
		{name: "release conflicted", operation: "delete-object", fault: cliFault{code: "ConditionalRequestConflict"}, puts: 1, deletes: 2},
		{name: "release lost its response", operation: "delete-object", fault: cliFault{applied: true}, puts: 1, deletes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			aws := newFakeAWS(t)
			aws.inject(test.operation, test.fault)
			_, unlock, err := aws.store().Lock(t.Context())
			if err != nil {
				t.Fatalf("acquisition: %v", err)
			}
			if err := unlock(); err != nil {
				t.Fatalf("release: %v", err)
			}
			if puts, deletes := aws.count("put-object"), aws.count("delete-object"); puts != test.puts || deletes != test.deletes || aws.etag != "" {
				t.Fatalf("%d puts and %d deletes left %q", puts, deletes, aws.body)
			}
		})
	}
}

func TestUnconfirmedLeaseRequestsAreSettledByReadingTheLease(t *testing.T) {
	thief := &lease{Owner: "thief", Expires: time.Now().Add(time.Hour)}
	for _, test := range []struct {
		name            string
		acquire, free   []leaseFault
		wantLocked      bool
		wantTaken       bool
		writes, deletes int
	}{
		{name: "acquisition applied before a reset", acquire: []leaseFault{{applied: true}}, writes: 1, deletes: 1},
		{name: "acquisition reset before it applied", acquire: []leaseFault{{}}, writes: 1, deletes: 1},
		{name: "acquisition reset while another writer took the lease", acquire: []leaseFault{{winner: thief}}, wantLocked: true},
		{name: "acquisition answered 500 after it applied", acquire: []leaseFault{{status: http.StatusInternalServerError, applied: true}}, writes: 1, deletes: 1},
		{name: "release applied before a reset", free: []leaseFault{{applied: true}}, writes: 1, deletes: 1},
		{name: "release reset before it applied", free: []leaseFault{{}}, writes: 1, deletes: 1},
		{name: "release reset while another writer took the lease", free: []leaseFault{{winner: thief}}, wantTaken: true, writes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &leaseServer{t: t}
			store := networkLeaseStore(t, server)
			server.inject(test.acquire...)
			_, unlock, err := store.Lock(t.Context())
			var locked ErrLocked
			if test.wantLocked {
				if !errors.As(err, &locked) || locked.Owner != "thief" {
					t.Fatalf("acquisition returned %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("acquisition: %v", err)
			}
			server.inject(test.free...)
			if err := unlock(); (err != nil) != test.wantTaken || test.wantTaken && !leaseTaken(err) {
				t.Fatalf("release returned %v", err)
			}
			if writes, deletes := server.counts(); writes != test.writes || deletes != test.deletes {
				t.Fatalf("%d writes and %d deletes", writes, deletes)
			}
		})
	}
}

func TestUnconfirmedRenewalAdoptsOnlyItsOwnLease(t *testing.T) {
	thief := &lease{Owner: "thief", Expires: time.Now().Add(time.Hour)}
	for _, test := range []struct {
		name      string
		faults    []leaseFault
		wantErr   bool
		wantTaken bool
		renewed   bool
	}{
		{name: "applied before a reset", faults: []leaseFault{{applied: true}}, renewed: true},
		{name: "reset before it applied", faults: []leaseFault{{}}, renewed: true},
		{name: "reset twice before it applied", faults: []leaseFault{{}, {}}, wantErr: true},
		{name: "reset while another writer took the lease", faults: []leaseFault{{winner: thief}}, wantErr: true, wantTaken: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &leaseServer{t: t}
			store := networkLeaseStore(t, server)
			held := &heldLease{store: store, owner: leaseOwner(), expires: time.Now().Add(leaseTTL)}
			var err error
			if held.token, err = store.putLease(t.Context(), lease{held.owner, held.expires}, "*"); err != nil {
				t.Fatal(err)
			}
			acquired := held.token
			server.inject(test.faults...)
			err = held.extend(t.Context())
			if (err != nil) != test.wantErr || leaseTaken(err) != test.wantTaken {
				t.Fatalf("renewal returned %v", err)
			}
			if renewed := held.token != acquired; renewed != test.renewed || held.token != server.etag && !test.wantTaken || server.lease().Expires.Equal(held.expires) != !test.wantTaken {
				t.Fatalf("renewal kept token %s against %s and expiry %s against %s", held.token, server.etag, held.expires, server.lease().Expires)
			}
			if !test.wantTaken {
				if err := held.extend(t.Context()); err != nil {
					t.Fatalf("renewal after the settled one: %v", err)
				}
			}
		})
	}
}

func TestConflictedLeaseRequestsAreReissuedOnce(t *testing.T) {
	thief := &lease{Owner: "thief", Expires: time.Now().Add(time.Hour)}
	for _, test := range []struct {
		name      string
		existing  *lease
		winner    *lease
		wantOwner string
	}{
		{name: "free lease", wantOwner: "pid"},
		{name: "expired lease", existing: &lease{Owner: "crashed", Expires: time.Now().Add(-time.Hour)}, wantOwner: "pid"},
		{name: "free lease won by another writer", winner: thief, wantOwner: "thief"},
		{name: "expired lease won by another writer", existing: &lease{Owner: "crashed", Expires: time.Now().Add(-time.Hour)}, winner: thief, wantOwner: "thief"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &leaseServer{t: t}
			if test.existing != nil {
				server.set(*test.existing)
			}
			server.inject(leaseFault{status: http.StatusConflict, winner: test.winner})
			_, unlock, err := leaseStore(server).Lock(t.Context())
			if owner := server.lease().Owner; !strings.Contains(owner, test.wantOwner) {
				t.Fatalf("lease owned by %q after a conflicted acquisition returned %v", owner, err)
			}
			if test.winner != nil {
				var locked ErrLocked
				if !errors.As(err, &locked) || locked.Owner != "thief" {
					t.Fatalf("acquisition lost to another writer returned %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("acquisition after a conflict: %v", err)
			}
			server.inject(leaseFault{status: http.StatusConflict})
			if err := unlock(); err != nil {
				t.Fatalf("release after a conflict: %v", err)
			}
			if _, deletes := server.counts(); deletes != 1 || server.lease().Owner != "" {
				t.Fatalf("release after a conflict deleted %d times and left %+v", deletes, server.lease())
			}
		})
	}
}

func TestReleaseConflictedByATakeoverKeepsTheNewOwner(t *testing.T) {
	server := &leaseServer{t: t}
	_, unlock, err := leaseStore(server).Lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	server.inject(leaseFault{status: http.StatusConflict, winner: &lease{Owner: "thief", Expires: time.Now().Add(time.Hour)}})
	if err := unlock(); err == nil || !leaseTaken(err) {
		t.Fatalf("release of a taken-over lease returned %v", err)
	}
	if _, deletes := server.counts(); deletes != 0 || server.lease().Owner != "thief" {
		t.Fatalf("release deleted %d times and left %+v", deletes, server.lease())
	}
}

func TestRenewalConflictKeepsAnOwnedLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := &leaseServer{t: t}
		held, unlock, err := leaseStore(server).Lock(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		acquired := time.Now()
		server.inject(leaseFault{status: http.StatusConflict})
		time.Sleep(leaseRenewal + time.Second)
		if held.Err() != nil {
			t.Fatalf("conflicted renewal cancelled the run: %v", context.Cause(held))
		}
		if expires := server.lease().Expires; !expires.After(acquired.Add(leaseTTL)) {
			t.Fatalf("conflicted renewal was not reissued; lease still expires at %s", expires)
		}
		if err := unlock(); err != nil {
			t.Fatal(err)
		}
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
			store := S3Store{Bucket: "bucket", Prefix: "production", Client: &objectstore.Client{Endpoint: server.URL, Region: "eu-north-1", AccessKey: "access", SecretKey: "secret", HTTP: server.Client(), Backoff: immediately}}
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
