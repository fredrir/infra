package reconcile

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

type memoryStore struct {
	status           Status
	locked, released bool
	fail             string
}

func (s *memoryStore) Read(context.Context) (Status, error) { return s.status, nil }
func (s *memoryStore) Write(_ context.Context, status Status) error {
	if status.Stage == s.fail {
		return errors.New("state unavailable")
	}
	s.status = status
	return nil
}
func (s *memoryStore) Lock(ctx context.Context) (context.Context, func() error, error) {
	if s.locked {
		return nil, nil, ErrLocked{Owner: "another run", Expires: time.Now().Add(time.Minute)}
	}
	s.locked = true
	return ctx, func() error { s.released = true; return nil }, nil
}

type fakeOps struct {
	calls      []string
	fail, base string
	failure    error
	full       bool
	selection  Selection
	drift      []Plan
	provenance []ProvenanceRange
}

func (o *fakeOps) call(name string) error {
	o.calls = append(o.calls, name)
	if name == o.fail {
		return cmp.Or(o.failure, errors.New("failed"))
	}
	return nil
}
func (o *fakeOps) Revision(context.Context) (string, error) { return "new", nil }
func (o *fakeOps) Select(_ context.Context, base string, full bool) (Selection, error) {
	o.base = base
	o.full = full
	return o.selection, nil
}
func (o *fakeOps) Provenance(_ context.Context, checked ProvenanceRange) error {
	o.provenance = append(o.provenance, checked)
	if o.fail == "provenance" {
		return cmp.Or(o.failure, errors.New("unverified commits"))
	}
	return nil
}
func (o *fakeOps) Preflight(_ context.Context, plan Plan) (Plan, error) { return plan, nil }
func (o *fakeOps) Plan(context.Context, Plan) error                     { return o.call("plan") }
func (o *fakeOps) Expand(context.Context, Plan) error                   { return o.call("expand") }
func (o *fakeOps) Hosts(context.Context, Plan) error                    { return o.call("hosts") }
func (o *fakeOps) ExpansionUnchanged(context.Context) (bool, error)     { return false, nil }
func (o *fakeOps) Publish(context.Context, string) error                { return o.call("publish") }
func (o *fakeOps) Kubernetes(context.Context, Plan) error               { return o.call("kubernetes") }
func (o *fakeOps) Monitor(context.Context, Plan) error                  { return o.call("monitor") }
func (o *fakeOps) Verify(context.Context, Plan) error                   { return o.call("verify") }
func (o *fakeOps) Retire(context.Context, Plan) error                   { return o.call("retire") }
func (o *fakeOps) VerifyDrift(_ context.Context, plan Plan) error {
	o.drift = append(o.drift, plan)
	return o.call("drift-verification")
}

func TestTransitionRetiresOnlyAfterVerification(t *testing.T) {
	want := []string{"plan", "expand", "hosts", "publish", "kubernetes", "monitor", "verify", "retire"}
	for _, fail := range append([]string{""}, want...) {
		t.Run("failure_"+fail, func(t *testing.T) {
			store := &memoryStore{status: Status{Desired: "old", Applied: "old"}}
			ops := &fakeOps{fail: fail, selection: All()}
			err := (Reconciler{Store: store, Ops: ops, Host: "logs.fredrir.com"}).Apply(context.Background(), false)
			if (err != nil) != (fail != "") {
				t.Fatalf("unexpected error: %v", err)
			}
			if !store.released {
				t.Fatal("lock not released")
			}
			if ops.base != "old" {
				t.Fatalf("comparison base: %s", ops.base)
			}
			if store.status.Desired != "new" {
				t.Fatal("desired revision not recorded")
			}
			if fail == "" {
				if !reflect.DeepEqual(ops.calls, want) || store.status.Applied != "new" || store.status.Stage != "complete" {
					t.Fatalf("incomplete: %+v %+v", ops.calls, store.status)
				}
			} else {
				if store.status.Applied != "old" || store.status.Failure == "" {
					t.Fatalf("failure lost: %+v", store.status)
				}
				for index, name := range want {
					if name == fail && !reflect.DeepEqual(ops.calls, want[:index+1]) {
						t.Fatalf("continued after failure: %v", ops.calls)
					}
				}
			}
		})
	}
}

