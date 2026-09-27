package reconciler

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
	"github.com/fredrir/infra/internal/reconcile"
	"github.com/golang-jwt/jwt/v4"
	"github.com/klauspost/compress/zstd"
)

const (
	supervisorBinary = "/opt/supervisor/infra"
	provenanceSecret = "ghp_provenance-secret"
	runnerInstall    = 4242
)

type appIdentity struct {
	issuer string
	key    *rsa.PublicKey
}

type githubAPI struct {
	mu         sync.Mutex
	expiration string
	failMint   bool
	apps       map[string]appIdentity
	minted     map[string][]map[string]any
	revoked    []string
	unexpected []string
	rateReads  int
}

func (g *githubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	authorization := r.Header.Get("Authorization")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		installation := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
		if g.failMint && installation == fmt.Sprint(runnerInstall) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if !g.authenticates(installation, authorization) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		g.minted[installation] = append(g.minted[installation], decoded)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_" + installation, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
		g.revoked = append(g.revoked, authorization)
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/rate_limit" && authorization == "Bearer "+provenanceSecret:
		g.rateReads++
		if g.expiration != "" {
			w.Header().Set("Github-Authentication-Token-Expiration", g.expiration)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{}})
	default:
		g.unexpected = append(g.unexpected, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (g *githubAPI) authenticates(installation, authorization string) bool {
	app, found := g.apps[installation]
	bearer, bearing := strings.CutPrefix(authorization, "Bearer ")
	if !found || !bearing {
		return false
	}
	claims := jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(bearer, &claims, func(*jwt.Token) (any, error) { return app.key, nil }, jwt.WithValidMethods([]string{"RS256"}))
	return err == nil && claims.Issuer == app.issuer
}

func appIdentityOf(t *testing.T, app App, privateKey string) appIdentity {
	t.Helper()
	block, _ := pem.Decode([]byte(privateKey))
	if block == nil {
		t.Fatal("App key is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return appIdentity{issuer: fmt.Sprint(app.AppID), key: &key.PublicKey}
}

type stateBucket struct {
	mu      sync.Mutex
	applied string
	puts    map[string][]byte
}

func (s *stateBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIAAPPLY/") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/llunde-pyparser-bucket/reconciliation/production/status.json":
		w.Header().Set("ETag", `"status"`)
		_ = json.NewEncoder(w).Encode(reconcile.Status{Desired: s.applied, Applied: s.applied, Stage: "complete"})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/llunde-pyparser-bucket/reconciliation/production/runs/") && r.Header.Get("X-Amz-Server-Side-Encryption") == "AES256":
		s.puts[r.URL.Path] = body
		w.Header().Set("ETag", `"etag"`)
	default:
		w.WriteHeader(http.StatusForbidden)
	}
}

type applyHeartbeats struct {
	mu       sync.Mutex
	received []url.Values
}

func (h *applyHeartbeats) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/endpoints/reconciliation_apply/external" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("apply-gatus-", 3) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	h.received = append(h.received, r.URL.Query())
}

type engineCall struct {
	args []string
	env  []string
	work string
}

type applyHarness struct {
	applier     Applier
	origin      string
	applied     string
	tip         string
	credentials map[string]string
	github      *githubAPI
	bucket      *stateBucket
	gatus       *applyHeartbeats
	mu          sync.Mutex
	commands    []string
	secrets     []string
	selection   reconcile.Selection
	gate        func(t *testing.T, call engineCall) (int, string)
	engine      func(t *testing.T, call engineCall) (int, string)
	step        func(t *testing.T, name string, call engineCall) (process.Result, error)
	applies     []engineCall
}

var testKeys = struct {
	sync.Mutex
	byRole map[string]string
}{byRole: map[string]string{}}

func privateKey(t *testing.T, role string) string {
	t.Helper()
	testKeys.Lock()
	defer testKeys.Unlock()
	if key, found := testKeys.byRole[role]; found {
		return key
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	testKeys.byRole[role] = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return testKeys.byRole[role]
}

func applyCredentialValues(t *testing.T) map[string]string {
	return map[string]string{
		AWSAccessKeyID:        "AKIAAPPLY",
		AWSSecretAccessKey:    "aws-apply-secret",
		CloudflareAPIToken:    "cloudflare-apply-secret",
		HcloudToken:           "hcloud-apply-secret",
		PlatformMailRecipient: "operator@example.net",
		KubernetesToken:       "kubernetes-apply-secret",
		RunnerAppKey:          privateKey(t, "runner"),
		PublisherAppKey:       privateKey(t, "publisher"),
		ProvenanceToken:       provenanceSecret,
		GatusToken:            strings.Repeat("apply-gatus-", 3),
	}
}

func validApplyConfig() ApplyConfig {
	site := validConfig().Site
	site.State, site.Cache, site.Heartbeat, site.KnownHosts = "/var/lib/infra-apply", "/var/cache/infra-apply", "reconciliation_apply", "/etc/infra-reconcile/known_hosts"
	return ApplyConfig{
		Site:   site,
		Runner: App{AppID: 4924976, InstallationID: runnerInstall, API: "https://api.github.com"},
	}
}

func newApplyHarness(t *testing.T) *applyHarness {
	t.Helper()
	origin, applied := originRepository(t)
	h := &applyHarness{origin: origin, applied: applied, tip: branchTip(t, origin, "main"), credentials: applyCredentialValues(t), github: &githubAPI{minted: map[string][]map[string]any{}, expiration: "2027-09-26 12:00:00 UTC"}, bucket: &stateBucket{applied: applied, puts: map[string][]byte{}}, gatus: &applyHeartbeats{}, selection: reconcile.Selection{Tofu: true, Ansible: true, HostScope: reconcile.HostScopeFull}}
	urls := map[string]string{}
	for name, handler := range map[string]http.Handler{"github": h.github, "s3": h.bucket, "gatus": h.gatus} {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		urls[name] = server.URL
	}
	authority := filepath.Join(t.TempDir(), "kubernetes-ca.crt")
	identity := filepath.Join(t.TempDir(), "ssh-identity")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	for path, content := range map[string][]byte{authority: testAuthority(t), identity: pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("ssh-identity-secret")}), knownHosts: []byte("fredrir-06 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA\n")} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := validApplyConfig()
	config.Repository, config.State, config.Cache, config.Shared, config.Endpoint, config.Gatus, config.KnownHosts = "file://"+origin, t.TempDir(), t.TempDir(), sharedDirectory(t), urls["s3"], urls["gatus"], knownHosts
	config.Kubernetes.CertificateAuthority = authority
	config.Runner.API = urls["github"]
	h.gate = func(t *testing.T, call engineCall) (int, string) {
		return 0, fmt.Sprintf(`{"base":%q,"revision":%q}`, h.applied, h.tip)
	}
	h.engine = func(t *testing.T, call engineCall) (int, string) {
		return 0, fmt.Sprintf(`{"desired_revision":%q,"applied_revision":%q,"stage":"complete"}`, h.tip, h.tip)
	}
	h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) { return process.Result{}, nil }
	h.applier = Applier{Config: config, Identity: identity, Self: supervisorBinary, Now: func() time.Time { return time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC) }, LockPoll: time.Millisecond, Execute: h.execute(t)}
	h.github.apps = map[string]appIdentity{fmt.Sprint(runnerInstall): appIdentityOf(t, config.Runner, h.credentials[RunnerAppKey])}
	h.writeCredentials(t)
	return h
}

