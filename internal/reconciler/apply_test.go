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
	"github.com/klauspost/compress/zstd"
)

const (
	supervisorBinary = "/opt/supervisor/infra"
	provenanceSecret = "ghp_provenance-secret"
	runnerInstall    = 4242
	publisherInstall = 164968284
)

type githubAPI struct {
	mu         sync.Mutex
	expiration string
	minted     map[string][]map[string]any
	revoked    []string
	checks     []string
	outputs    []map[string]any
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
		g.minted[installation] = append(g.minted[installation], decoded)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_" + installation, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
		g.revoked = append(g.revoked, authorization)
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/repos/fredrir/infra/check-runs" && r.Method == http.MethodPost && authorization == fmt.Sprintf("Bearer ghs_%d", publisherInstall):
		g.checks = append(g.checks, "create "+decoded["head_sha"].(string)+" "+decoded["status"].(string))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 77})
	case r.URL.Path == "/repos/fredrir/infra/check-runs/77" && r.Method == http.MethodPatch && authorization == fmt.Sprintf("Bearer ghs_%d", publisherInstall):
		output, _ := decoded["output"].(map[string]any)
		g.checks = append(g.checks, "complete "+decoded["conclusion"].(string))
		g.outputs = append(g.outputs, output)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 77})
	case r.URL.Path == "/rate_limit" && authorization == "Bearer "+provenanceSecret:
		g.rateReads++
		if g.expiration != "" {
			w.Header().Set("Github-Authentication-Token-Expiration", g.expiration)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
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
	gate        func(t *testing.T, call engineCall) (int, string)
	engine      func(t *testing.T, call engineCall) (int, string)
	applies     []engineCall
}

func privateKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func applyCredentialValues(t *testing.T) map[string]string {
	return map[string]string{
		AWSAccessKeyID:        "AKIAAPPLY",
		AWSSecretAccessKey:    "aws-apply-secret",
		CloudflareAPIToken:    "cloudflare-apply-secret",
		HcloudToken:           "hcloud-apply-secret",
		PlatformMailRecipient: "operator@example.net",
		KubernetesToken:       "kubernetes-apply-secret",
		RunnerAppKey:          privateKey(t),
		PublisherAppKey:       privateKey(t),
		ProvenanceToken:       provenanceSecret,
		GatusToken:            strings.Repeat("apply-gatus-", 3),
	}
}

func validApplyConfig() ApplyConfig {
	site := validConfig().Site
	site.State, site.Cache, site.Heartbeat, site.KnownHosts = "/var/lib/infra-apply", "/var/cache/infra-apply", "reconciliation_apply", "/etc/infra-reconcile/known_hosts"
	return ApplyConfig{
		Site:      site,
		Runner:    App{AppID: 4924976, InstallationID: runnerInstall, API: "https://api.github.com"},
		Publisher: Publisher{App: App{AppID: 5079532, InstallationID: publisherInstall, API: "https://api.github.com"}, Repository: "fredrir/infra"},
	}
}

func newApplyHarness(t *testing.T) *applyHarness {
	t.Helper()
	origin, applied := originRepository(t)
	h := &applyHarness{origin: origin, applied: applied, tip: gitCommand(t, origin, "rev-parse", "main"), credentials: applyCredentialValues(t), github: &githubAPI{minted: map[string][]map[string]any{}, expiration: "2027-09-26 12:00:00 UTC"}, bucket: &stateBucket{applied: applied, puts: map[string][]byte{}}, gatus: &applyHeartbeats{}}
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
	config.Runner.API, config.Publisher.API = urls["github"], urls["github"]
	h.gate = func(t *testing.T, call engineCall) (int, string) {
		return 0, fmt.Sprintf(`{"base":%q,"revision":%q}`, h.applied, h.tip)
	}
	h.engine = func(t *testing.T, call engineCall) (int, string) {
		return 0, fmt.Sprintf(`{"desired_revision":%q,"applied_revision":%q,"stage":"complete"}`, h.tip, h.tip)
	}
	h.applier = Applier{Config: config, Credentials: writeCredentials(t, anyValues(h.credentials)), Identity: identity, Self: supervisorBinary, Now: func() time.Time { return time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC) }, LockPoll: time.Millisecond, Execute: h.execute(t)}
	return h
}

