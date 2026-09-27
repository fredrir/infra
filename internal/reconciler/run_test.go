package reconciler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

type repositoryTemplate struct {
	once      sync.Once
	directory string
}

var originTemplate, offMainTemplate repositoryTemplate

func (r *repositoryTemplate) copy(t *testing.T, build func(t *testing.T) string) string {
	t.Helper()
	r.once.Do(func() { r.directory = build(t) })
	if r.directory == "" {
		t.Fatal("repository template unavailable")
	}
	copied := t.TempDir()
	if err := os.CopyFS(copied, os.DirFS(r.directory)); err != nil {
		t.Fatal(err)
	}
	return copied
}

func branchTip(t *testing.T, repository, branch string) string {
	t.Helper()
	tip, err := os.ReadFile(filepath.Join(repository, ".git", "refs", "heads", branch))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(tip))
}

func originRepository(t *testing.T) (string, string) {
	t.Helper()
	origin := originTemplate.copy(t, buildOriginRepository)
	return origin, branchTip(t, origin, "production")
}

func TestOriginTemplateStartsNoBackgroundGitMaintenance(t *testing.T) {
	events := filepath.Join(t.TempDir(), "trace2.json")
	t.Setenv("GIT_TRACE2_EVENT", events)
	directory := buildOriginRepository(t)
	t.Cleanup(func() { os.RemoveAll(directory) })
	if started := gitMaintenanceStarts(t, events); len(started) != 0 {
		t.Fatalf("building the origin template started %q", started)
	}
}

func gitMaintenanceStarts(t *testing.T, events string) []string {
	t.Helper()
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "child_start" && len(event.Argv) > 1 && (event.Argv[1] == "maintenance" || event.Argv[1] == "gc") {
			started = append(started, strings.Join(event.Argv, " "))
		}
	}
	return started
}