func TestRecoveryUsesLastSuccessfulRevisionAndForcesAllSystems(t *testing.T) {
	for _, full := range []bool{false, true} {
		store := &memoryStore{status: Status{Desired: "failed", Applied: "old"}}
		ops := &fakeOps{selection: All()}
		if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), full); err != nil {
			t.Fatal(err)
		}
		if ops.base != "old" || !ops.full {
			t.Fatalf("recovery missed changes: %+v", ops)
		}
	}
}

func TestStateFailurePreventsSideEffects(t *testing.T) {
	store := &memoryStore{fail: "plan"}
	ops := &fakeOps{}
	if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); err == nil {
		t.Fatal("expected state failure")
	}
	if len(ops.calls) != 0 {
		t.Fatalf("side effects before state save: %v", ops.calls)
	}
}

func TestConcurrentApplyDoesNotExecute(t *testing.T) {
	store := &memoryStore{locked: true}
	ops := &fakeOps{}
	if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); err == nil {
		t.Fatal("expected lock failure")
	}
	if len(ops.calls) != 0 {
		t.Fatal("concurrent apply executed")
	}
}

type losingStore struct {
	memoryStore
	lose context.CancelCauseFunc
}

func (s *losingStore) Lock(ctx context.Context) (context.Context, func() error, error) {
	held, lose := context.WithCancelCause(ctx)
	s.lose = lose
	return held, func() error { lose(nil); s.released = true; return nil }, nil
}

type leaseLosingOps struct {
	fakeOps
	store *losingStore
}

func (o *leaseLosingOps) Hosts(ctx context.Context, _ Plan) error {
	o.calls = append(o.calls, "hosts")
	o.store.lose(fmt.Errorf("%w: taken over", errLeaseLost))
	return ctx.Err()
}

func TestLostLeaseStopsTheRunWithoutRecordingState(t *testing.T) {
	store := &losingStore{memoryStore: memoryStore{status: Status{Desired: "old", Applied: "old"}}}
	ops := &leaseLosingOps{fakeOps: fakeOps{selection: All()}, store: store}
	err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false)
	if !errors.Is(err, errLeaseLost) || !strings.HasPrefix(err.Error(), "hosts: reconciliation lease lost") {
		t.Fatalf("lost lease reported as %v", err)
	}
	if !reflect.DeepEqual(ops.calls, []string{"plan", "expand", "hosts"}) {
		t.Fatalf("run continued after losing its lease: %v", ops.calls)
	}
	if store.status.Stage != "hosts" || store.status.Failure != "" || !store.released {
		t.Fatalf("status written after losing the lease: %+v", store.status)
	}
	if Retryable(err) {
		t.Fatal("lost lease classified as retryable")
	}
}

type supersededOps struct{ fakeOps }

func (o *supersededOps) Revision(context.Context) (string, error) { return "", ErrSuperseded }

type unrecordedFailureStore struct{ memoryStore }

func (s *unrecordedFailureStore) Write(ctx context.Context, status Status) error {
	if status.Failure != "" {
		return errors.New("state unavailable")
	}
	return s.memoryStore.Write(ctx, status)
}

func TestApplyClassifiesRetryableOutcomes(t *testing.T) {
	for _, test := range []struct {
		name      string
		store     Store
		ops       Operations
		retryable bool
	}{
		{name: "locked", store: &memoryStore{locked: true}, ops: &fakeOps{}, retryable: true},
		{name: "superseded at start", store: &memoryStore{}, ops: &supersededOps{}, retryable: true},
		{name: "superseded at publish", store: &memoryStore{status: Status{Desired: "old", Applied: "old"}}, ops: &fakeOps{selection: All(), fail: "publish", failure: ErrSuperseded}, retryable: true},
		{name: "superseded without recorded failure", store: &unrecordedFailureStore{memoryStore{status: Status{Desired: "old", Applied: "old"}}}, ops: &fakeOps{selection: All(), fail: "publish", failure: ErrSuperseded}},
		{name: "failed", store: &memoryStore{status: Status{Desired: "old", Applied: "old"}}, ops: &fakeOps{selection: All(), fail: "hosts"}},
		{name: "complete", store: &memoryStore{status: Status{Desired: "old", Applied: "old"}}, ops: &fakeOps{selection: All()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (Reconciler{Store: test.store, Ops: test.ops}).Apply(context.Background(), false)
			if Retryable(err) != test.retryable {
				t.Fatalf("%v classified retryable=%t", err, Retryable(err))
			}
		})
	}
}

