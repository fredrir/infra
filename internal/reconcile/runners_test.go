package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

func testRunnerFleet() RunnerFleet {
	return RunnerFleet{Schema: 1, Owner: "fredrir", Host: "infra-build-09", Version: "2.337.0", SHA256: strings.Repeat("a", 64), Labels: []string{"dagger-amd64", "infra-trusted"}, Repositories: []string{"infra", "Y"}}
}

func writeRunnerFleet(t *testing.T, fleet RunnerFleet) string {
	t.Helper()
	root := t.TempDir()
	data, err := json.Marshal(fleet)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "build"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build/runners.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return root
}

func healthyRunner(fleet RunnerFleet, repository string) registeredRunner {
	version := fleet.Version
	labels := []runnerLabel{{"self-hosted", "read-only"}, {"Linux", "read-only"}, {"X64", "read-only"}}
	for _, label := range fleet.Labels {
		labels = append(labels, runnerLabel{label, "custom"})
	}
	return registeredRunner{Name: fleet.Host + "-" + repository, Status: "online", Version: &version, Labels: labels}
}

func runnerResponse(t *testing.T, runners ...registeredRunner) process.Result {
	data, err := json.Marshal(map[string]any{"total_count": len(runners), "runners": runners})
	if err != nil {
		t.Error(err)
	}
	return process.Result{Stdout: data}
}

func queriedRepository(opts process.Options) string {
	return strings.Split(opts.Args[1], "/")[2]
}

type fleetHarness struct {
	t              *testing.T
	fleet          RunnerFleet
	mu             sync.Mutex
	calls          []string
	queries        int
	unreadable     func(query int) bool
	rejectLabels   bool
	failing        []string
	change         func(*registeredRunner)
	play           func()
	stdout, stderr bytes.Buffer
}

func (h *fleetHarness) commands() *Commands {
	return &Commands{Runner: ci.Runner{Dir: writeRunnerFleet(h.t, h.fleet), Execute: h.execute, Stdout: &h.stdout, Stderr: &h.stderr}}
}

func (h *fleetHarness) execute(ctx context.Context, opts process.Options) (process.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case opts.Name == "ansible-playbook":
		call := strings.Join(opts.Args[2:], " ")
		h.calls = append(h.calls, call)
		if h.play != nil {
			h.play()
		}
		if slices.Contains(h.failing, call) {
			return process.Result{ExitCode: 2}, errors.New(call + " failed")
		}
		return process.Result{}, nil
	case opts.Name == "gh" && (slices.Contains(opts.Args, "PUT") || slices.Contains(opts.Args, "DELETE")):
		h.calls = append(h.calls, "gh "+strings.Join(opts.Args, " "))
		if h.rejectLabels {
			return process.Result{ExitCode: 1}, errors.New("HTTP 403")
		}
		if ctx.Err() != nil {
			return process.Result{ExitCode: -1}, ctx.Err()
		}
		return process.Result{}, nil
	case opts.Name == "gh":
		h.queries++
		if h.unreadable != nil && h.unreadable(h.queries) {
			return process.Result{ExitCode: 1}, errors.New("HTTP 502")
		}
		runner := healthyRunner(h.fleet, queriedRepository(opts))
		if h.change != nil {
			h.change(&runner)
		}
		return runnerResponse(h.t, runner), nil
	}
	h.t.Errorf("unexpected command: %s %q", opts.Name, opts.Args)
	return process.Result{}, errors.New("unexpected command")
}

func fastRunnerReads(t *testing.T) {
	window, interval, missing := runnerStateWindow, runnerStateInterval, runnerMissingPause
	runnerStateInterval, runnerMissingPause = time.Millisecond, time.Millisecond
	t.Cleanup(func() { runnerStateWindow, runnerStateInterval, runnerMissingPause = window, interval, missing })
}

func offlineY(runner *registeredRunner) {
	if runner.Name == "infra-build-09-Y" {
		runner.Status = "offline"
	}
}

func unregisteredY(runner *registeredRunner) {
	if runner.Name == "infra-build-09-Y" {
		runner.Name = "infra-build-08-Y"
	}
}