func (h *applyHarness) writeCredentials(t *testing.T) {
	t.Helper()
	h.applier.Credentials = writeCredentials(t, anyValues(h.credentials))
}

func (h *applyHarness) runtime() string {
	return filepath.Dir(h.applier.Credentials)
}

func (h *applyHarness) secretFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	directories, _ := filepath.Glob(filepath.Join(h.runtime(), "secrets-*"))
	for _, directory := range directories {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			files = append(files, entry.Name())
		}
	}
	return files
}

func (h *applyHarness) execute(t *testing.T) func(context.Context, process.Options) (process.Result, error) {
	return func(ctx context.Context, options process.Options) (process.Result, error) {
		name := filepath.Base(options.Name)
		if options.Name == supervisorBinary {
			name = "supervisor"
		}
		command := name + " " + strings.Join(options.Args, " ")
		h.mu.Lock()
		h.commands = append(h.commands, command)
		h.mu.Unlock()
		if name == "git" || name == "infra" || name == "supervisor" {
			for _, variable := range hardenedGit {
				if !slices.Contains(options.Env, variable) {
					t.Errorf("%s %q runs without %s", name, options.Args, variable)
				}
			}
		}
		call := engineCall{args: options.Args, env: options.Env}
		for _, variable := range options.Env {
			if home, ok := strings.CutPrefix(variable, "HOME="); ok && filepath.Base(home) == "home" {
				call.work = filepath.Dir(home)
			}
		}
		allowed := map[string]bool{}
		switch {
		case name == "supervisor" && options.Args[0] == "reconcile":
			allowed = map[string]bool{AWSAccessKeyID: true, AWSSecretAccessKey: true, ProvenanceToken: true}
		case name == "infra" && slices.Equal(options.Args[:2], []string{"reconcile", "requirements"}):
			allowed = map[string]bool{AWSAccessKeyID: true, AWSSecretAccessKey: true}
		case name == "infra" && slices.Equal(options.Args[:2], []string{"reconcile", "apply"}):
			allowed = map[string]bool{AWSAccessKeyID: true, AWSSecretAccessKey: true, CloudflareAPIToken: true, HcloudToken: true, PlatformMailRecipient: true, ProvenanceToken: true}
		}
		for _, variable := range options.Env {
			for credential, secret := range h.credentials {
				if strings.Contains(variable, strings.TrimSpace(secret)) && !allowed[credential] {
					t.Errorf("%s %s received %s in %s", name, options.Args[0], credential, strings.SplitN(variable, "=", 2)[0])
				}
			}
		}
		if name == "go" || name == "uv" || (name == "infra" && options.Args[0] == "ci") {
			if files := h.secretFiles(t); len(files) != 0 {
				t.Errorf("%s ran while %v held secrets", command, files)
			}
			if slices.ContainsFunc(options.Env, func(variable string) bool { return strings.HasPrefix(variable, "ANSIBLE_SSH_EXTRA_ARGS=") }) {
				t.Errorf("%s ran with host access", command)
			}
		}
		report := func(code int, content string) (process.Result, error) {
			for _, argument := range options.Args {
				if path, ok := strings.CutPrefix(argument, "--report="); ok && content != "" {
					if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if code != 0 {
				return process.Result{ExitCode: code}, fmt.Errorf("%s exited %d", name, code)
			}
			return process.Result{}, nil
		}
		switch name {
		case "git":
			if slices.Contains(options.Args, "fetch") {
				if result, err := h.step(t, "fetch", call); err != nil {
					return result, err
				}
			}
			return process.Run(ctx, options)
		case "supervisor":
			if options.Args[0] == "ci" {
				if !slices.Contains(options.Env, "INFRA_TOOL_CACHE="+filepath.Join(call.work, "gate", "tools")) || !slices.Contains(options.Env, "INFRA_TOOL_DOWNLOADS="+filepath.Join(h.applier.Config.Cache, "gate-tools")) || !slices.Equal(options.Args[4:], gateTools) {
					t.Errorf("gate tools installed with %q %q", options.Args, options.Env)
				}
				return h.step(t, "gate-tools", call)
			}
			if !slices.Contains(options.Env, "PATH="+filepath.Join(call.work, "gate", "tools")+":/usr/local/bin:/usr/bin:/bin") || !slices.ContainsFunc(options.Env, func(variable string) bool {
				return strings.HasPrefix(variable, "TMPDIR="+filepath.Join(h.runtime(), "secrets-"))
			}) {
				t.Errorf("gate runs without its own tools and private TMPDIR: %q", options.Env)
			}
			return report(h.gate(t, call))
		case "go":
			downloadModules(t, filepath.Join(call.work, "go", "mod", "cache", "download"))
			if result, err := h.step(t, "build", call); err != nil {
				return result, err
			}
			return process.Result{}, os.WriteFile(options.Args[3], []byte("engine"), 0o755)
		case "uv":
			for _, variable := range []string{"UV_PROJECT_ENVIRONMENT=" + filepath.Join(call.work, "venv"), "UV_PYTHON_DOWNLOADS=never", "UV_CACHE_DIR=" + filepath.Join(h.applier.Config.Cache, "uv")} {
				if !slices.Contains(options.Env, variable) {
					t.Errorf("uv runs without %s", variable)
				}
			}
			return h.step(t, "uv", call)
		case "infra":
			switch {
			case options.Args[0] == "ci" && options.Args[1] == "install-tools":
				if !slices.Equal(options.Args[4:], applyTools) {
					t.Errorf("engine installed %q", options.Args[4:])
				}
				return h.step(t, "tools", call)
			case options.Args[0] == "ci":
				return h.step(t, options.Args[1], call)
			case options.Args[1] == "requirements":
				if result, err := h.step(t, "requirements", call); err != nil {
					return result, err
				}
				data, _ := json.Marshal(h.selection)
				return process.Result{Stdout: data}, nil
			}
			h.mu.Lock()
			h.applies = append(h.applies, call)
			h.mu.Unlock()
			h.inspectApply(t, call)
			return report(h.engine(t, call))
		}
		t.Errorf("unexpected command %s", options.Name)
		return process.Result{}, errors.New("unexpected command")
	}
}

func (h *applyHarness) inspectApply(t *testing.T, call engineCall) {
	t.Helper()
	environment := map[string]string{}
	for _, variable := range call.env {
		name, value, _ := strings.Cut(variable, "=")
		environment[name] = value
	}
	hosts := h.selection.Ansible || h.selection.Tooling
	runnerToken := ""
	if hosts {
		runnerToken = fmt.Sprintf("ghs_%d", runnerInstall)
	}
	secrets := filepath.Dir(environment["KUBECONFIG"])
	for name, want := range map[string]string{"GH_TOKEN": runnerToken, "INFRA_RECONCILE_TAILNET": "true", "PROVENANCE_TOKEN": provenanceSecret, "AWS_ENDPOINT_URL_S3": h.applier.Config.Endpoint, "TMPDIR": secrets, "TF_PLUGIN_CACHE_DIR": filepath.Join(h.applier.Config.Cache, "tofu", fmt.Sprintf("%x", sha256.Sum256([]byte(providerLock))))} {
		if environment[name] != want {
			t.Errorf("engine %s = %q, want %q", name, environment[name], want)
		}
	}
	if filepath.Dir(secrets) != h.runtime() || !strings.HasPrefix(filepath.Base(secrets), "secrets-") || filepath.Dir(environment["PUBLISHER_APP_PRIVATE_KEY_FILE"]) != secrets {
		t.Errorf("secret files outside the runtime directory: %q", environment)
	}
	if !strings.HasPrefix(environment["PATH"], filepath.Join(call.work, "venv", "bin")+":") {
		t.Errorf("engine PATH %q lacks the Ansible environment", environment["PATH"])
	}
	if data, err := os.ReadFile(environment["PUBLISHER_APP_PRIVATE_KEY_FILE"]); err != nil || string(data) != strings.TrimSpace(h.credentials[PublisherAppKey])+"\n" {
		t.Errorf("publisher key file does not hold the key: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(call.work, "home")); err != nil || len(entries) != 0 {
		t.Errorf("engine HOME holds %v: %v", entries, err)
	}
	extra := environment["ANSIBLE_SSH_EXTRA_ARGS"]
	sshConfig, err := os.ReadFile(strings.TrimPrefix(extra, "-F "))
	if err != nil || filepath.Dir(strings.TrimPrefix(extra, "-F ")) != secrets || !strings.Contains(string(sshConfig), "IdentityFile "+h.applier.Identity+"\n") || !strings.Contains(string(sshConfig), "UserKnownHostsFile "+h.applier.Config.KnownHosts+"\n") {
		t.Errorf("SSH configuration %q: %s %v", extra, sshConfig, err)
	}
	var config kubeconfig
	data, err := os.ReadFile(environment["KUBECONFIG"])
	if err != nil || json.Unmarshal(data, &config) != nil || config.Users[0].Name != "infrastructure-apply" || config.Users[0].User.Token != h.credentials[KubernetesToken] {
		t.Errorf("kubeconfig %s: %v", data, err)
	}
}

func (h *applyHarness) index(t *testing.T, prefix string) int {
	t.Helper()
	return slices.IndexFunc(h.commands, func(command string) bool { return strings.HasPrefix(command, prefix) })
}

func (h *applyHarness) uploaded(t *testing.T) (Run, string) {
	t.Helper()
	h.bucket.mu.Lock()
	defer h.bucket.mu.Unlock()
	prefix := "/llunde-pyparser-bucket/reconciliation/production/runs/20260926T030000Z-apply-" + h.tip[:12] + "/"
	report, compressed := h.bucket.puts[prefix+"report.json"], h.bucket.puts[prefix+"log.txt.zst"]
	if report == nil || compressed == nil || len(h.bucket.puts) != 2 {
		t.Fatalf("uploaded %v, want report and log under %s", slices.Collect(maps.Keys(h.bucket.puts)), prefix)
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
	for name, secret := range h.credentials {
		if name != PlatformMailRecipient && (bytes.Contains(log, []byte(strings.TrimSpace(secret))) || bytes.Contains(report, []byte(strings.TrimSpace(secret)))) {
			t.Errorf("uploaded run contains %s", name)
		}
	}
	return run, string(log)
}

func (h *applyHarness) ledger(t *testing.T) Ledger {
	t.Helper()
	ledger, err := loadLedger(h.applier.Config.State)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func (h *applyHarness) commit(t *testing.T, files map[string]string) string {
	t.Helper()
	for path, content := range files {
		if err := os.MkdirAll(filepath.Join(h.origin, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.origin, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, h.origin, "add", ".")
	gitCommand(t, h.origin, "commit", "--quiet", "--no-gpg-sign", "-m", "change")
	return branchTip(t, h.origin, "main")
}

func (h *applyHarness) settle(t *testing.T, ledger Ledger) {
	t.Helper()
	if err := saveLedger(h.applier.Config.State, ledger); err != nil {
		t.Fatal(err)
	}
}

func (h *applyHarness) reset(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	h.commands, h.applies = nil, nil
	h.mu.Unlock()
	h.github.unexpected, h.gatus.received = nil, nil
	h.bucket.puts = map[string][]byte{}
	h.writeCredentials(t)
}

func TestApplyGatesTheMainTipBeforeBuildingAndPublishesThroughTheEngine(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if err := h.applier.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate, build, validate, apply := h.index(t, "supervisor reconcile provenance"), h.index(t, "go build"), h.index(t, "infra ci validate --before="+h.applied), h.index(t, "infra reconcile apply")
	if gate < 0 || h.index(t, "supervisor ci install-tools") > gate || gate > build || build > validate || validate > apply || h.index(t, "uv sync") > validate || h.index(t, "infra ci prepare-validation --before="+h.applied) > validate {
		t.Fatalf("commands ran in order %q", h.commands)
	}
	source := filepath.Join(h.applies[0].work, "source")
	if want := []string{"reconcile", "apply", "--root=" + source, "--state-bucket=llunde-pyparser-bucket", "--state-prefix=reconciliation/production", "--wait=11m", "--report=" + filepath.Join(h.applies[0].work, "status.json")}; len(h.applies) != 1 || !slices.Equal(h.applies[0].args, want) {
		t.Fatalf("engine applied %q, want %q", h.applies[0].args, want)
	}
	if requirements := h.commands[h.index(t, "infra reconcile requirements")]; requirements != "infra reconcile requirements --root="+source+" --state-bucket=llunde-pyparser-bucket --state-prefix=reconciliation/production" || h.index(t, "infra reconcile requirements") < validate {
		t.Errorf("requirements ran as %q", requirements)
	}
	if gate := h.commands[h.index(t, "supervisor reconcile provenance")]; gate != "supervisor reconcile provenance --root="+source+" --state-bucket=llunde-pyparser-bucket --state-prefix=reconciliation/production --report="+filepath.Join(h.applies[0].work, "provenance.json") {
		t.Errorf("gate ran %q", gate)
	}
	if len(h.github.unexpected) != 0 {
		t.Errorf("supervisor called GitHub beyond the runner token and token lifetime: %q", h.github.unexpected)
	}
	wantMinted := map[string][]map[string]any{
		fmt.Sprint(runnerInstall): {{"repositories": []any{"Y", "infra"}, "permissions": map[string]any{"administration": "write", "metadata": "read"}}},
	}
	if !reflect.DeepEqual(h.github.minted, wantMinted) || len(h.github.revoked) != 1 {
		t.Errorf("minted %v and revoked %v", h.github.minted, h.github.revoked)
	}
	run, log := h.uploaded(t)
	if run.Kind != "apply" || run.Outcome != OutcomeApplied || run.Revision != h.tip || run.Stage != "apply" || !strings.HasPrefix(run.Reason, "main at ") || !json.Valid(run.Status) || !json.Valid(run.Provenance) {
		t.Errorf("uploaded run %+v", run)
	}
	if !strings.Contains(log, "Reconciling main at "+h.tip) {
		t.Errorf("log lacks the revision:\n%s", log)
	}
	if want := []url.Values{{"success": {"true"}}}; !reflect.DeepEqual(h.gatus.received, want) {
		t.Errorf("heartbeats %v", h.gatus.received)
	}
	if ledger := h.ledger(t); ledger.Revision != h.tip || ledger.Outcome != OutcomeApplied || ledger.LastRepair != nil || ledger.Full || ledger.Attempts != 0 {
		t.Errorf("ledger %+v", ledger)
	}
	if entries, err := os.ReadDir(filepath.Join(h.applier.Config.State, "runs")); err != nil || len(entries) != 0 {
		t.Errorf("run directory kept %v: %v", entries, err)
	}
	if entries, err := os.ReadDir(h.runtime()); err != nil || len(entries) != 0 {
		t.Errorf("runtime directory kept %v: %v", entries, err)
	}
	decision, err := h.applier.Pending(context.Background())
	if err != nil || decision.Run {
		t.Errorf("an applied tip is still pending: %+v, %v", decision, err)
	}
}

func TestApplyMintsTheRunnerTokenOnlyWhenHostsAreSelected(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		selection reconcile.Selection
		minted    bool
	}{
		{name: "project only", selection: reconcile.Selection{Kubernetes: true, Projects: []string{"portfolio"}}},
		{name: "host playbooks", selection: reconcile.Selection{Ansible: true, HostScope: reconcile.HostScopeFull}, minted: true},
		{name: "tooling only", selection: reconcile.Selection{Tooling: true}, minted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			h.selection = test.selection
			if err := h.applier.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, minted := h.github.minted[fmt.Sprint(runnerInstall)]; minted != test.minted || len(h.applies) != 1 {
				t.Fatalf("minted %v for applies %d", h.github.minted, len(h.applies))
			}
		})
	}
}

func TestGateReportsBindTheCheckedRevision(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		report   func(h *applyHarness) (int, string)
		want     string
		terminal bool
	}{
		{name: "another revision", report: func(h *applyHarness) (int, string) {
			return 0, fmt.Sprintf(`{"base":%q,"revision":%q}`, h.applied, h.applied)
		}, want: "provenance report covers", terminal: true},
		{name: "invalid base", report: func(h *applyHarness) (int, string) {
			return 0, fmt.Sprintf(`{"base":"main","revision":%q}`, h.tip)
		}, want: `invalid base "main"`, terminal: true},
		{name: "rejected commit", report: func(h *applyHarness) (int, string) {
			return 1, fmt.Sprintf(`{"base":%q,"revision":%q,"error":"commit %s is not SSH-signed by an administrator"}`, h.applied, h.tip, h.tip[:12])
		}, want: "is not SSH-signed", terminal: true},
		{name: "state unreadable", report: func(h *applyHarness) (int, string) {
			return 1, `{"base":"","revision":"","error":"read reconciliation status: 503 Slow Down"}`
		}, want: "503 Slow Down"},
		{name: "gate crashed", report: func(h *applyHarness) (int, string) { return -1, "" }, want: "no such file"},
		{name: "attestation outage", report: func(h *applyHarness) (int, string) {
			return 1, fmt.Sprintf(`{"base":%q,"revision":%q,"error":"unverified commits: deploy: source unavailable: HTTP 502","unavailable":true}`, h.applied, h.tip)
		}, want: "source unavailable: HTTP 502"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			h.gate = func(t *testing.T, call engineCall) (int, string) { return test.report(h) }
			err := h.applier.Apply(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("apply returned %v, want %q", err, test.want)
			}
			if h.index(t, "go ") >= 0 || h.index(t, "infra ") >= 0 || h.index(t, "uv ") >= 0 {
				t.Fatalf("checkout tooling ran after a failed gate: %q", h.commands)
			}
			run, _ := h.uploaded(t)
			want := OutcomeRetry
			if test.terminal {
				want = reconcile.OutcomeFailed
			}
			if run.Outcome != want || run.Stage != "provenance" || h.ledger(t).Outcome != want || len(h.github.unexpected) != 0 {
				t.Errorf("uploaded run %+v, GitHub calls %q", run, h.github.unexpected)
			}
			if len(h.gatus.received) != 1 || h.gatus.received[0].Get("success") != "false" || !strings.Contains(h.gatus.received[0].Get("error"), test.want) {
				t.Errorf("heartbeats %v", h.gatus.received)
			}
			decision, err := h.applier.Pending(context.Background())
			if err != nil || decision.Run == test.terminal {
				t.Errorf("after a %s gate failure pending decided %+v, %v", map[bool]string{true: "terminal", false: "transient"}[test.terminal], decision, err)
			}
		})
	}
}

func TestGateOutagesRetryABoundedNumberOfTimes(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	h.gate = func(t *testing.T, call engineCall) (int, string) {
		return 1, fmt.Sprintf(`{"base":%q,"revision":%q,"error":"unverified commits: deploy: source unavailable: HTTP 503","unavailable":true}`, h.applied, h.tip)
	}
	now := time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC)
	for attempt := 1; attempt <= gateOutageRetries+1; attempt++ {
		h.reset(t)
		h.applier.Now = func() time.Time { return now }
		if err := h.applier.Apply(context.Background()); err == nil {
			t.Fatalf("attempt %d succeeded", attempt)
		}
		ledger := h.ledger(t)
		want := OutcomeRetry
		if attempt > gateOutageRetries {
			want = reconcile.OutcomeFailed
		}
		if ledger.Outcome != want || ledger.Attempts != attempt && want == OutcomeRetry {
			t.Fatalf("attempt %d left %+v, want %s", attempt, ledger, want)
		}
		if h.index(t, "go ") >= 0 {
			t.Fatalf("attempt %d built after an unverified gate: %q", attempt, h.commands)
		}
		now = ledger.RetryAt
		if want == reconcile.OutcomeFailed {
			decision, err := h.applier.Pending(context.Background())
			if err != nil || decision.Run {
				t.Fatalf("a settled outage still retries: %+v, %v", decision, err)
			}
		}
	}
}

func TestTransientFailuresRetryAndDeterministicFailuresWait(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		prepare  func(h *applyHarness)
		stage    string
		terminal bool
	}{
		{name: "GitHub unreachable during checkout", stage: "checkout", prepare: func(h *applyHarness) {
			h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) {
				if name == "fetch" {
					return process.Result{ExitCode: 128}, errors.New("fatal: unable to access 'https://github.com/fredrir/infra.git/': Could not resolve host: github.com")
				}
				return process.Result{}, nil
			}
		}},
		{name: "module proxy down", stage: "build", prepare: func(h *applyHarness) {
			h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) {
				if name == "build" {
					return process.Result{ExitCode: 1}, errors.New("proxy.golang.org: 502")
				}
				return process.Result{}, nil
			}
		}},
		{name: "tool download", stage: "tools", prepare: func(h *applyHarness) {
			h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) {
				if name == "tools" {
					return process.Result{ExitCode: 1}, errors.New("download tofu: 503")
				}
				return process.Result{}, nil
			}
		}},
		{name: "gate tool download", stage: "provenance", prepare: func(h *applyHarness) {
			h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) {
				if name == "gate-tools" {
					return process.Result{ExitCode: 1}, errors.New("download cosign: 503")
				}
				return process.Result{}, nil
			}
		}},
		{name: "PyPI down", stage: "tools", prepare: func(h *applyHarness) {
			h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) {
				if name == "uv" {
					return process.Result{ExitCode: 2}, errors.New("pypi: timeout")
				}
				return process.Result{}, nil
			}
		}},
		{name: "provider download during validation", stage: "validate", prepare: func(h *applyHarness) {
			h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) {
				if name == "prepare-validation" {
					return process.Result{ExitCode: 1}, errors.New("registry.opentofu.org: 503")
				}
				return process.Result{}, nil
			}
		}},
		{name: "state unreadable for requirements", stage: "requirements", prepare: func(h *applyHarness) {
			h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) {
				if name == "requirements" {
					return process.Result{ExitCode: 1}, errors.New("s3: 503")
				}
				return process.Result{}, nil
			}
		}},
		{name: "runner token", stage: "credentials", prepare: func(h *applyHarness) { h.github.failMint = true }},
		{name: "missing credential", stage: "credentials", prepare: func(h *applyHarness) { delete(h.credentials, KubernetesToken) }},
		{name: "engine killed", stage: "apply", prepare: func(h *applyHarness) {
			h.engine = func(t *testing.T, call engineCall) (int, string) { return -1, "" }
		}},
		{name: "invalid declarations", stage: "validate", terminal: true, prepare: func(h *applyHarness) {
			h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) {
				if name == "validate" {
					return process.Result{ExitCode: 1}, errors.New("tofu validate failed")
				}
				return process.Result{}, nil
			}
		}},
		{name: "engine failure", stage: "apply", terminal: true, prepare: func(h *applyHarness) {
			h.engine = func(t *testing.T, call engineCall) (int, string) {
				return 1, fmt.Sprintf(`{"desired_revision":%q,"applied_revision":%q,"stage":"hosts","failure":"hosts: fredrir-09 unreachable"}`, h.tip, h.applied)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			test.prepare(h)
			if test.name == "missing credential" {
				h.writeCredentials(t)
			}
			if err := h.applier.Apply(context.Background()); err == nil {
				t.Fatal("a failed run returned no error")
			}
			ledger := h.ledger(t)
			want := OutcomeRetry
			if test.terminal {
				want = reconcile.OutcomeFailed
			}
			if ledger.Outcome != want || ledger.Revision != h.tip {
				t.Fatalf("ledger %+v, want %s", ledger, want)
			}
			if run, _ := h.uploaded(t); run.Stage != test.stage {
				t.Errorf("failed at %s, want %s", run.Stage, test.stage)
			}
			if len(h.github.unexpected) != 0 {
				t.Errorf("GitHub calls %q", h.github.unexpected)
			}
			if test.name != "missing credential" && (len(h.gatus.received) != 1 || h.gatus.received[0].Get("success") != "false") {
				t.Errorf("heartbeats %v", h.gatus.received)
			}
			decision, err := h.applier.Pending(context.Background())
			if err != nil || decision.Run == test.terminal {
				t.Fatalf("pending decided %+v, %v", decision, err)
			}
			if test.terminal {
				return
			}
			h.reset(t)
			h.applier.Now = func() time.Time { return time.Date(2026, 9, 26, 3, 0, 1, 0, time.UTC) }
			h.step = func(t *testing.T, name string, call engineCall) (process.Result, error) { return process.Result{}, nil }
			h.github.failMint = false
			h.credentials = applyCredentialValues(t)
			h.writeCredentials(t)
			h.engine = func(t *testing.T, call engineCall) (int, string) {
				return 0, fmt.Sprintf(`{"desired_revision":%q,"applied_revision":%q,"stage":"complete"}`, h.tip, h.tip)
			}
			if err := h.applier.Apply(context.Background()); err != nil {
				t.Fatalf("the retry failed: %v", err)
			}
			if ledger := h.ledger(t); ledger.Outcome != OutcomeApplied || ledger.Attempts != 0 {
				t.Errorf("the retry left %+v", ledger)
			}
		})
	}
}

