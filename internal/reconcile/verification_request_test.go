package reconcile

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

type runState struct {
	status, conclusion string
	unchanged          bool
	limited            bool
}

type githubAppServer struct {
	t        *testing.T
	key      *rsa.PrivateKey
	token    int
	dispatch int
	runID    int64
	runs     []runState
	mu       sync.Mutex
	calls    []string
	bodies   map[string]any
	etags    []string
}

const runURL = "https://github.com/fredrir/infra/actions/runs/7"

func (s *githubAppServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	if r.Method == http.MethodPost {
		var body any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Errorf("%s body: %v", r.URL.Path, err)
		}
		s.bodies[r.URL.Path] = body
	}
	switch r.URL.Path {
	case "/app/installations/43/access_tokens":
		claims := jwt.RegisteredClaims{}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, err := jwt.ParseWithClaims(bearer, &claims, func(*jwt.Token) (any, error) { return &s.key.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"})); err != nil || claims.Issuer != "42" {
			s.t.Errorf("token exchange authenticated as %q: %v", claims.Issuer, err)
		}
		w.WriteHeader(s.token)
		json.NewEncoder(w).Encode(map[string]any{"token": "installation-token", "expires_at": time.Now().Add(time.Hour)})
	case "/repos/fredrir/infra/actions/workflows/reconcile.yml/dispatches":
		s.authorized(r)
		w.WriteHeader(s.dispatch)
		switch {
		case s.dispatch == http.StatusOK:
			json.NewEncoder(w).Encode(map[string]any{"workflow_run_id": s.runID, "html_url": runURL})
		case s.dispatch >= http.StatusBadRequest:
			json.NewEncoder(w).Encode(map[string]any{"message": "Unexpected inputs provided"})
		}
	case "/repos/fredrir/infra/actions/runs/7":
		s.authorized(r)
		s.etags = append(s.etags, r.Header.Get("If-None-Match"))
		polls := len(s.etags)
		state := s.runs[min(polls, len(s.runs))-1]
		switch {
		case state.limited:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"message": "API rate limit exceeded"})
		case state.unchanged:
			w.WriteHeader(http.StatusNotModified)
		default:
			w.Header().Set("ETag", `"`+strconv.Itoa(polls)+`"`)
			json.NewEncoder(w).Encode(map[string]any{"status": state.status, "conclusion": state.conclusion, "html_url": runURL})
		}
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *githubAppServer) authorized(r *http.Request) {
	if authorization := r.Header.Get("Authorization"); authorization != "token installation-token" {
		s.t.Errorf("%s authorized with %q", r.URL.Path, authorization)
	}
}

type gatusServer struct {
	mu         sync.Mutex
	heartbeats []url.Values
}

func (g *gatusServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/endpoints/reconciliation_verification/external" || r.Header.Get("Authorization") != "Bearer "+heartbeatToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	g.heartbeats = append(g.heartbeats, r.URL.Query())
}

var heartbeatToken = strings.Repeat("verification-token", 3)

func githubAppKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func verificationFixture(t *testing.T, runs ...runState) (*githubAppServer, *gatusServer, VerificationRequest) {
	t.Helper()
	key, encoded := githubAppKey(t)
	github := &githubAppServer{t: t, key: key, token: http.StatusCreated, dispatch: http.StatusOK, runID: 7, runs: runs, bodies: map[string]any{}}
	gatus := &gatusServer{}
	githubServer, gatusServer := httptest.NewServer(github), httptest.NewServer(gatus)
	t.Cleanup(githubServer.Close)
	t.Cleanup(gatusServer.Close)
	return github, gatus, VerificationRequest{AppID: 42, InstallationID: 43, PrivateKey: encoded, Repository: "fredrir/infra", Workflow: "reconcile.yml", Ref: "main", API: githubServer.URL, Gatus: gatusServer.URL, HeartbeatToken: heartbeatToken, Poll: time.Millisecond, Deadline: time.Minute}
}