func TestLoadRunnerFleetValidatesDeclaration(t *testing.T) {
	fleet, err := LoadRunnerFleet(writeRunnerFleet(t, testRunnerFleet()))
	if err != nil || !reflect.DeepEqual(fleet, testRunnerFleet()) {
		t.Fatalf("valid runner fleet rejected: %+v, %v", fleet, err)
	}
	for name, change := range map[string]func(*RunnerFleet){
		"schema":                func(f *RunnerFleet) { f.Schema = 2 },
		"owner":                 func(f *RunnerFleet) { f.Owner = "fredrir/infra" },
		"host":                  func(f *RunnerFleet) { f.Host = "Infra_Build" },
		"version":               func(f *RunnerFleet) { f.Version = "v2.337.0" },
		"partial version":       func(f *RunnerFleet) { f.Version = "2.337" },
		"uppercase sha256":      func(f *RunnerFleet) { f.SHA256 = strings.Repeat("A", 64) },
		"short sha256":          func(f *RunnerFleet) { f.SHA256 = strings.Repeat("a", 63) },
		"label":                 func(f *RunnerFleet) { f.Labels = []string{"dagger amd64"} },
		"no labels":             func(f *RunnerFleet) { f.Labels = nil },
		"duplicate label":       func(f *RunnerFleet) { f.Labels = []string{"infra-trusted", "infra-trusted"} },
		"no repositories":       func(f *RunnerFleet) { f.Repositories = nil },
		"duplicate repository":  func(f *RunnerFleet) { f.Repositories = []string{"infra", "Y", "infra"} },
		"traversing repository": func(f *RunnerFleet) { f.Repositories = []string{".."} },
	} {
		t.Run(name, func(t *testing.T) {
			fleet := testRunnerFleet()
			change(&fleet)
			if _, err := LoadRunnerFleet(writeRunnerFleet(t, fleet)); err == nil {
				t.Fatal("invalid runner fleet accepted")
			}
		})
	}
	valid, err := json.Marshal(testRunnerFleet())
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"unknown field":      `{"schema":1,"repository":"infra"}`,
		"trailing object":    string(valid) + "\n{}",
		"trailing delimiter": string(valid) + "}",
	} {
		root := writeRunnerFleet(t, testRunnerFleet())
		if err := os.WriteFile(filepath.Join(root, "build/runners.json"), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRunnerFleet(root); err == nil {
			t.Errorf("runner fleet with %s accepted", name)
		}
	}
}

func TestRunnerDrift(t *testing.T) {
	fleet := testRunnerFleet()
	stale := "2.300.0"
	for _, test := range []struct {
		name   string
		change func(map[string][]registeredRunner)
		want   string
	}{
		{"healthy", func(map[string][]registeredRunner) {}, ""},
		{"read-only labels ignored", func(s map[string][]registeredRunner) {
			s["Y"][0].Labels = append(s["Y"][0].Labels, runnerLabel{"ARM64", "read-only"})
		}, ""},
		{"label order ignored", func(s map[string][]registeredRunner) { slices.Reverse(s["Y"][0].Labels) }, ""},
		{"missing", func(s map[string][]registeredRunner) { delete(s, "Y") }, "fredrir/Y has 0 runners named infra-build-09-Y, want 1"},
		{"renamed", func(s map[string][]registeredRunner) { s["Y"][0].Name = "infra-build-08-Y" }, "fredrir/Y has 0 runners named infra-build-09-Y, want 1"},
		{"duplicate", func(s map[string][]registeredRunner) { s["Y"] = append(s["Y"], s["Y"][0]) }, "fredrir/Y has 2 runners named infra-build-09-Y, want 1"},
		{"offline", func(s map[string][]registeredRunner) { s["Y"][0].Status = "offline" }, "infra-build-09-Y is offline, want online"},
		{"null version", func(s map[string][]registeredRunner) { s["Y"][0].Version = nil }, "infra-build-09-Y reports no version, want 2.337.0"},
		{"stale version", func(s map[string][]registeredRunner) { s["Y"][0].Version = &stale }, "infra-build-09-Y runs 2.300.0, want 2.337.0"},
		{"missing label", func(s map[string][]registeredRunner) { s["Y"][0].Labels = s["Y"][0].Labels[:4] }, "infra-build-09-Y has labels [dagger-amd64], want [dagger-amd64 infra-trusted]"},
		{"extra label", func(s map[string][]registeredRunner) {
			s["Y"][0].Labels = append(s["Y"][0].Labels, runnerLabel{"gpu", "custom"})
		}, "infra-build-09-Y has labels [dagger-amd64 gpu infra-trusted], want [dagger-amd64 infra-trusted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			states := map[string][]registeredRunner{}
			for _, repository := range fleet.Repositories {
				states[repository] = []registeredRunner{healthyRunner(fleet, repository)}
			}
			test.change(states)
			var want []string
			if test.want != "" {
				want = []string{test.want}
			}
			if problems := runnerDrift(fleet, states); !reflect.DeepEqual(problems, want) {
				t.Fatalf("unexpected drift: %q", problems)
			}
		})
	}
}