func TestACrashedRunIsRetriedWithItsIntent(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	h.settle(t, Ledger{Revision: h.tip, Outcome: OutcomeRunning, Full: true, Repair: true, Attempts: 1, RetryAt: h.applier.Now(), Started: h.applier.Now().Add(-time.Hour), Checked: h.applier.Now().Add(-time.Hour)})
	if err := h.applier.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.applies) != 1 || !slices.Contains(h.applies[0].args, "--full") {
		t.Fatalf("the retry applied %v", h.applies)
	}
	if ledger := h.ledger(t); ledger.Outcome != OutcomeApplied || ledger.LastRepair == nil || ledger.LastRepair.Revision != h.tip {
		t.Errorf("ledger %+v", ledger)
	}
}

func TestADeferredRepairKeepsFullAndDoesNotSpendTheCap(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	h.settle(t, Ledger{Revision: h.tip, Outcome: OutcomeApplied, Checked: h.applier.Now()})
	if err := WriteRequest(h.applier.Config.Shared, Request{Kind: RequestRepair, Revision: h.tip, Full: true, Reason: "1 differences: opentofu: drift", Requested: h.applier.Now()}); err != nil {
		t.Fatal(err)
	}
	h.engine = func(t *testing.T, call engineCall) (int, string) { return exitRetry, "" }
	if err := h.applier.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ledger := h.ledger(t); ledger.Outcome != OutcomeDeferred || !ledger.Full || !ledger.Repair || ledger.LastRepair != nil {
		t.Fatalf("deferred repair left %+v", ledger)
	}
	h.reset(t)
	h.applier.Now = func() time.Time { return time.Date(2026, 9, 26, 3, 1, 0, 0, time.UTC) }
	h.engine = func(t *testing.T, call engineCall) (int, string) {
		return 0, fmt.Sprintf(`{"desired_revision":%q,"applied_revision":%q,"stage":"complete"}`, h.tip, h.tip)
	}
	if err := h.applier.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.applies) != 1 || !slices.Contains(h.applies[0].args, "--full") {
		t.Fatalf("the retried repair applied %v", h.applies)
	}
	if ledger := h.ledger(t); ledger.LastRepair == nil || ledger.LastRepair.Outcome != OutcomeApplied || ledger.Full {
		t.Errorf("ledger %+v", ledger)
	}
}