func (h *applyHarness) execute(t *testing.T) func(context.Context, process.Options) (process.Result, error) {
	return func(ctx context.Context, options process.Options) (process.Result, error) {
		name := filepath.Base(options.Name)
		if options.Name == supervisorBinary {
			name = "supervisor"
		}
		h.mu.Lock()
		h.commands = append(h.commands, name+" "+strings.Join(options.Args, " "))
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
		case name == "infra" && options.Args[0] == "reconcile":
			allowed = map[string]bool{AWSAccessKeyID: true, AWSSecretAccessKey: true, CloudflareAPIToken: true, HcloudToken: true, PlatformMailRecipient: true, ProvenanceToken: true}
		}
		for _, variable := range options.Env {
			for credential, secret := range h.credentials {
				if strings.Contains(variable, strings.TrimSpace(secret)) && !allowed[credential] {
					t.Errorf("%s %s received %s in %s", name, options.Args[0], credential, strings.SplitN(variable, "=", 2)[0])
				}
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
			return process.Run(ctx, options)
		case "supervisor":
			if options.Args[0] == "ci" {
				if !slices.Contains(options.Env, "INFRA_TOOL_CACHE="+filepath.Join(call.work, "gate", "tools")) || !slices.Equal(options.Args[4:], gateTools) {
					t.Errorf("gate tools installed with %q %q", options.Args, options.Env)
				}
				return process.Result{}, nil
			}
			if !slices.Contains(options.Env, "PATH="+filepath.Join(call.work, "gate", "tools")+":/usr/local/bin:/usr/bin:/bin") {
				t.Errorf("gate runs without its own tools: %q", options.Env)
			}
			return report(h.gate(t, call))
		case "go":
			downloadModules(t, filepath.Join(call.work, "go", "mod", "cache", "download"))
			return process.Result{}, os.WriteFile(options.Args[3], []byte("engine"), 0o755)
		case "uv":
			for _, variable := range []string{"UV_PROJECT_ENVIRONMENT=" + filepath.Join(call.work, "venv"), "UV_PYTHON_DOWNLOADS=never", "UV_CACHE_DIR=" + filepath.Join(h.applier.Config.Cache, "uv")} {
				if !slices.Contains(options.Env, variable) {
					t.Errorf("uv runs without %s", variable)
				}
			}
			return process.Result{}, nil
		case "infra":
			if options.Args[0] == "ci" {
				if options.Args[1] == "install-tools" && !slices.Equal(options.Args[4:], applyTools) {
					t.Errorf("engine installed %q", options.Args[4:])
				}
				return process.Result{}, nil
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
	for name, want := range map[string]string{"GH_TOKEN": fmt.Sprintf("ghs_%d", runnerInstall), "INFRA_RECONCILE_TAILNET": "true", "PROVENANCE_TOKEN": provenanceSecret, "AWS_ENDPOINT_URL_S3": h.applier.Config.Endpoint, "TF_PLUGIN_CACHE_DIR": filepath.Join(h.applier.Config.Cache, "tofu", fmt.Sprintf("%x", sha256.Sum256([]byte(providerLock))))} {
		if environment[name] != want {
			t.Errorf("engine %s = %q, want %q", name, environment[name], want)
		}
	}
	if !strings.HasPrefix(environment["PATH"], filepath.Join(call.work, "venv", "bin")+":") {
		t.Errorf("engine PATH %q lacks the Ansible environment", environment["PATH"])
	}
	for path, want := range map[string]string{environment["PUBLISHER_APP_PRIVATE_KEY_FILE"]: strings.TrimSpace(h.credentials[PublisherAppKey]) + "\n", filepath.Join(call.work, "home", ".ssh", "known_hosts"): "fredrir-06 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA\n"} {
		data, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil || string(data) != want || info.Mode().Perm() != 0o600 {
			t.Errorf("%s does not hold the expected private content (%v, %v)", path, err, statErr)
		}
	}
	if identity, err := os.ReadFile(filepath.Join(call.work, "home", ".ssh", "id_ed25519")); err != nil || !bytes.Contains(identity, []byte("OPENSSH PRIVATE KEY")) {
		t.Errorf("SSH identity %q: %v", identity, err)
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

func TestApplyGatesTheMainTipBeforeBuildingAndPublishesThroughTheEngine(t *testing.T) {
	h := newApplyHarness(t)
	if err := h.applier.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate, build, apply := h.index(t, "supervisor reconcile provenance"), h.index(t, "go build"), h.index(t, "infra reconcile apply")
	if gate < 0 || h.index(t, "supervisor ci install-tools") > gate || gate > build || build > apply || h.index(t, "uv sync") > apply {
		t.Fatalf("commands ran in order %q", h.commands)
	}
	for _, step := range []string{"prepare-validation", "validate"} {
		if position := h.index(t, "infra ci "+step+" --before="+h.applied); position < build || position > apply {
			t.Errorf("ci %s ran at %d in %q", step, position, h.commands)
		}
	}
	source := filepath.Join(h.applies[0].work, "source")
	if want := []string{"reconcile", "apply", "--root=" + source, "--state-bucket=llunde-pyparser-bucket", "--state-prefix=reconciliation/production", "--wait=11m", "--report=" + filepath.Join(h.applies[0].work, "status.json")}; len(h.applies) != 1 || !slices.Equal(h.applies[0].args, want) {
		t.Fatalf("engine applied %q, want %q", h.applies[0].args, want)
	}
	if gate := h.commands[h.index(t, "supervisor reconcile provenance")]; gate != "supervisor reconcile provenance --root="+source+" --state-bucket=llunde-pyparser-bucket --state-prefix=reconciliation/production --report="+filepath.Join(h.applies[0].work, "provenance.json") {
		t.Errorf("gate ran %q", gate)
	}
	if want := []string{"create " + h.tip + " in_progress", "complete success"}; !slices.Equal(h.github.checks, want) || h.github.outputs[0]["title"] != "Applied "+h.tip[:12] {
		t.Errorf("check run %q %v", h.github.checks, h.github.outputs)
	}
	wantMinted := map[string][]map[string]any{
		fmt.Sprint(runnerInstall):    {{"repositories": []any{"Y", "infra"}, "permissions": map[string]any{"administration": "write", "metadata": "read"}}},
		fmt.Sprint(publisherInstall): {{"repositories": []any{"infra"}, "permissions": map[string]any{"checks": "write", "metadata": "read"}}, {"repositories": []any{"infra"}, "permissions": map[string]any{"checks": "write", "metadata": "read"}}},
	}
	if !reflect.DeepEqual(h.github.minted, wantMinted) || len(h.github.revoked) != 3 {
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
	if ledger := h.ledger(t); ledger.Revision != h.tip || ledger.Outcome != OutcomeApplied || ledger.Repair != nil {
		t.Errorf("ledger %+v", ledger)
	}
	if entries, err := os.ReadDir(filepath.Join(h.applier.Config.State, "runs")); err != nil || len(entries) != 0 {
		t.Errorf("run directory kept %v: %v", entries, err)
	}
	if _, err := os.Stat(h.applier.Credentials); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("credentials outlived the run: %v", err)
	}
	decision, err := h.applier.Pending(context.Background())
	if err != nil || decision.Run {
		t.Errorf("an applied tip is still pending: %+v, %v", decision, err)
	}
}

func TestApplyRefusesToBuildAnUnverifiedTip(t *testing.T) {
	h := newApplyHarness(t)
	h.gate = func(t *testing.T, call engineCall) (int, string) {
		return 1, fmt.Sprintf(`{"base":%q,"revision":%q,"error":"commit %s is not SSH-signed by an administrator"}`, h.applied, h.tip, h.tip[:12])
	}
	err := h.applier.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not SSH-signed") {
		t.Fatalf("apply returned %v", err)
	}
	if h.index(t, "go ") >= 0 || h.index(t, "infra ") >= 0 || h.index(t, "uv ") >= 0 {
		t.Fatalf("checkout tooling ran after a failed gate: %q", h.commands)
	}
	if run, _ := h.uploaded(t); run.Outcome != reconcile.OutcomeFailed || run.Stage != "provenance" {
		t.Errorf("uploaded run %+v", run)
	}
	if !slices.Equal(h.github.checks, []string{"create " + h.tip + " in_progress", "complete failure"}) {
		t.Errorf("check run %q", h.github.checks)
	}
	if len(h.gatus.received) != 1 || h.gatus.received[0].Get("success") != "false" || !strings.Contains(h.gatus.received[0].Get("error"), "is not SSH-signed") {
		t.Errorf("heartbeats %v", h.gatus.received)
	}
	if ledger := h.ledger(t); ledger.Outcome != reconcile.OutcomeFailed || ledger.Revision != h.tip {
		t.Errorf("ledger %+v", ledger)
	}
	if decision, err := h.applier.Pending(context.Background()); err != nil || decision.Run {
		t.Errorf("a failed tip is retried: %+v, %v", decision, err)
	}
}

func TestApplyDefersOrYieldsWhenTheEngineAsksForARetry(t *testing.T) {
	for _, test := range []struct {
		name    string
		advance bool
		want    string
	}{
		{name: "lease held", want: OutcomeDeferred},
		{name: "main advanced", advance: true, want: OutcomeSuperseded},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newApplyHarness(t)
			h.engine = func(t *testing.T, call engineCall) (int, string) {
				if test.advance {
					if err := os.WriteFile(filepath.Join(h.origin, "next"), []byte("next"), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCommand(t, h.origin, "add", ".")
					gitCommand(t, h.origin, "commit", "--quiet", "--no-gpg-sign", "-m", "next")
				}
				return exitRetry, ""
			}
			if err := h.applier.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			if run, _ := h.uploaded(t); run.Outcome != test.want {
				t.Errorf("uploaded run %+v", run)
			}
			if len(h.gatus.received) != 0 || !slices.Equal(h.github.checks, []string{"create " + h.tip + " in_progress", "complete skipped"}) {
				t.Errorf("heartbeats %v and check run %q", h.gatus.received, h.github.checks)
			}
			decision, err := h.applier.Pending(context.Background())
			if err != nil || !decision.Run || !decision.Apply {
				t.Errorf("the retry is not pending: %+v, %v", decision, err)
			}
		})
	}
}

func TestApplyFailureIsReportedOnceAndNotRetried(t *testing.T) {
	h := newApplyHarness(t)
	h.engine = func(t *testing.T, call engineCall) (int, string) {
		return 1, fmt.Sprintf(`{"desired_revision":%q,"applied_revision":%q,"stage":"hosts","failure":"hosts: fredrir-09 unreachable"}`, h.tip, h.applied)
	}
	if err := h.applier.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "fredrir-09 unreachable") {
		t.Fatalf("apply returned %v", err)
	}
	if len(h.gatus.received) != 1 || !strings.Contains(h.gatus.received[0].Get("error"), "reconciliation of "+h.tip+" failed: hosts: fredrir-09 unreachable") {
		t.Errorf("heartbeats %v", h.gatus.received)
	}
	if h.github.outputs[0]["title"] != "Failed at apply" || !strings.Contains(h.github.outputs[0]["summary"].(string), "fredrir-09 unreachable") {
		t.Errorf("check run output %v", h.github.outputs)
	}
	if decision, err := h.applier.Pending(context.Background()); err != nil || decision.Run {
		t.Errorf("a failed tip is retried: %+v, %v", decision, err)
	}
}

func TestApplyRecordsPushIgnoredCommitsWithoutRunning(t *testing.T) {
	h := newApplyHarness(t)
	if err := saveLedger(h.applier.Config.State, Ledger{Revision: h.tip, Outcome: OutcomeApplied, Checked: h.applier.Now()}); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"docs/runbook.md": "runbook", "README.md": "readme", "build/evidence/run.json": "{}"} {
		if err := os.MkdirAll(filepath.Join(h.origin, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.origin, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, h.origin, "add", ".")
	gitCommand(t, h.origin, "commit", "--quiet", "--no-gpg-sign", "-m", "document")
	documented := gitCommand(t, h.origin, "rev-parse", "main")
	if err := h.applier.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.index(t, "supervisor ") >= 0 || h.index(t, "go ") >= 0 || len(h.github.checks) != 0 || len(h.gatus.received) != 0 || len(h.bucket.puts) != 0 {
		t.Fatalf("a push-ignored commit ran %q", h.commands)
	}
	if ledger := h.ledger(t); ledger.Revision != documented || ledger.Outcome != OutcomeApplied {
		t.Errorf("ledger %+v", ledger)
	}
	if err := os.WriteFile(filepath.Join(h.origin, "tofu", "main.tf"), []byte("# change"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, h.origin, "add", ".")
	gitCommand(t, h.origin, "commit", "--quiet", "--no-gpg-sign", "-m", "change")
	h.tip = gitCommand(t, h.origin, "rev-parse", "main")
	h.applier.Credentials = writeCredentials(t, anyValues(h.credentials))
	if err := h.applier.Apply(context.Background()); err != nil || len(h.applies) != 1 {
		t.Fatalf("a declaration change did not apply: %v %q", err, h.commands)
	}
}

func TestApplyRepairsFullyOncePerRevision(t *testing.T) {
	h := newApplyHarness(t)
	if err := saveLedger(h.applier.Config.State, Ledger{Revision: h.tip, Outcome: OutcomeApplied, Checked: h.applier.Now()}); err != nil {
		t.Fatal(err)
	}
	repair := Request{Kind: RequestRepair, Revision: h.tip, Full: true, Reason: "1 differences: opentofu: drift", Requested: h.applier.Now()}
	if err := WriteRequest(h.applier.Config.Shared, repair); err != nil {
		t.Fatal(err)
	}
	if err := h.applier.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.applies) != 1 || !slices.Contains(h.applies[0].args, "--full") {
		t.Fatalf("repair applied %v", h.applies)
	}
	if ledger := h.ledger(t); ledger.Repair == nil || ledger.Repair.Revision != h.tip || ledger.Repair.Outcome != OutcomeApplied {
		t.Errorf("ledger %+v", ledger)
	}
	if _, err := os.Stat(filepath.Join(h.applier.Config.Shared, "requests", "repair.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the repair request survived: %v", err)
	}
	if err := WriteRequest(h.applier.Config.Shared, repair); err != nil {
		t.Fatal(err)
	}
	if decision, err := h.applier.Pending(context.Background()); err != nil || decision.Run {
		t.Errorf("a second repair within six hours is pending: %+v, %v", decision, err)
	}
}

func TestReadinessReportsTheLedgerAndExpiringCredentialsDaily(t *testing.T) {
	for _, test := range []struct {
		name       string
		expiration string
		outcome    string
		want       url.Values
	}{
		{name: "ready", expiration: "2027-09-26 12:00:00 UTC", outcome: OutcomeApplied, want: url.Values{"success": {"true"}}},
		{name: "offset expiry", expiration: "2027-09-26 12:00:00 +0200", outcome: OutcomeApplied, want: url.Values{"success": {"true"}}},
		{name: "expiring token", expiration: "2026-10-10 12:00:00 UTC", outcome: OutcomeApplied, want: url.Values{"success": {"false"}, "error": {"provenance token: expires 2026-10-10"}}},
		{name: "token without expiry", outcome: OutcomeApplied, want: url.Values{"success": {"false"}, "error": {"provenance token: has no expiry"}}},
		{name: "failed tip", expiration: "2027-09-26 12:00:00 UTC", outcome: reconcile.OutcomeFailed, want: url.Values{"success": {"false"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newApplyHarness(t)
			h.github.expiration = test.expiration
			if err := saveLedger(h.applier.Config.State, Ledger{Revision: h.tip, Outcome: test.outcome, Failure: "hosts: unreachable", Checked: h.applier.Now().Add(-readinessInterval)}); err != nil {
				t.Fatal(err)
			}
			err := h.applier.Apply(context.Background())
			if (err == nil) != (test.want.Get("success") == "true") {
				t.Fatalf("readiness returned %v", err)
			}
			if len(h.applies) != 0 || h.index(t, "go ") >= 0 || len(h.github.checks) != 0 {
				t.Fatalf("readiness reconciled: %q", h.commands)
			}
			got := h.gatus.received
			if test.outcome == reconcile.OutcomeFailed && len(got) == 1 && strings.Contains(got[0].Get("error"), "reconciliation of "+h.tip+" failed: hosts: unreachable") {
				got = []url.Values{{"success": got[0]["success"]}}
			}
			if !reflect.DeepEqual(got, []url.Values{test.want}) {
				t.Errorf("heartbeats %v, want %v", h.gatus.received, test.want)
			}
			if ledger := h.ledger(t); !ledger.Checked.Equal(h.applier.Now()) || ledger.Outcome != test.outcome {
				t.Errorf("ledger %+v", ledger)
			}
		})
	}
}

func TestPendingNeedsNoCredentials(t *testing.T) {
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
}

func TestApplyWaitsForTheHostLock(t *testing.T) {
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

func TestApplyConfigRequiresHostAccessAndApps(t *testing.T) {
	if _, err := LoadApplyConfig(writeConfig(t, validApplyConfig())); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ApplyConfig){
		"known hosts":          func(c *ApplyConfig) { c.KnownHosts = "" },
		"runner app":           func(c *ApplyConfig) { c.Runner.InstallationID = 0 },
		"publisher app":        func(c *ApplyConfig) { c.Publisher.AppID = 0 },
		"publisher repository": func(c *ApplyConfig) { c.Publisher.Repository = "infra" },
		"shared":               func(c *ApplyConfig) { c.Shared = c.State + "/shared" },
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
}