func TestRunnerStatesQueryEachRepositoryByRunnerName(t *testing.T) {
	fleet := testRunnerFleet()
	var mu sync.Mutex
	var queries []string
	commands := Commands{Runner: ci.Runner{Execute: func(_ context.Context, opts process.Options) (process.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		queries = append(queries, opts.Name+" "+strings.Join(opts.Args, " "))
		return runnerResponse(t, healthyRunner(fleet, queriedRepository(opts))), nil
	}}}
	states, err := commands.runnerStates(context.Background(), fleet)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(queries)
	want := []string{
		"gh api repos/fredrir/Y/actions/runners?name=infra-build-09-Y",
		"gh api repos/fredrir/infra/actions/runners?name=infra-build-09-infra",
	}
	if !reflect.DeepEqual(queries, want) {
		t.Fatalf("unexpected queries: %q", queries)
	}
	if problems := runnerDrift(fleet, states); len(states) != len(fleet.Repositories) || len(problems) != 0 {
		t.Fatalf("healthy states not decoded: %+v, %q", states, problems)
	}
	failure := errors.New("HTTP 403")
	commands.Runner.Execute = func(context.Context, process.Options) (process.Result, error) {
		return process.Result{}, failure
	}
	if _, err := commands.runnerStates(context.Background(), fleet); !errors.Is(err, failure) {
		t.Fatalf("API failure lost: %v", err)
	}
}

