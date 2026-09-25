package reconcile

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

type githubAppServer struct {
	t        *testing.T
	key      *rsa.PrivateKey
	token    int
	dispatch int
	mu       sync.Mutex
	calls    []string
	bodies   map[string]any
}

func (s *githubAppServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	var body any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.t.Errorf("%s body: %v", r.URL.Path, err)
	}
	s.bodies[r.URL.Path] = body
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
		if authorization := r.Header.Get("Authorization"); authorization != "token installation-token" {
			s.t.Errorf("dispatch authorized with %q", authorization)
		}
		w.WriteHeader(s.dispatch)
		switch {
		case s.dispatch == http.StatusOK:
			json.NewEncoder(w).Encode(map[string]any{"workflow_run_id": 7, "html_url": "https://github.com/fredrir/infra/actions/runs/7"})
		case s.dispatch >= http.StatusBadRequest:
			json.NewEncoder(w).Encode(map[string]any{"message": "Unexpected inputs provided"})
		}
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func githubAppKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestVerificationRequestDispatchesWithARestrictedInstallationToken(t *testing.T) {
	key, encoded := githubAppKey(t)
	github := &githubAppServer{t: t, key: key, token: http.StatusCreated, dispatch: http.StatusOK, bodies: map[string]any{}}
	server := httptest.NewServer(github)
	defer server.Close()
	run, err := RequestVerification(context.Background(), VerificationRequest{AppID: 42, InstallationID: 43, PrivateKey: encoded, Repository: "fredrir/infra", Workflow: "reconcile.yml", Ref: "main", API: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if run != "https://github.com/fredrir/infra/actions/runs/7" {
		t.Errorf("dispatched run %q", run)
	}
	if want := []string{"POST /app/installations/43/access_tokens", "POST /repos/fredrir/infra/actions/workflows/reconcile.yml/dispatches"}; !reflect.DeepEqual(github.calls, want) {
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
	github.dispatch = http.StatusNoContent
	if run, err := RequestVerification(context.Background(), VerificationRequest{AppID: 42, InstallationID: 43, PrivateKey: encoded, Repository: "fredrir/infra", Workflow: "reconcile.yml", Ref: "main", API: server.URL}); err != nil || run != "" {
		t.Fatalf("dispatch without run details returned %q, %v", run, err)
	}
}

func TestVerificationRequestFailures(t *testing.T) {
	key, encoded := githubAppKey(t)
	valid := VerificationRequest{AppID: 42, InstallationID: 43, PrivateKey: encoded, Repository: "fredrir/infra", Workflow: "reconcile.yml", Ref: "main"}
	for _, test := range []struct {
		name     string
		change   func(*VerificationRequest)
		token    int
		dispatch int
		calls    int
		want     string
	}{
		{name: "token exchange rejected", token: http.StatusUnauthorized, calls: 1, want: "could not refresh installation id 43's token"},
		{name: "dispatch rejected", token: http.StatusCreated, dispatch: http.StatusUnprocessableEntity, calls: 2, want: "dispatch reconcile.yml in fredrir/infra at main: 422 Unprocessable Entity Unexpected inputs provided"},
		{name: "invalid key", change: func(request *VerificationRequest) { request.PrivateKey = []byte("not a key") }, want: "GitHub App private key"},
		{name: "missing app", change: func(request *VerificationRequest) { request.AppID = 0 }, want: "GitHub App and installation IDs are required"},
		{name: "missing installation", change: func(request *VerificationRequest) { request.InstallationID = 0 }, want: "GitHub App and installation IDs are required"},
		{name: "repository without owner", change: func(request *VerificationRequest) { request.Repository = "infra" }, want: `repository "infra" is not OWNER/NAME`},
		{name: "nested repository", change: func(request *VerificationRequest) { request.Repository = "fredrir/infra/extra" }, want: "is not OWNER/NAME"},
		{name: "missing workflow", change: func(request *VerificationRequest) { request.Workflow = "" }, want: "workflow and ref are required"},
		{name: "missing ref", change: func(request *VerificationRequest) { request.Ref = "" }, want: "workflow and ref are required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			github := &githubAppServer{t: t, key: key, token: test.token, dispatch: test.dispatch, bodies: map[string]any{}}
			server := httptest.NewServer(github)
			defer server.Close()
			request := valid
			request.API = server.URL
			if test.change != nil {
				test.change(&request)
			}
			run, err := RequestVerification(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), test.want) || run != "" {
				t.Fatalf("request returned %q, %v; want error containing %q", run, err, test.want)
			}
			if len(github.calls) != test.calls {
				t.Fatalf("requests %v, want %d", github.calls, test.calls)
			}
		})
	}
}
