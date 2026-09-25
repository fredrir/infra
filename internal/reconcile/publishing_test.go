package reconcile

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"github.com/golang-jwt/jwt/v4"
)

const publisherSecret = "ghs_publisher-installation-secret"

type publisherAppServer struct {
	t       *testing.T
	key     *rsa.PublicKey
	mu      sync.Mutex
	minted  []map[string]any
	revoked []string
}

func (s *publisherAppServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "POST /app/installations/43/access_tokens":
		claims := jwt.RegisteredClaims{}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, err := jwt.ParseWithClaims(bearer, &claims, func(*jwt.Token) (any, error) { return s.key, nil }, jwt.WithValidMethods([]string{"RS256"})); err != nil || claims.Issuer != "42" {
			s.t.Errorf("token exchange authenticated as %q: %v", claims.Issuer, err)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Error(err)
		}
		s.minted = append(s.minted, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": publisherSecret, "expires_at": time.Now().Add(time.Hour)})
	case "DELETE /installation/token":
		s.revoked = append(s.revoked, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

type authenticatedGit struct {
	mu             sync.Mutex
	authorizations []string
	urls           []string
	backend        http.Handler
}

func (g *authenticatedGit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.urls = append(g.urls, r.URL.String())
	g.authorizations = append(g.authorizations, r.Header.Get("Authorization"))
	g.mu.Unlock()
	if user, password, ok := r.BasicAuth(); !ok || user != "x-access-token" || password != publisherSecret {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	g.backend.ServeHTTP(w, r)
}

type publishingFixture struct {
	t                *testing.T
	area, origin     string
	checkout, pusher string
	app              *publisherAppServer
	git              *authenticatedGit
	commands         *Commands
	logs             bytes.Buffer
	executed         []process.Options
}

func newPublishingFixture(t *testing.T) *publishingFixture {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	area := t.TempDir()
	f := &publishingFixture{t: t, area: area, origin: filepath.Join(area, "origin.git"), checkout: filepath.Join(area, "checkout"), pusher: filepath.Join(area, "pusher")}
	f.run(area, "init", "--quiet", "--bare", "--initial-branch=main", f.origin)
	f.run(f.origin, "config", "http.receivepack", "true")
	f.run(area, "init", "--quiet", "--initial-branch=main", f.pusher)
	f.commit("tofu/main.tf")
	f.run(area, "clone", "--quiet", f.origin, f.checkout)
	f.run(f.checkout, "config", "credential.helper", "!f() { echo username=ambient; echo password=ambient-secret; }; f")
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	key, encoded := githubAppKey(t)
	f.app = &publisherAppServer{t: t, key: &key.PublicKey}
	f.git = &authenticatedGit{backend: &cgi.Handler{Path: gitBinary, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + area, "GIT_HTTP_EXPORT_ALL=1"}, InheritEnv: []string{"PATH", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL"}}}
	app, remote := httptest.NewServer(f.app), httptest.NewServer(f.git)
	t.Cleanup(app.Close)
	t.Cleanup(remote.Close)
	record := func(ctx context.Context, options process.Options) (process.Result, error) {
		f.executed = append(f.executed, options)
		return process.Run(ctx, options)
	}
	f.commands = &Commands{
		Runner:      ci.Runner{Dir: f.checkout, Execute: record, Stdout: &f.logs, Stderr: &f.logs},
		RequireMain: true,
		Publisher:   &Publisher{Repository: "fredrir/infra", AppID: 42, InstallationID: 43, PrivateKey: encoded, API: app.URL, Remote: remote.URL + "/origin.git"},
	}
	return f
}

func (f *publishingFixture) run(dir string, args ...string) string {
	f.t.Helper()
	command := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (f *publishingFixture) commit(path string) string {
	f.t.Helper()
	return f.commitTo("main", path)
}

func (f *publishingFixture) rogue(ref string) string {
	f.t.Helper()
	f.run(f.pusher, "checkout", "--quiet", "--orphan", "rogue-"+ref)
	return f.commitTo(ref, "rogue.txt")
}

func (f *publishingFixture) commitTo(ref, path string) string {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.pusher, filepath.Dir(path)), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.pusher, path), []byte(path+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.run(f.pusher, "add", "--", path)
	f.run(f.pusher, "commit", "--quiet", "--message", path)
	f.run(f.pusher, "push", "--quiet", "--force", f.origin, "HEAD:refs/heads/"+ref)
	return f.run(f.pusher, "rev-parse", "HEAD")
}

func (f *publishingFixture) production() string {
	f.t.Helper()
	command := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/production")
	command.Dir = f.origin
	output, _ := command.Output()
	return strings.TrimSpace(string(output))
}

func (f *publishingFixture) assertTokenConfinedToThePush() {
	f.t.Helper()
	pushes := 0
	for _, options := range f.executed {
		if strings.Contains(options.Name+strings.Join(options.Args, " "), publisherSecret) {
			f.t.Errorf("token in argv: %s %q", options.Name, options.Args)
		}
		if slices.ContainsFunc(options.Env, func(entry string) bool { return strings.Contains(entry, publisherSecret) }) {
			if options.Name != "git" || len(options.Args) == 0 || options.Args[0] != "push" {
				f.t.Errorf("token in the environment of %s %q", options.Name, options.Args)
			}
			pushes++
		}
		if slices.Contains(options.Args, "--force") || slices.ContainsFunc(options.Args, func(arg string) bool { return strings.HasPrefix(arg, "+") }) {
			f.t.Errorf("forced push %q", options.Args)
		}
	}
	if pushes != 1 {
		f.t.Errorf("token reached %d git pushes, want 1", pushes)
	}
	for _, path := range []string{filepath.Join(f.checkout, ".git", "config"), filepath.Join(f.origin, "config")} {
		data, err := os.ReadFile(path)
		if err != nil {
			f.t.Fatal(err)
		}
		if strings.Contains(string(data), publisherSecret) || strings.Contains(string(data), "x-access-token") {
			f.t.Errorf("%s persists the publisher credential:\n%s", path, data)
		}
	}
	if strings.Contains(f.logs.String(), publisherSecret) {
		f.t.Errorf("token logged:\n%s", f.logs.String())
	}
	for _, url := range f.git.urls {
		if strings.Contains(url, publisherSecret) {
			f.t.Errorf("token in URL %s", url)
		}
	}
	if !slices.Contains(f.git.authorizations, "Basic eC1hY2Nlc3MtdG9rZW46Z2hzX3B1Ymxpc2hlci1pbnN0YWxsYXRpb24tc2VjcmV0") {
		f.t.Errorf("remote never received the publisher token: %q", f.git.authorizations)
	}
	if !reflect.DeepEqual(f.app.minted, []map[string]any{{"repositories": []any{"infra"}, "permissions": map[string]any{"contents": "write"}}}) {
		f.t.Errorf("minted %v", f.app.minted)
	}
	if !reflect.DeepEqual(f.app.revoked, []string{"Bearer " + publisherSecret}) {
		f.t.Errorf("revoked %q", f.app.revoked)
	}
}

func TestPublishPushesWithARevokedAppTokenOnlyThroughTheGitChild(t *testing.T) {
	f := newPublishingFixture(t)
	revision := f.run(f.checkout, "rev-parse", "HEAD")
	if err := f.commands.Publish(context.Background(), revision); err != nil {
		t.Fatalf("publish: %v\n%s", err, f.logs.String())
	}
	if production := f.production(); production != revision {
		t.Fatalf("production at %q, want %s", production, revision)
	}
	f.assertTokenConfinedToThePush()
}

func TestPublishNeverForcesProduction(t *testing.T) {
	f := newPublishingFixture(t)
	revision := f.run(f.checkout, "rev-parse", "HEAD")
	rogue := f.rogue("production")
	if err := f.commands.Publish(context.Background(), revision); err == nil {
		t.Fatal("published over a divergent production")
	}
	if production := f.production(); production != rogue {
		t.Fatalf("production moved to %q", production)
	}
	f.assertTokenConfinedToThePush()
}

func TestPublishMintsNoTokenWhenMainAdvanced(t *testing.T) {
	f := newPublishingFixture(t)
	revision := f.run(f.checkout, "rev-parse", "HEAD")
	f.commit("platform/projects/y/kustomization.yaml")
	if err := f.commands.Publish(context.Background(), revision); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("publish after main advanced returned %v", err)
	}
	if f.production() != "" || len(f.app.minted) != 0 {
		t.Fatalf("superseded publish minted %v and pushed %q", f.app.minted, f.production())
	}
	f.run(f.pusher, "push", "--quiet", "--force", f.origin, revision+":refs/heads/main")
	f.commands.Publisher = nil
	if err := f.commands.Publish(context.Background(), revision); err == nil || !strings.Contains(err.Error(), "publisher App private key required") {
		t.Fatalf("publish without the publisher App returned %v", err)
	}
}

func TestProductionAncestryFollowsCurrentMain(t *testing.T) {
	f := newPublishingFixture(t)
	published := f.run(f.checkout, "rev-parse", "HEAD")
	f.commit("docs/later.md")
	commands := &Commands{Runner: ci.Runner{Dir: f.checkout}}
	if onMain, err := commands.OnMain(context.Background(), published); err != nil || !onMain {
		t.Fatalf("published ancestor of main: %t, %v", onMain, err)
	}
	rogue := f.rogue("rogue")
	f.run(f.checkout, "fetch", "--quiet", "origin", "refs/heads/rogue")
	if onMain, err := commands.OnMain(context.Background(), rogue); err != nil || onMain {
		t.Fatalf("commit off main reported on main: %t, %v", onMain, err)
	}
}