func TestApplyDefersOrYieldsWhenTheEngineAsksForARetry(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		advance bool
		want    string
	}{
		{name: "lease held", want: OutcomeDeferred},
		{name: "main advanced", advance: true, want: OutcomeSuperseded},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			h.engine = func(t *testing.T, call engineCall) (int, string) {
				if test.advance {
					h.commit(t, map[string]string{"next": "next"})
				}
				return exitRetry, ""
			}
			if err := h.applier.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			if run, _ := h.uploaded(t); run.Outcome != test.want {
				t.Errorf("uploaded run %+v", run)
			}
			if len(h.gatus.received) != 0 || len(h.github.unexpected) != 0 {
				t.Errorf("heartbeats %v and GitHub calls %q", h.gatus.received, h.github.unexpected)
			}
			decision, err := h.applier.Pending(context.Background())
			if err != nil || !decision.Run || !decision.Apply {
				t.Errorf("the retry is not pending: %+v, %v", decision, err)
			}
		})
	}
}

func TestPushIgnoredCommitsAreSkippedOnlyAfterASettledAncestor(t *testing.T) {
	t.Parallel()
	documents := map[string]string{"docs/runbook.md": "runbook", "README.md": "readme", "build/evidence/run.json": "{}"}
	t.Run("settled ancestor", func(t *testing.T) {
		t.Parallel()
		h := newApplyHarness(t)
		h.settle(t, Ledger{Revision: h.tip, Outcome: OutcomeApplied, Checked: h.applier.Now()})
		documented := h.commit(t, documents)
		if err := h.applier.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		if h.index(t, "supervisor ") >= 0 || h.index(t, "go ") >= 0 || len(h.github.unexpected) != 0 || len(h.gatus.received) != 0 || len(h.bucket.puts) != 0 {
			t.Fatalf("a push-ignored commit ran %q", h.commands)
		}
		if ledger := h.ledger(t); ledger.Revision != documented || ledger.Outcome != OutcomeApplied {
			t.Errorf("ledger %+v", ledger)
		}
		h.reset(t)
		h.tip = h.commit(t, map[string]string{"tofu/main.tf": "# change"})
		if err := h.applier.Apply(context.Background()); err != nil || len(h.applies) != 1 {
			t.Fatalf("a declaration change did not apply: %v %q", err, h.commands)
		}
	})
	for _, outcome := range []string{OutcomeDeferred, OutcomeSuperseded, OutcomeRetry, OutcomeRunning} {
		t.Run("unsettled "+outcome, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			h.settle(t, Ledger{Revision: h.tip, Outcome: outcome, Checked: h.applier.Now()})
			h.tip = h.commit(t, documents)
			if err := h.applier.Apply(context.Background()); err != nil || len(h.applies) != 1 {
				t.Fatalf("an unapplied revision was skipped behind a push-ignored commit: %v %q", err, h.commands)
			}
		})
	}
	t.Run("unrelated history", func(t *testing.T) {
		t.Parallel()
		h := newApplyHarness(t)
		gitCommand(t, h.origin, "checkout", "--quiet", "--orphan", "rewritten")
		gitCommand(t, h.origin, "commit", "--quiet", "--no-gpg-sign", "-m", "rewritten")
		rewritten := branchTip(t, h.origin, "rewritten")
		gitCommand(t, h.origin, "checkout", "--quiet", "main")
		h.settle(t, Ledger{Revision: rewritten, Outcome: OutcomeApplied, Checked: h.applier.Now()})
		if err := h.applier.Apply(context.Background()); err != nil || len(h.applies) != 1 {
			t.Fatalf("a tip that does not descend from the handled revision was skipped: %v %q", err, h.commands)
		}
	})
	t.Run("failed revision keeps the blame", func(t *testing.T) {
		t.Parallel()
		h := newApplyHarness(t)
		failed := "reconciliation of " + h.tip + " failed at apply: hosts: unreachable"
		h.settle(t, Ledger{Revision: h.tip, Outcome: reconcile.OutcomeFailed, Failure: failed, Checked: h.applier.Now()})
		h.commit(t, documents)
		if err := h.applier.Apply(context.Background()); err != nil || len(h.applies) != 0 {
			t.Fatalf("a push-ignored commit ran after a failure: %v %q", err, h.commands)
		}
		if ledger := h.ledger(t); ledger.Failure != failed || ledger.Outcome != reconcile.OutcomeFailed {
			t.Errorf("ledger %+v", ledger)
		}
	})
}

