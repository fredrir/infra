package reconcile

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
		{"verification trigger", []string{"ansible/roles/verification_trigger/templates/infra-verification-request.timer.j2"}, HostScopeMonitor, false, false},
		{"CLI release and verification trigger", []string{"build/cli-release.json", "ansible/roles/verification_trigger/tasks/main.yml"}, HostScopeFull, false, false},
		{"mixed hosts", []string{"ansible/roles/gatus/tasks/main.yml", "ansible/roles/build_runner/tasks/main.yml"}, HostScopeFull, false, false},
		{"settings", []string{"platform/clusters/production/settings.yaml"}, HostScopeMonitor, true, true},
		{"inventory", []string{"ansible/inventory/production.yml"}, HostScopeFull, false, false},
		{"cluster backup secret", []string{"platform/components/backups/backup.secret.sops.yaml"}, HostScopeNone, false, true},
		{"control backup secret", []string{"ansible/roles/control_backup/files/control.sops.yaml"}, HostScopeFull, false, false},
		{"monitor secret", []string{"ansible/roles/gatus/files/secrets.sops.yaml"}, HostScopeMonitor, false, false},
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
	cli := []string{"build/cli-release.json"}
	full := []string{"external.yml", "reconcile.yml", "verify-runners.yml", "verify.yml", "volatile.yml"}
	for _, test := range []struct {
		name      string
		scope     string
		inputs    []string
		playbooks []string
		want      []string
		planned   []string
		runners   bool
	}{
		{name: HostScopeFull, scope: HostScopeFull, want: full, planned: full, runners: true},
		{name: "cluster role", scope: HostScopeFull, playbooks: []string{"k3s.yml", "volatile.yml"}, want: []string{"facts.yml k3s.yml", "verify.yml", "volatile.yml"}, planned: full, runners: true},
		{name: "runner role in full scope", scope: HostScopeFull, playbooks: []string{"k3s.yml", "build-runners.yml"}, want: []string{"facts.yml k3s.yml build-runners.yml", "verify-runners.yml", "verify.yml"}, planned: full, runners: true},
		{name: "monitor and volatile playbooks", scope: HostScopeFull, playbooks: []string{"external.yml", "volatile.yml"}, want: []string{"external.yml", "verify.yml", "volatile.yml"}, planned: full, runners: true},
		{name: HostScopeRunners, scope: HostScopeRunners, want: []string{"build-runners.yml", "verify-runners.yml"}, planned: []string{"build-runners.yml", "verify-runners.yml"}, runners: true},
		{name: "CLI release", scope: HostScopeRunners, inputs: cli, want: []string{"build-runners.yml --tags=infra_binary", "external.yml --tags=infra_binary", "verify-runners.yml"}, planned: []string{"build-runners.yml", "external.yml --tags=infra_binary", "verify-runners.yml"}, runners: true},
		{name: HostScopeMonitor, scope: HostScopeMonitor, want: []string{"external.yml --tags=gatus,verification_trigger", "verify.yml --limit=external"}, planned: []string{"external.yml --tags=gatus,verification_trigger", "verify.yml --limit=external"}},
		{name: "CLI release in full scope", scope: HostScopeFull, inputs: cli, want: []string{"external.yml", "reconcile.yml", "verify-runners.yml", "verify.yml", "volatile.yml"}, planned: []string{"external.yml", "reconcile.yml", "verify-runners.yml", "verify.yml", "volatile.yml"}, runners: true},
		{name: HostScopeNone, scope: HostScopeNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			queried := 0
			commands := Commands{Runner: ci.Runner{Dir: root, Execute: func(_ context.Context, opts process.Options) (process.Result, error) {
				mu.Lock()
				defer mu.Unlock()
				switch opts.Name {
				case "ansible-playbook":
					calls = append(calls, strings.Join(opts.Args[2:], " "))
					if environment(opts, "JUNIT_OUTPUT_DIR") != "" {
						return fakePlaybooks{reports: []string{junitReport(strings.TrimSuffix(opts.Args[2], ".yml"))}}.execute(t, opts)
					}
					return process.Result{}, nil
				case "gh":
					queried++
					return runnerResponse(t, healthyRunners(fleet, queriedRepository(opts))...), nil
				}
				t.Errorf("unexpected command: %s", opts.Name)
				return process.Result{}, errors.New("unexpected command")
			}}}
			plan := Plan{Affected: Selection{Ansible: test.scope != HostScopeNone, HostScope: test.scope, HostPlaybooks: test.playbooks, RunnerInputs: test.inputs}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := commands.PlanHosts(ctx, plan); err != nil {
				t.Fatal(err)
			}
			var syntax, listed []string
			for _, call := range calls {
				syntax = append(syntax, strings.Replace(call, " --syntax-check", "", 1))
				listed = append(listed, strings.Replace(call, " --list-tasks", "", 1))
			}
			syntax = slices.DeleteFunc(syntax, func(call string) bool { return strings.Contains(call, "--list-tasks") })
			listed = slices.DeleteFunc(listed, func(call string) bool { return strings.Contains(call, "--syntax-check") })
			slices.Sort(syntax)
			slices.Sort(listed)
			if !slices.Equal(syntax, test.planned) || !slices.Equal(listed, test.planned) {
				t.Fatalf("plan checked %v", calls)
			}
			calls = nil
			for _, operation := range []func(context.Context, Plan) error{commands.Hosts, commands.Monitor, commands.VerifyHosts, commands.Volatile} {
				if err := operation(ctx, plan); err != nil {
					t.Fatal(err)
				}
			}
			slices.Sort(calls)
			if !reflect.DeepEqual(calls, test.want) {
				t.Fatalf("scope leaked or missed verification: %v", calls)
			}
			want := 0
			if test.runners {
				want = 2 * len(fleet.Repositories)
			}
			if queried != want {
				t.Fatalf("runner fleet convergence or verification leaked or missed: %d queries", queried)
			}
		})
	}
}