func TestMainAdvanceSupersedesOnlyReconciledChanges(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	area := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...)
		command.Dir = dir
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	origin, pusher, checkout := filepath.Join(area, "origin.git"), filepath.Join(area, "pusher"), filepath.Join(area, "checkout")
	git(area, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	git(area, "init", "--quiet", "--initial-branch=main", pusher)
	commit := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(pusher, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pusher, path), []byte(path+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(pusher, "add", "--", path)
		git(pusher, "commit", "--quiet", "--message", path)
		git(pusher, "push", "--quiet", "--force", origin, "HEAD:main")
	}
	commit("tofu/main.tf")
	git(area, "clone", "--quiet", origin, checkout)
	revision := git(checkout, "rev-parse", "HEAD")
	ops := &Commands{Runner: ci.Runner{Dir: checkout}, RequireMain: true}
	for _, test := range []struct {
		name       string
		advance    func()
		superseded bool
	}{
		{name: "current", advance: func() {}},
		{name: "root documentation", advance: func() { commit("README.md") }},
		{name: "nested documentation", advance: func() { commit("docs/runbooks/apply.md"); commit("build/evidence/run.json") }},
		{name: "documentation asset", advance: func() { commit("docs/diagram.svg") }, superseded: true},
		{name: "declaration", advance: func() { commit("README.md"); commit("platform/projects/y/kustomization.yaml") }, superseded: true},
		{name: "rewritten", advance: func() { git(pusher, "checkout", "--quiet", "--orphan", "rewritten"); commit("README.md") }, superseded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			git(pusher, "reset", "--quiet", "--hard", revision)
			test.advance()
			got, err := ops.Revision(context.Background())
			if test.superseded != errors.Is(err, ErrSuperseded) || (!test.superseded && (err != nil || got != revision)) {
				t.Fatalf("revision %q, error %v; want superseded=%t", got, err, test.superseded)
			}
		})
	}
}

func sameSelection(a, b Selection) bool {
	a.Reasons, b.Reasons = nil, nil
	a.HostScope, b.HostScope = effectiveHostScope(a), effectiveHostScope(b)
	return reflect.DeepEqual(a, b)
}

func TestAffectedCrossSystemInputs(t *testing.T) {
	for _, test := range []struct {
		path string
		want Selection
	}{
		{"platform/clusters/production/settings.yaml", Selection{Tofu: true, Kubernetes: true, Ansible: true, MonitorOnly: true}},
		{"tofu/platform-dns.tf", Selection{Tofu: true, Ansible: true}},
		{"ansible/roles/gatus/tasks/main.yml", Selection{Ansible: true, MonitorOnly: true}},
		{"platform/projects/y/application.yaml", Selection{Kubernetes: true, Projects: []string{"y"}}},
		{"build/cli-release.json", Selection{Ansible: true, HostScope: HostScopeRunners, RunnerInputs: []string{"build/cli-release.json"}}},
		{"build/runners.json", Selection{Ansible: true, HostScope: HostScopeRunners, RunnerInputs: []string{"build/runners.json"}}},
		{"build/toolchain.json", Selection{Ansible: true, HostScope: HostScopeRunners, RunnerInputs: []string{"build/toolchain.json"}}},
		{"ansible/roles/host_packages/tasks/main.yml", Selection{Ansible: true}},
		{"internal/reconcile/run.go", Selection{Tooling: true}},
		{"cmd/infra/main.go", Selection{Tooling: true}},
		{".github/workflows/deploy.yml", Selection{Tooling: true}},
		{".github/actions/setup-reconciliation/action.yml", Selection{Tooling: true}},
		{"MODULE.bazel.lock", Selection{Tooling: true}},
		{"uv.lock", All()},
		{".sops.yaml", All()},
		{"platform/components/policy/kustomization.yaml", All()},
		{"internal/../tofu/main.tf", All()},
		{"README.md", Selection{}},
	} {
		if got := Affected([]string{test.path}); !sameSelection(got, test.want) {
			t.Errorf("%s: %+v", test.path, got)
		}
	}
}