func TestReadinessReportsTheLedgerAndExpiringCredentialsDaily(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		expiration string
		outcome    string
		want       url.Values
	}{
		{name: "ready", expiration: "2027-09-26 12:00:00 UTC", outcome: OutcomeApplied, want: url.Values{"success": {"true"}}},
		{name: "offset expiry", expiration: "2027-09-26 12:00:00 +0200", outcome: OutcomeApplied, want: url.Values{"success": {"true"}}},
		{name: "expiring token", expiration: "2026-10-10 12:00:00 UTC", outcome: OutcomeApplied, want: url.Values{"success": {"false"}, "error": {"provenance token: expires 2026-10-10"}}},
		{name: "token without expiry", outcome: OutcomeApplied, want: url.Values{"success": {"true"}}},
		{name: "failed tip", expiration: "2027-09-26 12:00:00 UTC", outcome: reconcile.OutcomeFailed, want: url.Values{"success": {"false"}, "error": {"reconciliation failed"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			h.github.expiration = test.expiration
			h.settle(t, Ledger{Revision: h.tip, Outcome: test.outcome, Failure: "reconciliation failed", Checked: h.applier.Now().Add(-readinessInterval)})
			err := h.applier.Apply(context.Background())
			if (err == nil) != (test.want.Get("success") == "true") {
				t.Fatalf("readiness returned %v", err)
			}
			if len(h.applies) != 0 || h.index(t, "go ") >= 0 || len(h.github.unexpected) != 0 {
				t.Fatalf("readiness reconciled: %q", h.commands)
			}
			if !reflect.DeepEqual(h.gatus.received, []url.Values{test.want}) {
				t.Errorf("heartbeats %v, want %v", h.gatus.received, test.want)
			}
			if ledger := h.ledger(t); !ledger.Checked.Equal(h.applier.Now()) || ledger.Outcome != test.outcome {
				t.Errorf("ledger %+v", ledger)
			}
		})
	}
}

