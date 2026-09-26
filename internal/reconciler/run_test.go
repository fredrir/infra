package reconciler

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
	"github.com/fredrir/infra/internal/reconcile"
	"github.com/klauspost/compress/zstd"
)

var isolatedGit = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com"}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir, command.Env = dir, append(os.Environ(), isolatedGit...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func originRepository(t *testing.T) (string, string) {
	t.Helper()
	origin := t.TempDir()
	for path, content := range map[string]string{
		"build/toolchain.json": `{"go": "1.27.1"}`,
		"build/runners.json":   `{"schema": 2, "owner": "fredrir", "host": "infra-build-09", "version": "2.337.0", "sha256": "` + strings.Repeat("a", 64) + `", "labels": ["dagger-amd64"], "repositories": {"infra": 3, "Y": 2}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(origin, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(origin, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, origin, "init", "--quiet", "--initial-branch=main")
	gitCommand(t, origin, "add", ".")
	gitCommand(t, origin, "commit", "--quiet", "--no-gpg-sign", "-m", "declare")
	gitCommand(t, origin, "branch", "production")
	published := gitCommand(t, origin, "rev-parse", "production")
	if err := os.WriteFile(filepath.Join(origin, "unpublished"), []byte("unreviewed"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, origin, "add", ".")
	gitCommand(t, origin, "commit", "--quiet", "--no-gpg-sign", "-m", "unpublished")
	return origin, published
}

type objects struct {
	mu   sync.Mutex
	puts map[string][]byte
}

func (o *objects) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPut || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIAVERIFY/") || r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	o.puts[r.URL.Path] = body
	w.Header().Set("ETag", `"etag"`)
}

type heartbeats struct {
	mu       sync.Mutex
	received []url.Values
}

func (h *heartbeats) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/endpoints/reconciliation_verification/external" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("gatus-secret-", 3) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	h.received = append(h.received, r.URL.Query())
}

type observerApp struct {
	mu      sync.Mutex
	minted  []map[string]any
	revoked []string
}

func (a *observerApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/164992211/access_tokens" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.minted = append(a.minted, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_observer", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
		a.revoked = append(a.revoked, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type harness struct {
	supervisor Supervisor
	revision   string
	objects    *objects
	heartbeats *heartbeats
	app        *observerApp
	commands   []string
}

func newHarness(t *testing.T, credentials map[string]string, engine func(t *testing.T, args []string, env []string) (int, string), build error) *harness {
	t.Helper()
	origin, revision := originRepository(t)
	return newHarnessAt(t, origin, revision, credentials, engine, build)
}

func newHarnessAt(t *testing.T, origin, revision string, credentials map[string]string, engine func(t *testing.T, args []string, env []string) (int, string), build error) *harness {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := credentials[ObserverAppKey]; ok {
		credentials[ObserverAppKey] = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	}
	h := &harness{revision: revision, objects: &objects{puts: map[string][]byte{}}, heartbeats: &heartbeats{}, app: &observerApp{}}
	servers := map[string]http.Handler{"s3": h.objects, "gatus": h.heartbeats, "github": h.app}
	urls := map[string]string{}
	for name, handler := range servers {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		urls[name] = server.URL
	}
	authority := filepath.Join(t.TempDir(), "kubernetes-ca.crt")
	if err := os.WriteFile(authority, testAuthority(t), 0o644); err != nil {
		t.Fatal(err)
	}
	config := validConfig()
	config.Repository, config.State, config.Endpoint, config.Gatus, config.Observer.API, config.Kubernetes.CertificateAuthority = "file://"+origin, t.TempDir(), urls["s3"], urls["gatus"], urls["github"], authority
	var mu sync.Mutex
	h.supervisor = Supervisor{Config: config, Credentials: writeCredentials(t, anyValues(credentials)), Now: func() time.Time { return time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC) }, Execute: func(ctx context.Context, options process.Options) (process.Result, error) {
		mu.Lock()
		h.commands = append(h.commands, filepath.Base(options.Name)+" "+strings.Join(options.Args, " "))
		mu.Unlock()
		if name := filepath.Base(options.Name); name == "git" || name == "infra" {
			for _, variable := range hardenedGit {
				if !slices.Contains(options.Env, variable) {
					t.Errorf("%s %q runs without %s", name, options.Args, variable)
				}
			}
		}
		run := runDirectory(t, config.State, options.Env)
		if !slices.Contains(options.Env, "PATH="+filepath.Join(run, "tools")+":/usr/local/bin:/usr/bin:/bin") || !slices.Contains(options.Env, "HOME="+filepath.Join(run, "home")) {
			t.Errorf("%s runs with %q", options.Name, options.Env)
		}
		verifying := filepath.Base(options.Name) == "infra" && slices.Contains(options.Args, "verify")
		for _, variable := range options.Env {
			for name, secret := range credentials {
				allowed := verifying && name != GatusToken && name != ObserverAppKey
				if strings.Contains(variable, strings.TrimSpace(secret)) && !allowed {
					t.Errorf("%s received %s in %s", options.Name, name, strings.SplitN(variable, "=", 2)[0])
				}
			}
		}
		switch filepath.Base(options.Name) {
		case "git":
			return process.Run(ctx, options)
		case "go":
			run := runDirectory(t, config.State, options.Env)
			if !slices.Contains(options.Env, "GOTOOLCHAIN=go1.27.1") || !slices.Contains(options.Env, "GOFLAGS=-mod=readonly -modcacherw") || !slices.Contains(options.Env, "GOMODCACHE="+filepath.Join(run, "go", "mod")) || !slices.Contains(options.Env, "GOCACHE="+filepath.Join(run, "go", "cache")) {
				t.Errorf("engine built with %q", options.Env)
			}
			if build != nil {
				return process.Result{ExitCode: 1}, build
			}
			return process.Result{}, os.WriteFile(options.Args[3], []byte("engine"), 0o755)
		case "infra":
			if options.Args[0] == "ci" {
				if !slices.Contains(options.Env, "INFRA_TOOL_CACHE="+filepath.Join(runDirectory(t, config.State, options.Env), "tools")) || !reflect.DeepEqual(options.Args[4:], cloudTools) {
					t.Errorf("tools installed with %q %q", options.Args, options.Env)
				}
				return process.Result{}, nil
			}
			code, report := engine(t, options.Args, options.Env)
			if report != "" {
				if err := os.WriteFile(strings.TrimPrefix(options.Args[len(options.Args)-1], "--report="), []byte(report), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, _ = io.WriteString(options.Stdout, "engine compared declarations\n")
			if code != 0 {
				return process.Result{ExitCode: code}, errors.New("infra failed")
			}
			return process.Result{}, nil
		}
		t.Errorf("unexpected command %s", options.Name)
		return process.Result{}, errors.New("unexpected command")
	}}
	return h
}

func runDirectory(t *testing.T, state string, env []string) string {
	t.Helper()
	for _, variable := range env {
		if home, ok := strings.CutPrefix(variable, "HOME="); ok && filepath.Base(home) == "home" && filepath.Dir(filepath.Dir(home)) == state && strings.HasPrefix(filepath.Base(filepath.Dir(home)), "run-") {
			return filepath.Dir(home)
		}
	}
	t.Errorf("no per-run home under %s in %q", state, env)
	return ""
}

func plantPoison(t *testing.T, state string) {
	t.Helper()
	for _, path := range []string{"tools/tofu", "tools/tofu.json", "go/mod/cache/download/poison", "mirror.git/hooks/post-checkout", "run-stale/home/.gitconfig"} {
		if err := os.MkdirAll(filepath.Join(state, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, path), []byte("#!/bin/sh\ntouch "+filepath.Join(state, "poisoned")+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(state, "go/mod/cache/download"), 0o555); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) uploaded(t *testing.T) (Run, string) {
	t.Helper()
	prefix := "/" + h.supervisor.Config.Bucket + "/" + h.supervisor.Config.Prefix + "/runs/20260926T030000Z-verify-" + h.revision[:12] + "/"
	report, compressed := h.objects.puts[prefix+"report.json"], h.objects.puts[prefix+"log.txt.zst"]
	if report == nil || compressed == nil || len(h.objects.puts) != 2 {
		t.Fatalf("uploaded %v, want report and log under %s", slices.Collect(maps.Keys(h.objects.puts)), prefix)
	}
	var run Run
	if err := json.Unmarshal(report, &run); err != nil {
		t.Fatal(err)
	}
	decoder, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	log, err := io.ReadAll(decoder)
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range verifyCredentialValues() {
		if name != PlatformMailRecipient && (bytes.Contains(log, []byte(strings.TrimSpace(secret))) || bytes.Contains(report, []byte(strings.TrimSpace(secret)))) {
			t.Errorf("uploaded run contains %s", name)
		}
	}
	return run, string(log)
}

func verification(outcome string, differences []reconcile.Difference, errors []string) string {
	data, _ := json.Marshal(reconcile.Verification{Scope: reconcile.ScopeCloud, Outcome: outcome, Differences: differences, Errors: errors})
	return string(data)
}

func TestVerifyBuildsThePublishedEngineAndReportsItsOutcome(t *testing.T) {
	differs := []reconcile.Difference{{System: "opentofu", Item: "cloudflare_dns_record.grafana update"}}
	for _, test := range []struct {
		name          string
		code          int
		report        string
		wantOutcome   string
		wantHeartbeat url.Values
		wantErr       string
	}{
		{name: "matches", report: verification("matches", []reconcile.Difference{}, []string{}), wantOutcome: "matches", wantHeartbeat: url.Values{"success": {"true"}}},
		{name: "differs", code: 1, report: verification("differs", differs, []string{}), wantOutcome: "differs", wantHeartbeat: url.Values{"success": {"false"}, "error": {"1 differences: opentofu: cloudflare_dns_record.grafana update"}}, wantErr: "1 differences"},
		{name: "fails", code: 1, report: verification("failed", []reconcile.Difference{}, []string{"kubectl failed: connection refused"}), wantOutcome: "failed", wantHeartbeat: url.Values{"success": {"false"}, "error": {"verify: kubectl failed: connection refused"}}, wantErr: "connection refused"},
		{name: "locked", code: 75, report: verification("failed", []reconcile.Difference{}, []string{"comparisons skipped: reconciliation locked by fredrir-11 pid 7 until 2026-09-26 03:10:00 +0000 UTC"}), wantOutcome: "skipped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var engineArgs, engineEnv []string
			h := newHarness(t, verifyCredentialValues(), func(t *testing.T, args, env []string) (int, string) {
				engineArgs, engineEnv = args, env
				return test.code, test.report
			}, nil)
			plantPoison(t, h.supervisor.Config.State)
			err := h.supervisor.Verify(context.Background())
			if (err == nil) != (test.wantErr == "") || (err != nil && !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("verify returned %v, want %q", err, test.wantErr)
			}
			source := filepath.Join(filepath.Dir(strings.TrimPrefix(engineArgs[3], "--root=")), "source")
			if want := []string{"reconcile", "verify", "--scope=cloud", "--root=" + source, "--state-bucket=llunde-pyparser-bucket", "--state-prefix=reconciliation/production", "--report=" + filepath.Join(filepath.Dir(source), "verification.json")}; !slices.Equal(engineArgs, want) {
				t.Errorf("engine ran %q, want %q", engineArgs, want)
			}
			for _, variable := range []string{"GH_TOKEN=ghs_observer", "AWS_ACCESS_KEY_ID=AKIAVERIFY", "AWS_ENDPOINT_URL_S3=" + h.supervisor.Config.Endpoint, "TF_VAR_hcloud_token=hcloud-secret-value"} {
				if !slices.Contains(engineEnv, variable) {
					t.Errorf("engine environment lacks %s", strings.SplitN(variable, "=", 2)[0])
				}
			}
			run, log := h.uploaded(t)
			if run.Outcome != test.wantOutcome || run.Revision != h.revision || run.Stage != "verify" || run.Verification == nil || run.Kind != "verify" {
				t.Errorf("uploaded run %+v", run)
			}
			if !strings.Contains(log, "Verifying production at "+h.revision+"\n") || !strings.Contains(log, "engine compared declarations\n") {
				t.Errorf("uploaded log lacks the run output:\n%s", log)
			}
			var want []url.Values
			if test.wantHeartbeat != nil {
				want = []url.Values{test.wantHeartbeat}
			}
			if !reflect.DeepEqual(h.heartbeats.received, want) {
				t.Errorf("heartbeats %v, want %v", h.heartbeats.received, want)
			}
			wantMint := []map[string]any{{"repositories": []any{"Y", "infra"}, "permissions": map[string]any{"administration": "read", "metadata": "read"}}}
			if !reflect.DeepEqual(h.app.minted, wantMint) || !slices.Equal(h.app.revoked, []string{"Bearer ghs_observer"}) {
				t.Errorf("observer tokens minted %v and revoked %v", h.app.minted, h.app.revoked)
			}
			if entries, err := os.ReadDir(h.supervisor.Config.State); err != nil || len(entries) != 0 {
				t.Errorf("state kept %v across the run: %v", entries, err)
			}
		})
	}
}

func TestVerifyReportsFailuresBeforeTheEngineRuns(t *testing.T) {
	missing := verifyCredentialValues()
	delete(missing, KubernetesToken)
	for _, test := range []struct {
		name        string
		credentials map[string]string
		build       error
		wantStage   string
		wantError   string
	}{
		{name: "missing credential", credentials: missing, wantStage: "credentials", wantError: "credentials: credential kubernetes-token: missing"},
		{name: "build failure", credentials: verifyCredentialValues(), build: errors.New("go failed: exit status 1"), wantStage: "build", wantError: "build: build engine with go1.27.1: go failed: exit status 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, test.credentials, func(t *testing.T, _, _ []string) (int, string) {
				t.Error("engine verification ran")
				return 0, ""
			}, test.build)
			if err := h.supervisor.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("verify returned %v, want %q", err, test.wantError)
			}
			if len(h.heartbeats.received) != 1 || h.heartbeats.received[0].Get("success") != "false" || !strings.HasPrefix(h.heartbeats.received[0].Get("error"), test.wantError) {
				t.Fatalf("heartbeats %v", h.heartbeats.received)
			}
			var run Run
			for path, body := range h.objects.puts {
				if strings.HasSuffix(path, "/report.json") {
					if err := json.Unmarshal(body, &run); err != nil {
						t.Fatal(err)
					}
				}
			}
			if run.Stage != test.wantStage || run.Outcome != "failed" || run.Verification != nil {
				t.Fatalf("uploaded run %+v", run)
			}
		})
	}
}

func TestVerifyRefusesToBuildProductionOffMain(t *testing.T) {
	origin, forged := offMainOrigin(t)
	h := newHarnessAt(t, origin, forged, verifyCredentialValues(), func(t *testing.T, _, _ []string) (int, string) {
		t.Error("off-main production code ran")
		return 0, ""
	}, errors.New("off-main production built"))
	want := "production at " + forged + " is not on main"
	if err := h.supervisor.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("verify returned %v, want %q", err, want)
	}
	if slices.ContainsFunc(h.commands, func(command string) bool { return !strings.HasPrefix(command, "git ") }) {
		t.Fatalf("off-main production ran %q", h.commands)
	}
	run, log := h.uploaded(t)
	wantVerification := reconcile.Verification{Revision: forged, Scope: reconcile.ScopeCloud, Outcome: reconcile.OutcomeDiffers, Differences: []reconcile.Difference{{System: "revision", Item: want}}, Errors: []string{}}
	if run.Revision != forged || run.Stage != "checkout" || run.Outcome != reconcile.OutcomeDiffers || run.Verification == nil || !reflect.DeepEqual(*run.Verification, wantVerification) {
		t.Fatalf("uploaded run %+v", run)
	}
	if !strings.Contains(log, "Refusing production at "+forged+": not on main\n") {
		t.Fatalf("uploaded log:\n%s", log)
	}
	if want := []url.Values{{"success": {"false"}, "error": {"1 differences: revision: " + want}}}; !reflect.DeepEqual(h.heartbeats.received, want) {
		t.Fatalf("heartbeats %v, want %v", h.heartbeats.received, want)
	}
	if len(h.app.minted) != 0 {
		t.Fatalf("observer tokens minted for off-main production: %v", h.app.minted)
	}
}

func TestVerifyFailsClosedWithoutMain(t *testing.T) {
	origin, published := originRepository(t)
	gitCommand(t, origin, "checkout", "--quiet", "production")
	gitCommand(t, origin, "branch", "-D", "main")
	h := newHarnessAt(t, origin, published, verifyCredentialValues(), func(t *testing.T, _, _ []string) (int, string) {
		t.Error("engine ran without a main ancestry check")
		return 0, ""
	}, errors.New("built without a main ancestry check"))
	if err := h.supervisor.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "checkout: git") {
		t.Fatalf("verify returned %v, want a checkout failure", err)
	}
	if slices.ContainsFunc(h.commands, func(command string) bool { return !strings.HasPrefix(command, "git ") }) {
		t.Fatalf("ran %q without main", h.commands)
	}
	if len(h.heartbeats.received) != 1 || h.heartbeats.received[0].Get("success") != "false" || !strings.HasPrefix(h.heartbeats.received[0].Get("error"), "checkout: ") {
		t.Fatalf("heartbeats %v", h.heartbeats.received)
	}
}