func TestVerificationRequestDispatchesWithARestrictedInstallationToken(t *testing.T) {
	github, gatus, request := verificationFixture(t, runState{status: "queued"}, runState{unchanged: true}, runState{status: "in_progress"}, runState{status: "completed", conclusion: "success"})
	var log bytes.Buffer
	request.Log = &log
	if err := RequestVerification(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	poll := "GET /repos/fredrir/infra/actions/runs/7"
	if want := []string{"POST /app/installations/43/access_tokens", "POST /repos/fredrir/infra/actions/workflows/reconcile.yml/dispatches", poll, poll, poll, poll}; !reflect.DeepEqual(github.calls, want) {
		t.Fatalf("requests %v, want %v", github.calls, want)
	}
	restriction := map[string]any{"repositories": []any{"infra"}, "permissions": map[string]any{"actions": "write"}}
	if got := github.bodies["/app/installations/43/access_tokens"]; !reflect.DeepEqual(got, restriction) {
		t.Errorf("installation token requested %v, want %v", got, restriction)
	}
	dispatch := map[string]any{"ref": "main", "inputs": map[string]any{"verify": "true", "repair": "true"}, "return_run_details": true}
	if got := github.bodies["/repos/fredrir/infra/actions/workflows/reconcile.yml/dispatches"]; !reflect.DeepEqual(got, dispatch) {
		t.Errorf("dispatched %v, want %v", got, dispatch)
	}
	if want := []string{"", `"1"`, `"1"`, `"3"`}; !reflect.DeepEqual(github.etags, want) {
		t.Errorf("polls revalidated %q, want %q", github.etags, want)
	}
	if want := []url.Values{{"success": {"true"}}}; !reflect.DeepEqual(gatus.heartbeats, want) {
		t.Errorf("heartbeats %v, want %v", gatus.heartbeats, want)
	}
	if !strings.Contains(log.String(), "Requested verification: "+runURL) || !strings.Contains(log.String(), "Verification succeeded: "+runURL) {
		t.Errorf("log %q", log.String())
	}
}

func TestVerificationOutcomeReachesTheHeartbeat(t *testing.T) {
	for _, test := range []struct {
		name     string
		runs     []runState
		deadline time.Duration
		failure  string
	}{
		{name: "differences or errors", runs: []runState{{status: "completed", conclusion: "failure"}}, failure: runURL + " concluded failure"},
		{name: "timed out", runs: []runState{{status: "completed", conclusion: "timed_out"}}, failure: runURL + " concluded timed_out"},
		{name: "failed to start", runs: []runState{{status: "completed", conclusion: "startup_failure"}}, failure: runURL + " concluded startup_failure"},
		{name: "rate limited", runs: []runState{{limited: true}, {status: "completed", conclusion: "success"}}},
		{name: "never completes", runs: []runState{{status: "queued"}}, deadline: 50 * time.Millisecond, failure: runURL + " did not complete within 50ms: context deadline exceeded"},
		{name: "unreadable until the deadline", runs: []runState{{limited: true}}, deadline: 50 * time.Millisecond, failure: runURL + " did not complete within 50ms: last poll: 403 Forbidden API rate limit exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, gatus, request := verificationFixture(t, test.runs...)
			if test.deadline > 0 {
				request.Deadline = test.deadline
			}
			err := RequestVerification(context.Background(), request)
			want := url.Values{"success": {"true"}}
			if test.failure != "" {
				want = url.Values{"success": {"false"}, "error": {test.failure}}
			}
			if !reflect.DeepEqual(gatus.heartbeats, []url.Values{want}) {
				t.Fatalf("heartbeats %v, want %v", gatus.heartbeats, want)
			}
			if (err == nil) != (test.failure == "") || (err != nil && err.Error() != test.failure) {
				t.Fatalf("request returned %v, want %q", err, test.failure)
			}
		})
	}
}

func TestSupersededOrInterruptedVerificationSendsNoHeartbeat(t *testing.T) {
	_, gatus, request := verificationFixture(t, runState{status: "completed", conclusion: "cancelled"})
	var log bytes.Buffer
	request.Log = &log
	if err := RequestVerification(context.Background(), request); err != nil || len(gatus.heartbeats) != 0 || !strings.Contains(log.String(), "Verification cancelled: "+runURL) {
		t.Fatalf("cancelled run returned %v with heartbeats %v and log %q", err, gatus.heartbeats, log.String())
	}
	_, gatus, request = verificationFixture(t, runState{status: "in_progress"})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := RequestVerification(ctx, request); err == nil || len(gatus.heartbeats) != 0 {
		t.Fatalf("interrupted wait returned %v with heartbeats %v", err, gatus.heartbeats)
	}
}