func TestInvalidRequestsAreQuarantinedOnceWithAFailingHeartbeat(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	h.settle(t, Ledger{Revision: h.tip, Outcome: OutcomeApplied, Checked: h.applier.Now()})
	if err := os.WriteFile(requestPath(h.applier.Config.Shared, RequestApply), []byte(`{"kind":"apply","full":tr`), 0o640); err != nil {
		t.Fatal(err)
	}
	decision, err := h.applier.Pending(context.Background())
	if err != nil || !decision.Run || decision.Apply || decision.Reason != "quarantine an invalid apply request" {
		t.Fatalf("pending decided %+v, %v", decision, err)
	}
	var journal strings.Builder
	h.applier.Log = &journal
	if err := h.applier.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "apply request: unexpected EOF") {
		t.Fatalf("apply returned %v", err)
	}
	if !strings.Contains(journal.String(), "quarantined ") || len(h.applies) != 0 {
		t.Errorf("journal %q, applies %d", journal.String(), len(h.applies))
	}
	if len(h.gatus.received) != 1 || h.gatus.received[0].Get("success") != "false" || !strings.Contains(h.gatus.received[0].Get("error"), "apply request: unexpected EOF") {
		t.Errorf("heartbeats %v", h.gatus.received)
	}
	if ledger := h.ledger(t); len(ledger.Quarantined[RequestApply]) != 64 || ledger.Outcome != OutcomeApplied {
		t.Errorf("ledger %+v", ledger)
	}
	if decision, err := h.applier.Pending(context.Background()); err != nil || decision.Run {
		t.Fatalf("a quarantined request is still pending: %+v, %v", decision, err)
	}
	h.reset(t)
	if err := h.applier.Apply(context.Background()); err != nil || len(h.gatus.received) != 0 {
		t.Fatalf("a quarantined request reported again: %v %v", err, h.gatus.received)
	}
	if err := WriteRequest(h.applier.Config.Shared, Request{Kind: RequestApply, Full: true, Reason: "operator", Requested: h.applier.Now()}); err != nil {
		t.Fatal(err)
	}
	h.writeCredentials(t)
	if err := h.applier.Apply(context.Background()); err != nil || len(h.applies) != 1 || !slices.Contains(h.applies[0].args, "--full") {
		t.Fatalf("a corrected request did not apply: %v %v", err, h.applies)
	}
}