func TestRunnerFleetVerificationFailsWhenDriftPersists(t *testing.T) {
	fleet := testRunnerFleet()
	commands := Commands{Runner: ci.Runner{Execute: func(_ context.Context, opts process.Options) (process.Result, error) {
		runner := healthyRunner(fleet, queriedRepository(opts))
		runner.Version = nil
		return runnerResponse(t, runner), nil
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := commands.verifyRunnerFleet(ctx, fleet)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "infra-build-09-Y reports no version") {
		t.Fatalf("persistent drift passed verification: %v", err)
	}
}

func TestFleetRoutingMatchesDeclaredRunners(t *testing.T) {
	root := filepath.Join("..", "..")
	fleet, err := LoadRunnerFleet(root)
	if err != nil {
		t.Fatal(err)
	}
	workflows, err := filepath.Glob(filepath.Join(root, ".github/workflows/*.yml"))
	if err != nil || len(workflows) == 0 {
		t.Fatalf("workflows unavailable: %v", err)
	}
	var labels []string
	for _, label := range fleet.Labels {
		labels = append(labels, regexp.QuoteMeta(label))
	}
	label := regexp.MustCompile(`(^|[^A-Za-z0-9_-])(` + strings.Join(labels, "|") + `)($|[^A-Za-z0-9_-])`)
	repository := regexp.MustCompile(regexp.QuoteMeta(fleet.Owner) + `/([A-Za-z0-9_.-]+)`)
	var routed []string
	for _, path := range workflows {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			Jobs map[string]struct {
				RunsOn any `yaml:"runs-on"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for name, job := range workflow.Jobs {
			runsOn := fmt.Sprint(job.RunsOn)
			if !label.MatchString(runsOn) {
				continue
			}
			matches := repository.FindAllStringSubmatch(runsOn, -1)
			if len(matches) == 0 {
				t.Errorf("%s job %s routes to the runner fleet without naming its repositories", filepath.Base(path), name)
			}
			for _, match := range matches {
				routed = append(routed, match[1])
			}
		}
	}
	slices.Sort(routed)
	routed = slices.Compact(routed)
	for _, name := range routed {
		if !slices.Contains(fleet.Repositories, name) {
			t.Errorf("%s/%s routes to the runner fleet without a declared runner", fleet.Owner, name)
		}
	}
	for _, name := range fleet.Repositories {
		if !slices.Contains(routed, name) {
			t.Errorf("%s/%s declares a runner that no workflow routes to the runner fleet", fleet.Owner, name)
		}
	}
}

func TestRunnerRepairsFollowGitHubState(t *testing.T) {
	fleet := testRunnerFleet()
	states := map[string][]registeredRunner{}
	for _, repository := range fleet.Repositories {
		states[repository] = []registeredRunner{healthyRunner(fleet, repository)}
	}
	if restart, missing := offlineRunners(fleet, states), missingRunners(fleet, states); restart != nil || missing != nil {
		t.Fatalf("healthy runners repaired: restart %q, reregister %q", restart, missing)
	}
	states["Y"][0].Status = "offline"
	foreign := healthyRunner(fleet, "infra")
	foreign.Name, foreign.Status = "infra-build-08-infra", "offline"
	states["infra"] = append(states["infra"], foreign)
	if restart := offlineRunners(fleet, states); !reflect.DeepEqual(restart, []string{"Y"}) {
		t.Fatalf("restart list does not match offline fleet runners: %q", restart)
	}
	states["infra"] = states["infra"][1:]
	if missing := missingRunners(fleet, states); !reflect.DeepEqual(missing, []string{"infra"}) {
		t.Fatalf("reregistration list does not match missing fleet runners: %q", missing)
	}
}

func TestRunnerConvergenceArguments(t *testing.T) {
	fastRunnerReads(t)
	cli := []string{"build/cli-release.json"}
	runners := func(inputs ...string) Plan {
		return Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners, RunnerInputs: inputs}}
	}
	for _, test := range []struct {
		name   string
		plan   Plan
		change func(*registeredRunner)
		want   string
	}{
		{"CLI release", runners(cli...), nil, "build-runners.yml --tags=infra_binary"},
		{"CLI release and fleet", runners("build/cli-release.json", "build/runners.json"), nil, "build-runners.yml"},
		{"runner role", runners("ansible/roles/build_runner/tasks/main.yml"), nil, "build-runners.yml"},
		{"CLI release with offline runner", runners(cli...), offlineY, `build-runners.yml --extra-vars {"build_runner_restart":["Y"]}`},
		{"CLI release in full scope", Plan{Affected: Selection{Ansible: true, HostScope: HostScopeFull, RunnerInputs: cli}}, nil, "reconcile.yml"},
		{"full with offline runner", Plan{Affected: All()}, offlineY, `reconcile.yml --extra-vars {"build_runner_restart":["Y"]}`},
		{"all offline", Plan{Affected: All()}, func(runner *registeredRunner) { runner.Status = "offline" }, `reconcile.yml --extra-vars {"build_runner_restart":["infra","Y"]}`},
		{"missing registration", runners(cli...), unregisteredY, `build-runners.yml --extra-vars {"build_runner_reregister":["Y"]}`},
		{"missing and offline", Plan{Affected: All()}, func(runner *registeredRunner) {
			unregisteredY(runner)
			if runner.Name == "infra-build-09-infra" {
				runner.Status = "offline"
			}
		}, `reconcile.yml --extra-vars {"build_runner_reregister":["Y"],"build_runner_restart":["infra"]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := &fleetHarness{t: t, fleet: testRunnerFleet(), change: test.change}
			if err := harness.commands().Hosts(context.Background(), test.plan); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(harness.calls, []string{test.want}) {
				t.Fatalf("unexpected runner convergence: %q", harness.calls)
			}
		})
	}
}

func TestRunnerLabelsConvergeOnlyMismatchedRunners(t *testing.T) {
	harness := &fleetHarness{t: t, fleet: testRunnerFleet(), change: func(runner *registeredRunner) {
		runner.ID = 7
		if runner.Name == "infra-build-09-Y" {
			runner.ID = 42
			runner.Labels = append(runner.Labels[:3], runnerLabel{"dagger-amd64", "custom"}, runnerLabel{"gpu", "custom"})
		}
	}}
	plan := Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners, RunnerInputs: []string{"build/runners.json"}}}
	if err := harness.commands().Hosts(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"gh api --method PUT --silent repos/fredrir/Y/actions/runners/42/labels -f labels[]=dagger-amd64 -f labels[]=infra-trusted",
		"build-runners.yml",
	}
	if !reflect.DeepEqual(harness.calls, want) {
		t.Fatalf("labels converged on the wrong runners: %q", harness.calls)
	}
}