func TestAffectedNarrowsKubernetesScopeToSelectedProjects(t *testing.T) {
	narrowed := Selection{Kubernetes: true, Projects: []string{"llunde"}}
	for name, paths := range map[string][]string{
		"one project":         {"platform/projects/llunde/kustomization.yaml"},
		"nested project file": {"platform/projects/llunde/.deployments/llunde-frontend.json", "platform/projects/llunde/kustomization.yaml"},
	} {
		if got := Affected(paths); !sameSelection(got, narrowed) {
			t.Errorf("%s: %+v", name, got)
		}
	}
	for name, paths := range map[string][]string{
		"cluster manifest": {"platform/clusters/production/root.yaml"},
		"shared component": {"platform/projects/llunde/kustomization.yaml", "platform/components/cache/kustomization.yaml"},
		"charts directory": {"charts/llunde/values.yaml"},
		"projects parent":  {"platform/projects/kustomization.yaml"},
		"unrelated file":   {"README.md"},
		"empty change set": {},
	} {
		if got := Affected(paths); len(got.Projects) != 0 {
			t.Errorf("%s: narrowed %+v", name, got)
		}
	}
	if got := Affected([]string{"platform/projects/y/kustomization.yaml", "platform/projects/llunde/kustomization.yaml", "platform/projects/y/application.yaml"}); !sameSelection(got, Selection{Kubernetes: true, Projects: []string{"llunde", "y"}}) {
		t.Fatalf("multiple projects did not retain their scoped union: %+v", got)
	}
	if got := Affected([]string{"platform/projects/llunde/kustomization.yaml", "internal/reconcile/config.go"}); !sameSelection(got, Selection{Kubernetes: true, Tooling: true, Projects: []string{"llunde"}}) {
		t.Errorf("tooling change widened the project scope: %+v", got)
	}
}

func TestRetainedRoutesIncludeLegacyAndInterruptedTransition(t *testing.T) {
	state := []byte(`{"values":{"root_module":{"resources":[{"address":"cloudflare_dns_record.grafana[\"logs.fredrir.com\"]","values":{"name":"logs.fredrir.com"}}],"child_modules":[{"resources":[{"address":"module.platform_dns.cloudflare_dns_record.records[\"grafana\"]","values":{"name":"grafana.fredrir.com"}},{"address":"module.platform_dns.cloudflare_dns_record.records[\"cache\"]","values":{"name":"cache.fredrir.com"}}]}]}}}`)
	got, err := retainedHosts(state)
	if err != nil || !reflect.DeepEqual(got, []string{"grafana.fredrir.com", "logs.fredrir.com"}) {
		t.Fatalf("routes lost: %v %v", got, err)
	}
}

func TestReadinessRejectsStaleGenerationAndRevision(t *testing.T) {
	var item resource
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"monitoring","generation":2},"status":{"observedGeneration":1,"conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}}`), &item); err != nil {
		t.Fatal(err)
	}
	if ready(item) == nil {
		t.Fatal("stale Ready accepted")
	}
	item.Status.ObservedGeneration = 2
	if ready(item) == nil {
		t.Fatal("stale condition accepted")
	}
	item.Status.Conditions[0].ObservedGeneration = 2
	if err := ready(item); err != nil {
		t.Fatal(err)
	}
	c := Commands{Runner: ci.Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		return process.Result{Stdout: []byte(`{"items":[{"metadata":{"name":"flux-system","namespace":"flux-system","generation":2},"spec":{"sourceRef":{"kind":"GitRepository","name":"flux-system"}},"status":{"observedGeneration":2,"lastAppliedRevision":"production@sha1:old","conditions":[{"type":"Ready","status":"True","observedGeneration":2}]}}]}`)}, nil
	}}}
	if c.verifyKubernetes(context.Background(), "new", "") == nil {
		t.Fatal("stale revision accepted")
	}
}

