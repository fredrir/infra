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
	return RunnerFleet{Schema: 2, Owner: "fredrir", Host: "infra-build-09", Version: "2.337.0", SHA256: strings.Repeat("a", 64), Labels: []string{"dagger-amd64", "infra-trusted"}, Repositories: map[string]int{"infra": 2, "Y": 1}}
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

func healthyRunner(fleet RunnerFleet, name string) registeredRunner {
	version := fleet.Version
	labels := []runnerLabel{{"self-hosted", "read-only"}, {"Linux", "read-only"}, {"X64", "read-only"}}
	for _, label := range fleet.Labels {
		labels = append(labels, runnerLabel{label, "custom"})
	}
	return registeredRunner{Name: fleet.Host + "-" + name, Status: "online", Version: &version, Labels: labels}
}

func healthyRunners(fleet RunnerFleet, repository string) []registeredRunner {
	var runners []registeredRunner
	for _, runner := range fleet.Runners() {
		if runner.Repository == repository {
			runners = append(runners, healthyRunner(fleet, runner.Name))
		}
	}
	return runners
}

func healthyStates(fleet RunnerFleet) map[string][]registeredRunner {
	states := map[string][]registeredRunner{}
	for _, repository := range fleet.RepositoryNames() {
		states[repository] = healthyRunners(fleet, repository)
	}
	return states
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
	t            *testing.T
	fleet        RunnerFleet
	mu           sync.Mutex
	calls        []string
	queries      int
	unreadable   func(query int) bool
	rejectLabels bool
	change       func(*registeredRunner)
	stderr       bytes.Buffer
}

func (h *fleetHarness) commands() *Commands {
	return &Commands{Runner: ci.Runner{Dir: writeRunnerFleet(h.t, h.fleet), Execute: h.execute, Stderr: &h.stderr}}
}

func (h *fleetHarness) execute(_ context.Context, opts process.Options) (process.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case opts.Name == "ansible-playbook":
		h.calls = append(h.calls, strings.Join(opts.Args[2:], " "))
		return process.Result{}, nil
	case opts.Name == "gh" && slices.Contains(opts.Args, "PUT"):
		h.calls = append(h.calls, "gh "+strings.Join(opts.Args, " "))
		if h.rejectLabels {
			return process.Result{ExitCode: 1}, errors.New("HTTP 403")
		}
		return process.Result{}, nil
	case opts.Name == "gh":
		h.queries++
		if h.unreadable != nil && h.unreadable(h.queries) {
			return process.Result{ExitCode: 1}, errors.New("HTTP 502")
		}
		runners := healthyRunners(h.fleet, queriedRepository(opts))
		if h.change != nil {
			for index := range runners {
				h.change(&runners[index])
			}
		}
		return runnerResponse(h.t, runners...), nil
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
	if runner.Name == "infra-build-09-Y-1" {
		runner.Status = "offline"
	}
}

func unregisteredY(runner *registeredRunner) {
	if runner.Name == "infra-build-09-Y-1" {
		runner.Name = "infra-build-08-Y-1"
	}
}