func TestPendingNeedsNoCredentials(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if err := os.Remove(h.applier.Credentials); err != nil {
		t.Fatal(err)
	}
	decision, err := h.applier.Pending(context.Background())
	if err != nil || !decision.Run || decision.Reason != "main at "+h.tip {
		t.Fatalf("pending decided %+v, %v", decision, err)
	}
	if !slices.Equal(h.commands, []string{"git ls-remote --exit-code file://" + h.origin + " refs/heads/main"}) {
		t.Errorf("pending ran %q", h.commands)
	}
	if len(h.github.minted)+h.github.rateReads+len(h.bucket.puts)+len(h.gatus.received) != 0 {
		t.Error("pending reached a credentialed service")
	}
	if entries, err := os.ReadDir(h.applier.Config.State); err != nil || len(entries) != 0 {
		t.Errorf("pending wrote %v: %v", entries, err)
	}
}

func TestApplyWaitsForTheHostLock(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	release, err := acquireHostLock(context.Background(), h.applier.Config.Shared, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.applier.Apply(context.Background()) }()
	time.Sleep(30 * time.Millisecond)
	h.mu.Lock()
	started := len(h.commands)
	h.mu.Unlock()
	if started != 0 {
		t.Fatalf("apply ran %q while verification held the host", h.commands)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || len(h.applies) != 1 {
		t.Fatalf("apply after release: %v", err)
	}
}

func TestOnlyIgnoredRequiresTheHandledRevisionAsAnAncestor(t *testing.T) {
	t.Parallel()
	origin, _ := originRepository(t)
	base := branchTip(t, origin, "main")
	gitCommand(t, origin, "checkout", "--quiet", "-b", "side")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("side"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, origin, "add", ".")
	gitCommand(t, origin, "commit", "--quiet", "--no-gpg-sign", "-m", "side")
	side := branchTip(t, origin, "side")
	gitCommand(t, origin, "checkout", "--quiet", "main")
	if err := os.WriteFile(filepath.Join(origin, "CHANGELOG.md"), []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, origin, "add", ".")
	gitCommand(t, origin, "commit", "--quiet", "--no-gpg-sign", "-m", "main")
	tip := branchTip(t, origin, "main")
	hardened := hardenedExecutor(t)
	if ignored, err := hardened.onlyIgnored(context.Background(), origin, base, tip); err != nil || !ignored {
		t.Fatalf("an ancestor with push-ignored changes: %v, %v", ignored, err)
	}
	if ignored, err := hardened.onlyIgnored(context.Background(), origin, side, tip); err != nil || ignored {
		t.Fatalf("a sibling with push-ignored differences was skipped: %v, %v", ignored, err)
	}
}

func TestSecretsStayOnAMemoryBackedFileSystem(t *testing.T) {
	if err := memoryBackedFilesystem("/dev/shm"); err != nil {
		t.Skipf("no tmpfs at /dev/shm: %v", err)
	}
	disk, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if memoryBackedFilesystem(disk) == nil {
		t.Skipf("%s is memory-backed", disk)
	}
	h := newApplyHarness(t)
	memoryBacked = memoryBackedFilesystem
	t.Cleanup(func() { memoryBacked = func(string) error { return nil } })
	persistent, err := os.MkdirTemp(disk, ".credentials-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(persistent) })
	data, err := os.ReadFile(h.applier.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	h.applier.Credentials = filepath.Join(persistent, "credentials.json")
	if err := os.WriteFile(h.applier.Credentials, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.applier.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "is not on a memory-backed file system") {
		t.Fatalf("apply returned %v", err)
	}
	if len(h.applies) != 0 || h.index(t, "supervisor ") >= 0 {
		t.Fatalf("ran with secrets on disk: %q", h.commands)
	}
	if entries, err := os.ReadDir(persistent); err != nil || len(entries) != 0 {
		t.Fatalf("left %v on disk: %v", entries, err)
	}
}

func TestApplyConfigRequiresHostAccessAndApps(t *testing.T) {
	t.Parallel()
	if _, err := LoadApplyConfig(writeConfig(t, validApplyConfig())); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ApplyConfig){
		"known hosts": func(c *ApplyConfig) { c.KnownHosts = "" },
		"runner app":  func(c *ApplyConfig) { c.Runner.InstallationID = 0 },
		"shared":      func(c *ApplyConfig) { c.Shared = c.State + "/shared" },
	} {
		config := validApplyConfig()
		change(&config)
		if _, err := LoadApplyConfig(writeConfig(t, config)); err == nil {
			t.Errorf("%s: accepted %+v", name, config)
		}
	}
	if _, err := LoadApplyConfig(writeConfig(t, validConfig())); err == nil {
		t.Error("the verify configuration loaded as an apply configuration")
	}
	data, err := json.Marshal(validApplyConfig())
	if err != nil {
		t.Fatal(err)
	}
	var withPublisher map[string]any
	if err := json.Unmarshal(data, &withPublisher); err != nil {
		t.Fatal(err)
	}
	withPublisher["publisher"] = map[string]any{"app_id": 5079532, "installation_id": 164968284, "api": "https://api.github.com", "repository": "fredrir/infra"}
	if _, err := LoadApplyConfig(writeConfig(t, withPublisher)); err == nil || !strings.Contains(err.Error(), `unknown field "publisher"`) {
		t.Errorf("an apply configuration with a publisher App loaded: %v", err)
	}
}