func TestRejectedHeartbeatFailsTheRequest(t *testing.T) {
	_, _, request := verificationFixture(t, runState{status: "completed", conclusion: "success"})
	request.HeartbeatToken = strings.Repeat("rejected-token", 3)
	if err := RequestVerification(context.Background(), request); err == nil || err.Error() != "heartbeat HTTP 401" {
		t.Fatalf("rejected heartbeat returned %v", err)
	}
}

func TestVerificationRequestFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*githubAppServer, *VerificationRequest)
		calls  int
		want   string
	}{
		{name: "token exchange rejected", change: func(github *githubAppServer, _ *VerificationRequest) { github.token = http.StatusUnauthorized }, calls: 1, want: "could not refresh installation id 43's token"},
		{name: "dispatch rejected", change: func(github *githubAppServer, _ *VerificationRequest) {
			github.dispatch = http.StatusUnprocessableEntity
		}, calls: 2, want: "dispatch reconcile.yml in fredrir/infra at main: 422 Unprocessable Entity Unexpected inputs provided"},
		{name: "dispatch without run details", change: func(github *githubAppServer, _ *VerificationRequest) { github.dispatch = http.StatusNoContent }, calls: 2, want: "dispatch reconcile.yml in fredrir/infra at main: response identifies no workflow run"},
		{name: "invalid key", change: func(_ *githubAppServer, request *VerificationRequest) { request.PrivateKey = []byte("not a key") }, want: "GitHub App private key"},
		{name: "missing app", change: func(_ *githubAppServer, request *VerificationRequest) { request.AppID = 0 }, want: "GitHub App and installation IDs are required"},
		{name: "missing installation", change: func(_ *githubAppServer, request *VerificationRequest) { request.InstallationID = 0 }, want: "GitHub App and installation IDs are required"},
		{name: "repository without owner", change: func(_ *githubAppServer, request *VerificationRequest) { request.Repository = "infra" }, want: `repository "infra" is not OWNER/NAME`},
		{name: "nested repository", change: func(_ *githubAppServer, request *VerificationRequest) { request.Repository = "fredrir/infra/extra" }, want: "is not OWNER/NAME"},
		{name: "missing workflow", change: func(_ *githubAppServer, request *VerificationRequest) { request.Workflow = "" }, want: "workflow and ref are required"},
		{name: "missing ref", change: func(_ *githubAppServer, request *VerificationRequest) { request.Ref = "" }, want: "workflow and ref are required"},
		{name: "invalid heartbeat token", change: func(_ *githubAppServer, request *VerificationRequest) { request.HeartbeatToken = "short" }, want: "invalid verification heartbeat token"},
		{name: "missing deadline", change: func(_ *githubAppServer, request *VerificationRequest) { request.Deadline = 0 }, want: "poll interval and deadline must be positive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			github, gatus, request := verificationFixture(t, runState{status: "completed", conclusion: "success"})
			test.change(github, &request)
			if err := RequestVerification(context.Background(), request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("request returned %v; want error containing %q", err, test.want)
			}
			if len(github.calls) != test.calls || len(gatus.heartbeats) != 0 {
				t.Fatalf("requests %v and heartbeats %v, want %d requests and no heartbeat", github.calls, gatus.heartbeats, test.calls)
			}
		})
	}
}

func TestRateLimitDelaysTheNextPoll(t *testing.T) {
	now := time.Unix(1_000, 0)
	for _, test := range []struct {
		status  int
		headers map[string]string
		want    time.Duration
	}{
		{http.StatusOK, map[string]string{"Retry-After": "30"}, 0},
		{http.StatusForbidden, map[string]string{"Retry-After": "30"}, 30 * time.Second},
		{http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, 7 * time.Second},
		{http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1600"}, 10 * time.Minute},
		{http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "12", "X-RateLimit-Reset": "1600"}, 0},
	} {
		response := &http.Response{StatusCode: test.status, Header: http.Header{}}
		for name, value := range test.headers {
			response.Header.Set(name, value)
		}
		if got := retryAfter(response, now); got != test.want {
			t.Errorf("HTTP %d with %v waits %s, want %s", test.status, test.headers, got, test.want)
		}
	}
	if retryAfter(nil, now) != 0 {
		t.Error("failed request delays polling")
	}
}