func buildOriginRepository(t *testing.T) string {
	t.Helper()
	origin, err := os.MkdirTemp("", "reconciler-origin-")
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"build/toolchain.json":     `{"go": "1.27.1"}`,
		"build/runners.json":       `{"schema": 2, "owner": "fredrir", "host": "infra-build-09", "version": "2.337.0", "sha256": "` + strings.Repeat("a", 64) + `", "labels": ["dagger-amd64"], "repositories": {"infra": 3, "Y": 2}}`,
		"tofu/.terraform.lock.hcl": providerLock,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(origin, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(origin, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, origin, "init", "--quiet", "--initial-branch=main", "--ref-format=files")
	gitCommand(t, origin, "config", "maintenance.auto", "false")
	gitCommand(t, origin, "config", "gc.auto", "0")
	gitCommand(t, origin, "add", ".")
	gitCommand(t, origin, "commit", "--quiet", "--no-gpg-sign", "-m", "declare")
	gitCommand(t, origin, "branch", "production")
	if err := os.WriteFile(filepath.Join(origin, "unpublished"), []byte("unreviewed"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, origin, "add", ".")
	gitCommand(t, origin, "commit", "--quiet", "--no-gpg-sign", "-m", "unpublished")
	return origin
}

const providerLock = `provider "registry.opentofu.org/hashicorp/aws" {
  version = "5.100.0"
}
`

var moduleDownloads = map[string]bool{
	"golang.org/toolchain/@v/v0.0.1-go1.27.1.linux-amd64.zip":     true,
	"golang.org/toolchain/@v/v0.0.1-go1.27.1.linux-amd64.ziphash": false,
	"golang.org/toolchain/@v/v0.0.1-go1.27.1.linux-amd64.lock":    false,
	"github.com/spf13/cobra/@v/list":                              true,
	"github.com/spf13/cobra/@v/v1.10.2.info":                      true,
	"github.com/spf13/cobra/@v/v1.10.2.mod":                       true,
	"github.com/spf13/cobra/@v/v1.10.2.zip":                       true,
	"github.com/spf13/cobra/@v/v1.10.2.zip123.tmp":                false,
	"sumdb/sum.golang.org/latest":                                 false,
}

func downloadModules(t *testing.T, directory string) {
	t.Helper()
	for path := range moduleDownloads {
		if err := os.MkdirAll(filepath.Join(directory, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, path), []byte(path), 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

func cachedFiles(t *testing.T, directory string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			relative, _ := filepath.Rel(directory, path)
			files = append(files, relative)
		}
		return err
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	slices.Sort(files)
	return files
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
	mu         sync.Mutex
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
	if _, ok := credentials[ObserverAppKey]; ok {
		credentials[ObserverAppKey] = privateKey(t, "observer")
	}
	h := &harness{revision: revision, objects: &objects{puts: map[string][]byte{}}, heartbeats: &heartbeats{}, app: &observerApp{}}
	servers := map[string]http.Handler{"s3": h.objects, "gatus": h.heartbeats, "github": h.app}
	urls, clients := map[string]string{}, map[string]*http.Client{}
	for name, handler := range servers {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		urls[name], clients[name] = server.URL, server.Client()
	}
	authority := filepath.Join(t.TempDir(), "kubernetes-ca.crt")
	if err := os.WriteFile(authority, testAuthority(t), 0o644); err != nil {
		t.Fatal(err)
	}
	config := validConfig()
	config.Repository, config.State, config.Cache, config.Endpoint, config.Gatus, config.Observer.API, config.Kubernetes.CertificateAuthority = "file://"+origin, t.TempDir(), t.TempDir(), urls["s3"], urls["gatus"], urls["github"], authority
	config.Shared = sharedDirectory(t)
	h.supervisor = Supervisor{Config: config, Credentials: writeCredentials(t, anyValues(credentials)), Now: func() time.Time { return time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC) }, HTTP: clients["s3"], Execute: func(ctx context.Context, options process.Options) (process.Result, error) {
		h.mu.Lock()
		h.commands = append(h.commands, filepath.Base(options.Name)+" "+strings.Join(options.Args, " "))
		h.mu.Unlock()
		if name := filepath.Base(options.Name); name == "git" || name == "infra" {
			for _, variable := range hardenedGit {
				if !slices.Contains(options.Env, variable) {
					t.Errorf("%s %q runs without %s", name, options.Args, variable)
				}
			}
		}
		run := runDirectory(t, config.State, options.Env)
		path := filepath.Join(run, "tools") + ":/usr/local/bin:/usr/bin:/bin"
		if h.supervisor.Config.Scope == reconcile.ScopeFull {
			path = filepath.Join(run, "venv", "bin") + ":" + path
		}
		if !slices.Contains(options.Env, "PATH="+path) || !slices.Contains(options.Env, "HOME="+filepath.Join(run, "home")) {
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
		case "uv":
			return process.Result{}, nil
		case "go":
			run := runDirectory(t, config.State, options.Env)
			for _, variable := range []string{"GOTOOLCHAIN=go1.27.1", "GOFLAGS=-mod=readonly -modcacherw", "GOMODCACHE=" + filepath.Join(run, "go", "mod"), "GOCACHE=" + filepath.Join(run, "go", "cache"), "GOPROXY=file://" + filepath.Join(config.Cache, "go") + ",https://proxy.golang.org,direct"} {
				if !slices.Contains(options.Env, variable) {
					t.Errorf("engine built without %s in %q", variable, options.Env)
				}
			}
			downloadModules(t, filepath.Join(run, "go", "mod", "cache", "download"))
			if build != nil {
				return process.Result{ExitCode: 1}, build
			}
			return process.Result{}, os.WriteFile(options.Args[3], []byte("engine"), 0o755)
		case "infra":
			if options.Args[0] == "ci" {
				tools := cloudTools
				if h.supervisor.Config.Scope == reconcile.ScopeFull {
					tools = append(slices.Clone(cloudTools), "uv")
				}
				if !slices.Contains(options.Env, "INFRA_TOOL_CACHE="+filepath.Join(runDirectory(t, config.State, options.Env), "tools")) || !slices.Contains(options.Env, "INFRA_TOOL_DOWNLOADS="+filepath.Join(config.Cache, "tools")) || !reflect.DeepEqual(options.Args[4:], tools) {
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

func TestMain(m *testing.M) {
	memoryBacked = func(string) error { return nil }
	code := m.Run()
	for _, template := range []*repositoryTemplate{&originTemplate, &offMainTemplate} {
		if template.directory != "" {
			os.RemoveAll(template.directory)
		}
	}
	os.Exit(code)
}

func sharedDirectory(t *testing.T) string {
	t.Helper()
	shared := t.TempDir()
	for _, directory := range []string{"repairs", "requests"} {
		if err := os.Mkdir(filepath.Join(shared, directory), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, "lock"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	return shared
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
	t.Parallel()
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
			t.Parallel()
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
			plugins := filepath.Join(h.supervisor.Config.Cache, "tofu", fmt.Sprintf("%x", sha256.Sum256([]byte(providerLock))))
			for _, variable := range []string{"GH_TOKEN=ghs_observer", "AWS_ACCESS_KEY_ID=AKIAVERIFY", "AWS_ENDPOINT_URL_S3=" + h.supervisor.Config.Endpoint, "TF_VAR_hcloud_token=hcloud-secret-value", "TF_PLUGIN_CACHE_DIR=" + plugins} {
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
	t.Parallel()
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
			t.Parallel()
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
			if cached := cachedFiles(t, h.supervisor.Config.Cache); len(cached) != 0 {
				t.Fatalf("unverified run cached %q", cached)
			}
		})
	}
}

func TestVerifyKeepsOnlyReverifiedCachesAcrossRuns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, verifyCredentialValues(), func(t *testing.T, _, _ []string) (int, string) {
		return 0, verification("matches", []reconcile.Difference{}, []string{})
	}, nil)
	cache := h.supervisor.Config.Cache
	plantPoison(t, h.supervisor.Config.State)
	stale := []string{"go/example.com/retired/@v/v0.1.0.zip", "go/github.com/spf13/cobra/@v/.mirror-123", "tofu/" + strings.Repeat("0", 64) + "/registry.opentofu.org/hashicorp/aws/5.99.0/linux_amd64/terraform-provider-aws"}
	for _, path := range append(stale, "go/github.com/spf13/cobra/@v/v1.10.2.zip") {
		if err := os.MkdirAll(filepath.Join(cache, filepath.Dir(path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, path), []byte("mirrored earlier"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.supervisor.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	var want []string
	for path, proxied := range moduleDownloads {
		if proxied {
			want = append(want, path)
		}
	}
	slices.Sort(want)
	if got := cachedFiles(t, filepath.Join(cache, "go")); !slices.Equal(got, want) {
		t.Errorf("module mirror holds %q, want %q", got, want)
	}
	if kept, err := os.ReadFile(filepath.Join(cache, "go/github.com/spf13/cobra/@v/v1.10.2.zip")); err != nil || string(kept) != "mirrored earlier" {
		t.Errorf("mirrored download rewritten: %q %v", kept, err)
	}
	if providers, err := os.ReadDir(filepath.Join(cache, "tofu")); err != nil || len(providers) != 1 || providers[0].Name() != fmt.Sprintf("%x", sha256.Sum256([]byte(providerLock))) {
		t.Errorf("provider cache kept %v: %v", providers, err)
	}
	if _, err := os.Stat(filepath.Join(cache, "go/example.com")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("retired module directory kept: %v", err)
	}
	if entries, err := os.ReadDir(h.supervisor.Config.State); err != nil || len(entries) != 0 {
		t.Errorf("state kept %v across the run: %v", entries, err)
	}
}

func TestVerifyRefusesToBuildProductionOffMain(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