func TestGrafanaChecksDatabaseAndAuthenticationWithoutRedirects(t *testing.T) {
	for _, failure := range []string{"", "database", "redirect", "auth", "root-url"} {
		t.Run(failure, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/health":
					if failure == "database" {
						fmt.Fprint(w, `{"database":"failed"}`)
					} else {
						fmt.Fprint(w, `{"database":"ok"}`)
					}
				case "/login":
					if failure == "redirect" {
						http.Redirect(w, r, "/api/health", 302)
						return
					}
					host := r.Host
					if failure == "root-url" {
						host = "old.example"
					}
					fmt.Fprintf(w, `<script>window.grafanaBootData={"settings":{"appUrl":%q}}</script>`, "http://"+host+"/")
				case "/api/user":
					if failure != "auth" {
						w.WriteHeader(401)
					}
				}
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			if err := verifyGrafana(context.Background(), client, server.URL); (err != nil) != (failure != "") {
				t.Fatalf("unexpected probe result: %v", err)
			}
		})
	}
}

func TestS3LockUsesConditionalTakeoverAndRelease(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			put, deleted := false, false
			runner := ci.Runner{Execute: func(_ context.Context, o process.Options) (process.Result, error) {
				switch o.Args[1] {
				case "get-object":
					expiry := time.Now().Add(time.Hour)
					if expired {
						expiry = time.Now().Add(-time.Hour)
					}
					body, _ := json.Marshal(lease{"previous", expiry})
					if err := os.WriteFile(o.Args[6], body, 0600); err != nil {
						t.Fatal(err)
					}
					return process.Result{Stdout: []byte(`{"ETag":"old-etag"}`)}, nil
				case "put-object":
					put = true
					if !strings.Contains(strings.Join(o.Args, " "), "--if-match old-etag") {
						t.Fatal("takeover is not conditional")
					}
					return process.Result{Stdout: []byte(`{"ETag":"new-etag"}`)}, nil
				case "delete-object":
					deleted = true
					if !strings.Contains(strings.Join(o.Args, " "), "--if-match new-etag") {
						t.Fatal("release can delete another owner")
					}
				}
				return process.Result{}, nil
			}}
			_, unlock, err := (S3Store{Runner: runner, Bucket: "bucket", Prefix: "production"}).Lock(context.Background())
			if expired {
				if err != nil {
					t.Fatal(err)
				}
				if err := unlock(); err != nil {
					t.Fatal(err)
				}
				if !put || !deleted {
					t.Fatal("expired lock not recovered")
				}
			} else if err == nil || put || deleted {
				t.Fatal("active lock stolen")
			}
		})
	}
}