func TestRunnerConvergenceRetriesTransientReads(t *testing.T) {
	fastRunnerReads(t)
	harness := &fleetHarness{t: t, fleet: testRunnerFleet(), unreadable: func(query int) bool { return query == 1 }, change: offlineY}
	if err := harness.commands().Hosts(context.Background(), Plan{Affected: All()}); err != nil {
		t.Fatal(err)
	}
	if want := []string{`reconcile.yml --extra-vars {"build_runner_restart":["Y"]}`}; !reflect.DeepEqual(harness.calls, want) || harness.stderr.Len() != 0 {
		t.Fatalf("transient read failure lost GitHub repairs: %q\n%s", harness.calls, harness.stderr.String())
	}
}

func TestUnreadableRunnerFleetDoesNotGateConvergence(t *testing.T) {
	fastRunnerReads(t)
	runnerStateWindow = 50 * time.Millisecond
	for _, test := range []struct {
		name string
		plan Plan
		want []string
	}{
		{"CLI release", Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners, RunnerInputs: []string{"build/cli-release.json"}}}, []string{"build-runners.yml"}},
		{"full", Plan{Affected: All()}, []string{"reconcile.yml"}},
		{"unchanged runners", Plan{Affected: All(), RunnersUnchanged: true}, []string{"reconcile.yml --skip-tags=runners", "build-runners.yml"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := &fleetHarness{t: t, fleet: testRunnerFleet(), unreadable: func(int) bool { return true }}
			commands := harness.commands()
			if err := commands.Hosts(context.Background(), test.plan); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(harness.calls, test.want) || commands.runnersVerified {
				t.Fatalf("unreadable fleet changed host convergence: %q, verified %t", harness.calls, commands.runnersVerified)
			}
			if !strings.Contains(harness.stderr.String(), "warning: runner fleet state unavailable") {
				t.Fatalf("unreadable fleet not reported: %q", harness.stderr.String())
			}
		})
	}
}

func TestRejectedLabelUpdateOnlyWarns(t *testing.T) {
	harness := &fleetHarness{t: t, fleet: testRunnerFleet(), rejectLabels: true, change: func(runner *registeredRunner) {
		if runner.Name == "infra-build-09-Y" {
			runner.Labels = runner.Labels[:3]
		}
	}}
	if err := harness.commands().Hosts(context.Background(), Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners}}); err != nil {
		t.Fatal(err)
	}
	if len(harness.calls) != 2 || harness.calls[1] != "build-runners.yml" || !strings.Contains(harness.stderr.String(), "warning: set labels of infra-build-09-Y") {
		t.Fatalf("rejected label update stopped convergence or went unreported: %q\n%s", harness.calls, harness.stderr.String())
	}
}

