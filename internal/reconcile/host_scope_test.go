package reconcile

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func TestHostScopeSelection(t *testing.T) {
	for _, test := range []struct {
		name             string
		paths            []string
		scope            string
		tofu, kubernetes bool
	}{
		{"runner role", []string{"ansible/roles/build_runner/tasks/main.yml"}, HostScopeRunners, false, false},
		{"engine role", []string{"ansible/roles/build_engine/templates/infra-dagger.service.j2"}, HostScopeRunners, false, false},
		{"CLI release", []string{"build/cli-release.json"}, HostScopeRunners, false, false},
		{"runner fleet", []string{"build/runners.json"}, HostScopeRunners, false, false},
		{"engine toolchain", []string{"build/toolchain.json"}, HostScopeRunners, false, false},
		{"monitor", []string{"ansible/roles/gatus/templates/config.yml.j2"}, HostScopeMonitor, false, false},
		{"mixed hosts", []string{"ansible/roles/gatus/tasks/main.yml", "ansible/roles/build_runner/tasks/main.yml"}, HostScopeFull, false, false},
		{"settings", []string{"platform/clusters/production/settings.yaml"}, HostScopeMonitor, true, true},
		{"inventory", []string{"ansible/inventory/production.yml"}, HostScopeFull, false, false},
		{"backup dependency", []string{"platform/components/backups/secrets.sops.yaml"}, HostScopeFull, false, true},
		{"active overlay", []string{"build/rollout/flux-artifacts/cutover/projects.yaml"}, HostScopeFull, true, true},
		{"flux artifact generator", []string{"build/rollout/flux-artifacts/generate/main.go"}, HostScopeFull, true, true},
		{"consumer patch", []string{"build/rollout/llunde-frontend.patch"}, HostScopeNone, false, false},
		{"unknown build input", []string{"build/new.json"}, HostScopeFull, true, true},
		{"unknown", []string{"new-input"}, HostScopeFull, true, true},
		{"CLI source", []string{"internal/ci/tools.go"}, HostScopeNone, false, false},
		{"dev source", []string{"internal/dev/setup.go"}, HostScopeNone, false, false},
		{"test source", []string{"internal/reconcile/host_scope_test.go"}, HostScopeNone, false, false},
		{"CLI workflow", []string{".github/workflows/infra-cli.yml"}, HostScopeNone, false, false},
		{"editor settings", []string{".vscode/settings.json"}, HostScopeNone, false, false},
		{"reconciliation workflow", []string{".github/workflows/reconcile.yml"}, HostScopeNone, false, false},
		{"unknown workflow", []string{".github/workflows/unknown.yml"}, HostScopeNone, false, false},
		{"docs", []string{"docs/plans/infra-reconcile/TODO.md"}, HostScopeNone, false, false},
		{"unsafe path", []string{"docs/../secrets/a"}, HostScopeFull, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Affected(test.paths)
			if effectiveHostScope(got) != test.scope || got.Tofu != test.tofu || got.Kubernetes != test.kubernetes || len(got.Reasons) == 0 {
				t.Fatalf("incorrect scope: %+v", got)
			}
		})
	}
	selected := Affected([]string{"README.md", "platform/projects/llunde/kustomization.yaml"})
	if !reflect.DeepEqual(selected.Projects, []string{"llunde"}) {
		t.Fatalf("documentation expanded project scope: %+v", selected)
	}
}