func TestLoadRunnerFleetValidatesDeclaration(t *testing.T) {
	fleet, err := LoadRunnerFleet(writeRunnerFleet(t, testRunnerFleet()))
	if err != nil || !reflect.DeepEqual(fleet, testRunnerFleet()) {
		t.Fatalf("valid runner fleet rejected: %+v, %v", fleet, err)
	}
	for name, change := range map[string]func(*RunnerFleet){
		"schema":                func(f *RunnerFleet) { f.Schema = 1 },
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
		"no runners":            func(f *RunnerFleet) { f.Repositories = map[string]int{"infra": 0} },
		"too many runners":      func(f *RunnerFleet) { f.Repositories = map[string]int{"infra": 9} },
		"traversing repository": func(f *RunnerFleet) { f.Repositories = map[string]int{"..": 1} },
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
		{"missing", func(s map[string][]registeredRunner) { delete(s, "Y") }, "fredrir/Y has 0 runners named infra-build-09-Y-1, want 1"},
		{"missing second runner", func(s map[string][]registeredRunner) { s["infra"] = s["infra"][:1] }, "fredrir/infra has 0 runners named infra-build-09-infra-2, want 1"},
		{"renamed", func(s map[string][]registeredRunner) { s["Y"][0].Name = "infra-build-08-Y-1" }, "fredrir/Y has 0 runners named infra-build-09-Y-1, want 1"},
		{"duplicate", func(s map[string][]registeredRunner) { s["Y"] = append(s["Y"], s["Y"][0]) }, "fredrir/Y has 2 runners named infra-build-09-Y-1, want 1"},
		{"retired single-name runner", func(s map[string][]registeredRunner) {
			s["infra"] = append(s["infra"], healthyRunner(fleet, "infra"))
		}, "fredrir/infra registers undeclared runner infra-build-09-infra"},
		{"offline", func(s map[string][]registeredRunner) { s["Y"][0].Status = "offline" }, "infra-build-09-Y-1 is offline, want online"},
		{"null version", func(s map[string][]registeredRunner) { s["Y"][0].Version = nil }, "infra-build-09-Y-1 reports no version, want 2.337.0"},
		{"stale version", func(s map[string][]registeredRunner) { s["Y"][0].Version = &stale }, "infra-build-09-Y-1 runs 2.300.0, want 2.337.0"},
		{"missing label", func(s map[string][]registeredRunner) { s["Y"][0].Labels = s["Y"][0].Labels[:4] }, "infra-build-09-Y-1 has labels [dagger-amd64], want [dagger-amd64 infra-trusted]"},
		{"extra label", func(s map[string][]registeredRunner) {
			s["Y"][0].Labels = append(s["Y"][0].Labels, runnerLabel{"gpu", "custom"})
		}, "infra-build-09-Y-1 has labels [dagger-amd64 gpu infra-trusted], want [dagger-amd64 infra-trusted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			states := healthyStates(fleet)
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

func TestRunnerStatesQueryEachRepository(t *testing.T) {
	fleet := testRunnerFleet()
	var mu sync.Mutex
	var queries []string
	commands := Commands{Runner: ci.Runner{Execute: func(_ context.Context, opts process.Options) (process.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		queries = append(queries, opts.Name+" "+strings.Join(opts.Args, " "))
		return runnerResponse(t, healthyRunners(fleet, queriedRepository(opts))...), nil
	}}}
	states, err := commands.runnerStates(context.Background(), fleet)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(queries)
	want := []string{
		"gh api repos/fredrir/Y/actions/runners?per_page=100",
		"gh api repos/fredrir/infra/actions/runners?per_page=100",
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
		runners := healthyRunners(fleet, queriedRepository(opts))
		for index := range runners {
			runners[index].Version = nil
		}
		return runnerResponse(t, runners...), nil
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := commands.verifyRunnerFleet(ctx, fleet)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "infra-build-09-Y-1 reports no version") {
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
		if fleet.Repositories[name] == 0 {
			t.Errorf("%s/%s routes to the runner fleet without a declared runner", fleet.Owner, name)
		}
	}
	for _, name := range fleet.RepositoryNames() {
		if !slices.Contains(routed, name) {
			t.Errorf("%s/%s declares a runner that no workflow routes to the runner fleet", fleet.Owner, name)
		}
	}
}

func TestRunnerRepairsFollowGitHubState(t *testing.T) {
	fleet := testRunnerFleet()
	states := healthyStates(fleet)
	if restart, missing := offlineRunners(fleet, states), missingRunners(fleet, states); restart != nil || missing != nil {
		t.Fatalf("healthy runners repaired: restart %q, reregister %q", restart, missing)
	}
	states["Y"][0].Status = "offline"
	foreign := healthyRunner(fleet, "infra-1")
	foreign.Name, foreign.Status = "infra-build-08-infra-1", "offline"
	states["infra"] = append(states["infra"], foreign)
	if restart := offlineRunners(fleet, states); !reflect.DeepEqual(restart, []string{"Y-1"}) {
		t.Fatalf("restart list does not match offline fleet runners: %q", restart)
	}
	states["infra"] = states["infra"][1:]
	if missing := missingRunners(fleet, states); !reflect.DeepEqual(missing, []string{"infra-1"}) {
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
		{"CLI release with offline runner", runners(cli...), offlineY, `build-runners.yml --extra-vars {"build_runner_restart":["Y-1"]}`},
		{"CLI release in full scope", Plan{Affected: Selection{Ansible: true, HostScope: HostScopeFull, RunnerInputs: cli}}, nil, "reconcile.yml"},
		{"full with offline runner", Plan{Affected: All()}, offlineY, `reconcile.yml --extra-vars {"build_runner_restart":["Y-1"]}`},
		{"all offline", Plan{Affected: All()}, func(runner *registeredRunner) { runner.Status = "offline" }, `reconcile.yml --extra-vars {"build_runner_restart":["Y-1","infra-1","infra-2"]}`},
		{"missing registration", runners(cli...), unregisteredY, `build-runners.yml --extra-vars {"build_runner_reregister":["Y-1"]}`},
		{"missing and offline", Plan{Affected: All()}, func(runner *registeredRunner) {
			unregisteredY(runner)
			if runner.Name == "infra-build-09-infra-2" {
				runner.Status = "offline"
			}
		}, `reconcile.yml --extra-vars {"build_runner_reregister":["Y-1"],"build_runner_restart":["infra-2"]}`},
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
		if runner.Name == "infra-build-09-Y-1" {
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
	if want := []string{`reconcile.yml --extra-vars {"build_runner_restart":["Y-1"]}`}; !reflect.DeepEqual(harness.calls, want) || harness.stderr.Len() != 0 {
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
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := &fleetHarness{t: t, fleet: testRunnerFleet(), unreadable: func(int) bool { return true }}
			commands := harness.commands()
			if err := commands.Hosts(context.Background(), test.plan); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(harness.calls, test.want) {
				t.Fatalf("unreadable fleet changed host convergence: %q", harness.calls)
			}
			if !strings.Contains(harness.stderr.String(), "warning: runner fleet state unavailable") {
				t.Fatalf("unreadable fleet not reported: %q", harness.stderr.String())
			}
		})
	}
}

func TestRejectedLabelUpdateOnlyWarns(t *testing.T) {
	harness := &fleetHarness{t: t, fleet: testRunnerFleet(), rejectLabels: true, change: func(runner *registeredRunner) {
		if runner.Name == "infra-build-09-Y-1" {
			runner.Labels = runner.Labels[:3]
		}
	}}
	if err := harness.commands().Hosts(context.Background(), Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners}}); err != nil {
		t.Fatal(err)
	}
	if len(harness.calls) != 2 || harness.calls[1] != "build-runners.yml" || !strings.Contains(harness.stderr.String(), "warning: set labels of infra-build-09-Y-1") {
		t.Fatalf("rejected label update stopped convergence or went unreported: %q\n%s", harness.calls, harness.stderr.String())
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
		{"registration missing", 2, nil, `build-runners.yml --extra-vars {"build_runner_reregister":["Y-1"]}`},
		{"confirmation unreadable", 2, func(query int) bool { return query == 3 }, "build-runners.yml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			harness := &fleetHarness{t: t, fleet: testRunnerFleet(), unreadable: test.unreadable, change: func(runner *registeredRunner) {
				if runner.Name == "infra-build-09-Y-1" {
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

func TestRunnerTokenReachesOnlyRunnerChildren(t *testing.T) {
	fastRunnerReads(t)
	const token = "GH_TOKEN=runner-app-secret"
	fleet := testRunnerFleet()
	var mu sync.Mutex
	var children []process.Options
	commands := &Commands{Work: t.TempDir(), RunnerToken: "runner-app-secret", Runner: ci.Runner{Dir: writeRunnerFleet(t, fleet), Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		children = append(children, options)
		switch {
		case options.Name == "gh" && !slices.Contains(options.Args, "PUT"):
			return runnerResponse(t, healthyRunner(fleet, queriedRepository(options))), nil
		case options.Name == "tofu" && slices.Contains(options.Args, "show"):
			return process.Result{Stdout: []byte("{}")}, nil
		}
		return process.Result{}, nil
	}}}
	ctx := context.Background()
	full, runners := Plan{Affected: All()}, Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners, RunnerInputs: []string{"build/runners.json"}}}
	for _, operation := range []func() error{
		func() error { return commands.Plan(ctx, Plan{Affected: Selection{Tofu: true}}) },
		func() error { return commands.PlanHosts(ctx, full) },
		func() error { return commands.PlanHosts(ctx, runners) },
		func() error { return commands.Hosts(ctx, full) },
		func() error { return commands.Hosts(ctx, runners) },
		func() error { return commands.Monitor(ctx, full) },
		func() error { return commands.VerifyHosts(ctx, full) },
		func() error { return commands.compareHosts(ctx) },
	} {
		operation()
	}
	granted := map[string]bool{}
	for _, child := range children {
		command := child.Name + " " + strings.Join(child.Args, " ")
		playbook := child.Name == "ansible-playbook" && len(child.Args) > 2 && (child.Args[2] == "reconcile.yml" || child.Args[2] == "build-runners.yml") && !slices.ContainsFunc(child.Args, func(arg string) bool { return arg == "--check" || arg == "--syntax-check" || arg == "--list-tasks" })
		fleetAPI := child.Name == "gh" && strings.Contains(command, "repos/fredrir/") && strings.Contains(command, "/actions/runners")
		if slices.Contains(child.Env, token) != (playbook || fleetAPI) {
			t.Errorf("runner token reached=%t for %s", slices.Contains(child.Env, token), command)
		}
		granted[child.Name] = granted[child.Name] || slices.Contains(child.Env, token)
	}
	for _, name := range []string{"tofu", "ansible-playbook", "gh"} {
		if !slices.ContainsFunc(children, func(child process.Options) bool { return child.Name == name }) {
			t.Errorf("no %s child exercised", name)
		}
	}
	if !granted["gh"] || !granted["ansible-playbook"] || granted["tofu"] {
		t.Errorf("runner token granted to %v", granted)
	}
}