func TestHostVerificationReportsEveryFailure(t *testing.T) {
	fleet := testRunnerFleet()
	failure := errors.New("verification failed")
	var calls, queried atomic.Int32
	commands := Commands{Runner: ci.Runner{Dir: writeRunnerFleet(t, fleet), Execute: func(_ context.Context, opts process.Options) (process.Result, error) {
		calls.Add(1)
		if opts.Name == "gh" {
			queried.Add(1)
			return runnerResponse(t, healthyRunners(fleet, queriedRepository(opts))...), nil
		}
		return process.Result{}, failure
	}}}
	err := commands.VerifyHosts(context.Background(), Plan{Affected: All()})
	if want := []string{"verify.yml was not compared: verification failed", "verify-runners.yml was not compared: verification failed"}; !errors.Is(err, failure) || !reflect.DeepEqual(VerificationOutcome("", ScopeCloud, err).Errors, want) || int(queried.Load()) != len(fleet.Repositories) {
		t.Fatalf("host verification hid a failing part: fleet queries=%d, error=%v", queried.Load(), err)
	}
	calls.Store(0)
	commands.Runner.Dir = t.TempDir()
	if err := commands.VerifyHosts(context.Background(), Plan{Affected: All()}); err == nil || calls.Load() != 0 {
		t.Fatalf("missing runner fleet reached host verification: calls=%d, error=%v", calls.Load(), err)
	}
	commands = Commands{Runner: ci.Runner{Dir: writeRunnerFleet(t, fleet), Execute: func(_ context.Context, opts process.Options) (process.Result, error) {
		if opts.Name != "gh" {
			return process.Result{}, nil
		}
		runners := healthyRunners(fleet, queriedRepository(opts))
		for index := range runners {
			runners[index].Status = "offline"
		}
		return runnerResponse(t, runners...), nil
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := commands.VerifyHosts(ctx, Plan{Affected: All()}); err == nil || !strings.Contains(err.Error(), "infra-build-09-infra-1 is offline") {
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