func TestHostScopeExecutesAndVerifiesMatchingPlaybooks(t *testing.T) {
	fleet := testRunnerFleet()
	root := writeRunnerFleet(t, fleet)
	for _, test := range []struct {
		scope   string
		want    []string
		runners bool
	}{
		{HostScopeFull, []string{"reconcile.yml", "external.yml", "verify.yml verify-runners.yml"}, true},
		{HostScopeRunners, []string{"build-runners.yml", "verify-runners.yml"}, true},
		{HostScopeMonitor, []string{"external.yml --tags=gatus", "verify.yml --limit=external"}, false},
		{HostScopeNone, nil, false},
	} {
		t.Run(test.scope, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			queried := 0
			commands := Commands{Runner: ci.Runner{Dir: root, Execute: func(_ context.Context, opts process.Options) (process.Result, error) {
				mu.Lock()
				defer mu.Unlock()
				switch opts.Name {
				case "ansible-playbook":
					calls = append(calls, strings.Join(opts.Args[2:], " "))
					return process.Result{}, nil
				case "gh":
					queried++
					return runnerResponse(t, healthyRunner(fleet, queriedRepository(opts))), nil
				}
				t.Errorf("unexpected command: %s", opts.Name)
				return process.Result{}, errors.New("unexpected command")
			}}}
			plan := Plan{Affected: Selection{Ansible: test.scope != HostScopeNone, HostScope: test.scope}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for _, operation := range []func(context.Context, Plan) error{commands.Hosts, commands.Monitor, commands.VerifyHosts} {
				if err := operation(ctx, plan); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(calls, test.want) {
				t.Fatalf("scope leaked or missed verification: %v", calls)
			}
			want := 0
			if test.runners {
				want = len(fleet.Repositories)
			}
			if queried != want {
				t.Fatalf("runner fleet verification leaked or missed: %d queries", queried)
			}
		})
	}
}

func TestHostScopeStopsOnVerificationFailure(t *testing.T) {
	fleet := testRunnerFleet()
	failure := errors.New("verification failed")
	calls := 0
	commands := Commands{Runner: ci.Runner{Dir: writeRunnerFleet(t, fleet), Execute: func(context.Context, process.Options) (process.Result, error) {
		calls++
		return process.Result{}, failure
	}}}
	if err := commands.VerifyHosts(context.Background(), Plan{Affected: All()}); !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("verification failure lost: calls=%d, error=%v", calls, err)
	}
	calls = 0
	commands.Runner.Dir = t.TempDir()
	if err := commands.VerifyHosts(context.Background(), Plan{Affected: All()}); err == nil || calls != 0 {
		t.Fatalf("missing runner fleet reached host verification: calls=%d, error=%v", calls, err)
	}
	commands = Commands{Runner: ci.Runner{Dir: writeRunnerFleet(t, fleet), Execute: func(_ context.Context, opts process.Options) (process.Result, error) {
		if opts.Name != "gh" {
			return process.Result{}, nil
		}
		runner := healthyRunner(fleet, queriedRepository(opts))
		runner.Status = "offline"
		return runnerResponse(t, runner), nil
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := commands.VerifyHosts(ctx, Plan{Affected: All()}); err == nil || !strings.Contains(err.Error(), "infra-build-09-infra is offline") {
		t.Fatalf("runner fleet drift passed host verification: %v", err)
	}
}

func TestHostPlanValidatesRunnerFleet(t *testing.T) {
	commands := Commands{Runner: ci.Runner{Dir: t.TempDir(), Execute: func(context.Context, process.Options) (process.Result, error) {
		return process.Result{}, nil
	}}}
	for _, scope := range []string{HostScopeFull, HostScopeRunners} {
		if err := commands.PlanHosts(context.Background(), Plan{Affected: Selection{Ansible: true, HostScope: scope}}); err == nil {
			t.Fatalf("%s plan accepted a missing runner fleet", scope)
		}
	}
	if err := commands.PlanHosts(context.Background(), Plan{Affected: Selection{Ansible: true, HostScope: HostScopeMonitor}}); err != nil {
		t.Fatalf("monitor plan required the runner fleet: %v", err)
	}
}

func TestHostCheckpointScopeCompatibility(t *testing.T) {
	for _, scope := range []string{"", HostScopeNone, HostScopeMonitor, HostScopeRunners, "unknown"} {
		status := hostCheckpoint()
		status.HostScope = scope
		if reusableHosts(status, time.Now()) {
			t.Fatalf("reused incomplete %q scope", scope)
		}
	}
	status := hostCheckpoint()
	delete(status.Durations, "hosts")
	status.Durations["hosts-runners"] = 1
	if reusableHosts(status, time.Now()) || status.Durations["hosts"] > 0 {
		t.Fatal("subset work passed full or legacy duration proof")
	}
}

func TestTrackedReconciliationInputsAreClassified(t *testing.T) {
	files, err := exec.Command("git", "ls-files", "-z", "--full-name", "--", ":/").Output()
	if err != nil {
		t.Skip("requires a Git checkout")
	}
	for _, path := range strings.Split(strings.TrimSuffix(string(files), "\x00"), "\x00") {
		selected := Affected([]string{path})
		if len(selected.Reasons) == 0 || strings.HasPrefix(selected.Reasons[0], "unclassified input:") {
			t.Errorf("classify tracked input %q explicitly", path)
		}
	}
	unknown := Affected([]string{"new-runtime-root/input.json"})
	if !unknown.Tofu || !unknown.Kubernetes || effectiveHostScope(unknown) != HostScopeFull {
		t.Fatal("unclassified input did not select conservative full mode")
	}
}