func TestSavedPlanAndStateLockArePreserved(t *testing.T) {
	work := t.TempDir()
	var calls []process.Options
	c := Commands{Work: work, Runner: ci.Runner{Execute: func(_ context.Context, o process.Options) (process.Result, error) {
		calls = append(calls, o)
		return process.Result{}, nil
	}}}
	if err := c.tofuPlan(context.Background(), "expand", []string{"grafana.fredrir.com"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Expand(context.Background(), Plan{}); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(calls[0].Args, " ")
	apply := strings.Join(calls[1].Args, " ")
	if !strings.Contains(plan, "-lock-timeout=5m") || !strings.Contains(apply, "-lock-timeout=5m") || !strings.Contains(plan, "-out="+filepath.Join(work, "expand.tfplan")) || !strings.HasSuffix(apply, filepath.Join(work, "expand.tfplan")) {
		t.Fatalf("unsafe commands: %s; %s", plan, apply)
	}
	data, err := os.ReadFile(filepath.Join(work, "expand.tfvars.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "grafana.fredrir.com") {
		t.Fatal("old route lost")
	}
}

func TestHostnameOnlyChangeSkipsUnrelatedHostConfiguration(t *testing.T) {
	selected := Affected([]string{"platform/clusters/production/settings.yaml"})
	store := &memoryStore{status: Status{Desired: "old", Applied: "old"}}
	ops := &fakeOps{selection: selected}
	if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for _, call := range ops.calls {
		if call == "hosts" {
			t.Fatal("hostname change reconfigured every host")
		}
	}
	if !reflect.DeepEqual(ops.calls, []string{"plan", "expand", "publish", "kubernetes", "monitor", "verify", "retire"}) {
		t.Fatalf("incomplete hostname transition: %v", ops.calls)
	}
	combined := Affected([]string{"platform/clusters/production/settings.yaml", "ansible/roles/k3s/tasks/main.yml"})
	if combined.MonitorOnly {
		t.Fatal("host changes skipped when combined with a hostname change")
	}
}

func TestOpenTofuVerificationRejectsRemainingDrift(t *testing.T) {
	c := Commands{Runner: ci.Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		return process.Result{ExitCode: 2}, errors.New("drift")
	}}}
	if c.verifyTofu(context.Background()) == nil {
		t.Fatal("remaining drift accepted")
	}
}

func TestCancellationStopsVerificationImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	if err := poll(ctx, func() error { calls++; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if calls != 0 {
		t.Fatal("verification ran after cancellation")
	}
}

func TestS3LockCreationUsesCreateOnlyCondition(t *testing.T) {
	var conditional bool
	runner := ci.Runner{Execute: func(_ context.Context, o process.Options) (process.Result, error) {
		switch o.Args[1] {
		case "get-object":
			return process.Result{ExitCode: 1}, errors.New("missing")
		case "list-objects-v2":
			return process.Result{Stdout: []byte(`{"Contents":[]}`)}, nil
		case "put-object":
			conditional = strings.Contains(strings.Join(o.Args, " "), "--if-none-match *")
			return process.Result{Stdout: []byte(`{"ETag":"owner"}`)}, nil
		}
		return process.Result{}, nil
	}}
	_, unlock, err := (S3Store{Runner: runner, Bucket: "bucket", Prefix: "production"}).Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !conditional {
		t.Fatal("parallel initial runs can acquire the same lock")
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestHelmReleasesReconcileAlongsideKustomizations(t *testing.T) {
	var calls []string
	token, lateToken, created := "", "", false
	item := func(name, namespace, handled string) string {
		return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q,"generation":1},"spec":{"sourceRef":{"kind":"GitRepository","name":"flux-system"}},"status":{"observedGeneration":1,"lastAppliedRevision":"production@sha1:revision","lastHandledReconcileAt":%q,"inventory":{"entries":[]},"conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}}`, name, namespace, handled)
	}
	c := Commands{kubernetes: declaredRoot(t), Runner: ci.Runner{Execute: func(_ context.Context, o process.Options) (process.Result, error) {
		args := strings.Join(o.Args, " ")
		if result, ok := deployedArtifacts(t, "revision", args); ok {
			return result, nil
		}
		switch {
		case o.Name == "kubectl" && strings.HasPrefix(args, "annotate "):
			calls = append(calls, o.Args[1])
			token = strings.TrimPrefix(o.Args[len(o.Args)-2], "reconcile.fluxcd.io/requestedAt=")
			if created {
				lateToken = token
			}
		case strings.HasPrefix(args, "get gitrepository "):
			return process.Result{Stdout: []byte(`{"spec":{"ref":{"branch":"production"}},"status":{"artifact":{"revision":"production@sha1:revision"}}}`)}, nil
		case strings.HasPrefix(args, "get kustomizations.") && strings.Contains(args, "--all-namespaces"):
			calls, created = append(calls, "kustomizations ready"), true
			return process.Result{Stdout: []byte(`{"items":[` + item("flux-system", "flux-system", token) + `]}`)}, nil
		case strings.HasPrefix(args, "get kustomizations."):
			return process.Result{Stdout: []byte(`{"items":[` + item("flux-system", "flux-system", token) + `]}`)}, nil
		case strings.HasPrefix(args, "get helmreleases."):
			return process.Result{Stdout: []byte(`{"items":[` + item("monitoring", "observability", token) + "," + item("created", "observability", lateToken) + `]}`)}, nil
		}
		return process.Result{}, nil
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Kubernetes(ctx, Plan{Revision: "revision", Affected: All()}); err != nil {
		t.Fatal(err)
	}
	want := []string{"kustomizations.kustomize.toolkit.fluxcd.io,helmreleases.helm.toolkit.fluxcd.io", "kustomizations ready", "helmreleases.helm.toolkit.fluxcd.io"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("HelmRelease reconciliation order: %v", calls)
	}
}

func TestFluxBootstrapSwitchesThroughItsDeclaredRoot(t *testing.T) {
	for _, initial := range []string{"main", "production"} {
		t.Run(initial, func(t *testing.T) {
			branch, token := initial, ""
			var rootReconciles int
			c := Commands{kubernetes: declaredRoot(t), Runner: ci.Runner{Execute: func(_ context.Context, o process.Options) (process.Result, error) {
				args := strings.Join(o.Args, " ")
				if result, ok := deployedArtifacts(t, "revision", args); ok {
					return result, nil
				}
				if o.Name == "flux" && strings.HasPrefix(args, "reconcile kustomization flux-system") {
					branch = "production"
					rootReconciles++
				}
				if o.Name == "kubectl" && strings.HasPrefix(args, "annotate ") {
					if !strings.Contains(args, "--field-manager=flux-client-side-apply") {
						t.Fatal("reconciliation request can be removed by Flux")
					}
					for _, arg := range o.Args {
						if strings.HasPrefix(arg, "reconcile.fluxcd.io/requestedAt=") {
							token = strings.TrimPrefix(arg, "reconcile.fluxcd.io/requestedAt=")
						}
					}
				}
				if strings.Contains(args, "-o=jsonpath={.spec.ref.branch}") {
					return process.Result{Stdout: []byte(branch)}, nil
				}
				if strings.HasPrefix(args, "get gitrepository ") {
					return process.Result{Stdout: []byte(fmt.Sprintf(`{"spec":{"ref":{"branch":%q}},"status":{"artifact":{"revision":"production@sha1:revision"}}}`, branch))}, nil
				}
				if strings.HasPrefix(args, "get kustomizations.") || strings.HasPrefix(args, "get helmreleases.") {
					name, namespace := "flux-system", "flux-system"
					if strings.HasPrefix(args, "get helmreleases.") {
						name, namespace = "monitoring", "observability"
					}
					body := fmt.Sprintf(`{"items":[{"metadata":{"name":%q,"namespace":%q,"generation":1},"spec":{"sourceRef":{"kind":"GitRepository","name":"flux-system"}},"status":{"observedGeneration":1,"lastAppliedRevision":"production@sha1:revision","lastHandledReconcileAt":%q,"inventory":{"entries":[]},"conditions":[{"type":"Ready","status":"True","observedGeneration":1}]}}]}`, name, namespace, token)
					return process.Result{Stdout: []byte(body)}, nil
				}
				return process.Result{}, nil
			}}}
			if err := c.Kubernetes(context.Background(), Plan{Revision: "revision", Affected: All()}); err != nil {
				t.Fatal(err)
			}
			if branch != "production" || rootReconciles == 0 || token == "" {
				t.Fatal("Flux did not converge through its root")
			}
		})
	}
}

func TestOpenTofuTestsGateThePlan(t *testing.T) {
	for _, failing := range []bool{false, true} {
		var calls []string
		c := Commands{Work: t.TempDir(), Runner: ci.Runner{Execute: func(_ context.Context, o process.Options) (process.Result, error) {
			calls = append(calls, strings.Join(o.Args[1:], " "))
			switch {
			case o.Args[1] == "test" && failing:
				return process.Result{ExitCode: 1}, errors.New("boundary test failed")
			case o.Args[1] == "show":
				return process.Result{Stdout: []byte("{}")}, nil
			}
			return process.Result{}, nil
		}}}
		err := c.Plan(context.Background(), Plan{Affected: Selection{Tofu: true}})
		tested := slices.Index(calls, "test")
		planned := slices.IndexFunc(calls, func(call string) bool { return strings.HasPrefix(call, "plan") })
		switch {
		case tested < 0:
			t.Fatalf("OpenTofu tests skipped: %v", calls)
		case failing && (err == nil || planned >= 0):
			t.Fatalf("failing OpenTofu tests did not stop the plan: %v, %v", err, calls)
		case !failing && (err != nil || planned < tested):
			t.Fatalf("OpenTofu plan ran before its tests: %v, %v", err, calls)
		}
	}
}
