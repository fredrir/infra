package reconcile

import (
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

func TestFleetRoutedRepositoriesHaveDeclaredRunners(t *testing.T) {
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
	if len(routed) == 0 {
		t.Fatal("no repository routes to the runner fleet")
	}
	slices.Sort(routed)
	for _, name := range slices.Compact(routed) {
		if !slices.Contains(fleet.Repositories, name) {
			t.Errorf("%s/%s routes to the runner fleet without a declared runner", fleet.Owner, name)
		}
	}
}