func TestOutdatedRunnersTakeNoNewJobsUntilReplaced(t *testing.T) {
	stale := "2.336.0"
	outdatedY := func(runner *registeredRunner) {
		runner.ID = 7
		if runner.Name == "infra-build-09-Y" {
			runner.ID, runner.Version = 42, &stale
		}
	}
	withdraw := "gh api --method DELETE --silent repos/fredrir/Y/actions/runners/42/labels"
	restore := "gh api --method PUT --silent repos/fredrir/Y/actions/runners/42/labels -f labels[]=dagger-amd64 -f labels[]=infra-trusted"
	plan := Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners, RunnerInputs: []string{"build/runners.json"}}}
	for _, test := range []struct {
		name    string
		harness func(*fleetHarness, context.CancelFunc)
		failure bool
		want    []string
		warning string
	}{
		{"replaced", func(*fleetHarness, context.CancelFunc) {}, false, []string{withdraw, "build-runners.yml", restore}, ""},
		{"failed play", func(h *fleetHarness, _ context.CancelFunc) { h.failing = []string{"build-runners.yml"} }, true, []string{withdraw, "build-runners.yml", restore}, ""},
		{"reconciliation deadline during play", func(h *fleetHarness, cancel context.CancelFunc) { h.play = cancel }, false, []string{withdraw, "build-runners.yml", restore}, ""},
		{"rejected withdrawal", func(h *fleetHarness, _ context.CancelFunc) { h.rejectLabels = true }, false, []string{withdraw, "build-runners.yml"}, "warning: withdraw infra-build-09-Y from new jobs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			harness := &fleetHarness{t: t, fleet: testRunnerFleet(), change: outdatedY}
			test.harness(harness, cancel)
			err := harness.commands().Hosts(ctx, plan)
			if (err != nil) != test.failure {
				t.Fatalf("runner replacement returned %v", err)
			}
			if !reflect.DeepEqual(harness.calls, test.want) {
				t.Fatalf("outdated runner was not withdrawn for exactly the play: %q", harness.calls)
			}
			if stderr := harness.stderr.String(); (test.warning == "" && stderr != "") || !strings.Contains(stderr, test.warning) {
				t.Fatalf("unexpected warnings: %q", stderr)
			}
		})
	}
}

func TestDriftRepairDispatchIsScopedToHourlyVerification(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github/workflows/reconcile-job.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			If          string            `yaml:"if"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	repair, ok := workflow.Jobs["repair"]
	if !ok || !reflect.DeepEqual(repair.Permissions, map[string]string{"actions": "write"}) {
		t.Fatalf("repair job permissions are not exactly actions: write: %+v", repair.Permissions)
	}
	for _, condition := range []string{"github.event.schedule == '47 * * * *'", "needs.apply.result == 'failure'", "!cancelled()"} {
		if !strings.Contains(repair.If, condition) || strings.Contains(repair.If, "||") {
			t.Errorf("repair job condition %q does not require %s", repair.If, condition)
		}
	}
	var script strings.Builder
	for _, step := range repair.Steps {
		script.WriteString(step.Run)
	}
	for _, guard := range []string{"--workflow reconcile.yml", "--event workflow_dispatch", "--user 'github-actions[bot]'", `--commit "$GITHUB_SHA"`, "failure|timed_out|startup_failure)"} {
		if !strings.Contains(script.String(), guard) {
			t.Errorf("repair dispatch lacks guard %s:\n%s", guard, script.String())
		}
	}
	for name, job := range workflow.Jobs {
		if name != "repair" && job.Permissions["actions"] == "write" {
			t.Errorf("job %s can dispatch workflows", name)
		}
	}
}

func TestMissingRunnersAreConfirmedBeforeReregistration(t *testing.T) {
	fastRunnerReads(t)
	for _, test := range []struct {
		name       string
		missing    int
		unreadable func(int) bool
		want       string
	}{
		{"registration appeared", 1, nil, "build-runners.yml"},
		{"registration missing", 2, nil, `build-runners.yml --extra-vars {"build_runner_reregister":["Y"]}`},
		{"confirmation unreadable", 2, func(query int) bool { return query == 3 }, "build-runners.yml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			harness := &fleetHarness{t: t, fleet: testRunnerFleet(), unreadable: test.unreadable, change: func(runner *registeredRunner) {
				if runner.Name == "infra-build-09-Y" {
					if reads++; reads <= test.missing {
						unregisteredY(runner)
					}
				}
			}}
			plan := Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners, RunnerInputs: []string{"build/runners.json"}}}
			if err := harness.commands().Hosts(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(harness.calls, []string{test.want}) || harness.queries != len(testRunnerFleet().Repositories)+1 {
				t.Fatalf("unconfirmed registration repaired: %q after %d queries", harness.calls, harness.queries)
			}
		})
	}
}
