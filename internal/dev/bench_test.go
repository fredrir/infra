package dev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

const scenariosFixture = `- name: fast
  command: "{infra} pipeline check-fast --local"
  setup: "{infra} ci prepare-validation"
  budget: 1s
  warmup: 1
  runs: 3
- name: slow
  command: go test ./...
  runs: 2
`

func benchRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, benchScenariosFile), scenariosFixture)
	return root
}

type benchFake struct {
	t        *testing.T
	root     string
	times    map[string][]float64
	commands [][]string
}

func (f *benchFake) runner() ci.Runner {
	return ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		command := append([]string{filepath.Base(options.Name)}, options.Args...)
		f.commands = append(f.commands, command)
		switch command[0] {
		case "git":
			return process.Result{Stdout: []byte(strings.Repeat("d", 40) + "\n")}, nil
		case "hyperfine":
			name := command[slices.Index(command, "--command-name")+1]
			export := command[slices.Index(command, "--export-json")+1]
			times := f.times[name]
			codes := make([]int, len(times))
			if name == "slow" {
				codes[len(codes)-1] = 1
			}
			memory := make([]int64, len(times))
			for index := range memory {
				memory[index] = int64(1000 * (index + 1))
			}
			payload := map[string]any{"results": []map[string]any{{"command": name, "mean": 0.5, "median": times[len(times)/2], "min": times[0], "max": times[len(times)-1], "user": 0.4, "system": 0.1, "times": times, "exit_codes": codes, "memory_usage_byte": memory}}}
			data, _ := json.Marshal(payload)
			if err := os.WriteFile(export, data, 0o644); err != nil {
				f.t.Fatal(err)
			}
		case "go":
			if command[1] == "test" && options.Stdout != nil {
				fmt.Fprintln(options.Stdout, "BenchmarkExample-16 1000 1200 ns/op")
			}
		}
		return process.Result{}, nil
	}}
}

func TestBenchRunRecordsPercentilesBudgetsAndFailures(t *testing.T) {
	root := benchRoot(t)
	fake := &benchFake{t: t, root: root, times: map[string][]float64{"fast": {0.2, 0.3, 1.5}, "slow": {2, 3}}}
	var log strings.Builder
	summary, err := BenchRun(context.Background(), BenchOptions{State: NewState(root), Runner: fake.runner(), Log: &log})
	if err == nil || !strings.Contains(err.Error(), "fast") || !strings.Contains(err.Error(), "slow") {
		t.Fatalf("budget breach and failure not reported: %v", err)
	}
	if summary.Revision != strings.Repeat("d", 40) || len(summary.Results) != 2 {
		t.Fatalf("unexpected summary %+v", summary)
	}
	fast := summary.Results[0]
	if fast.Name != "fast" || fast.Runs != 3 || fast.P95Seconds != 1.5 || fast.MaxSeconds != 1.5 || fast.BudgetSeconds != 1 || !fast.BudgetExceeded || fast.Failed || fast.PeakMemoryBytes != 3000 {
		t.Fatalf("unexpected fast result %+v", fast)
	}
	if slow := summary.Results[1]; !slow.Failed || slow.BudgetExceeded || slow.BudgetSeconds != 0 || slow.MedianSeconds != 3 {
		t.Fatalf("unexpected slow result %+v", slow)
	}
	binary := filepath.Join(root, ".cache/dev/bin/infra")
	var hyperfine []string
	for _, command := range fake.commands {
		if command[0] == "hyperfine" && slices.Contains(command, "fast") {
			hyperfine = command
		}
	}
	for _, expected := range []string{"--warmup", "1", "--runs", "3", "--shell=none", "--ignore-failure", "--command-name", "fast", "--setup", binary + " ci prepare-validation", "--", binary + " pipeline check-fast --local"} {
		if !slices.Contains(hyperfine, expected) {
			t.Errorf("hyperfine invocation lacks %q: %v", expected, hyperfine)
		}
	}
	if !slices.ContainsFunc(fake.commands, func(command []string) bool {
		return slices.Equal(command, []string{"go", "build", "-o", binary, "./cmd/infra"})
	}) {
		t.Fatalf("benchmark binary not built: %v", fake.commands)
	}
	latest, err := ReadBenchSummary(filepath.Join(root, ".cache/dev/bench/latest.json"))
	if err != nil || len(latest.Results) != 2 || latest.Revision != summary.Revision {
		t.Fatalf("latest summary not written: %v %+v", err, latest)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".cache/dev/bench"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected a timestamped directory beside latest.json: %v", err)
	}
	fake.commands = nil
	summary, err = BenchRun(context.Background(), BenchOptions{State: NewState(root), Runner: fake.runner(), Names: []string{"slow"}})
	if len(summary.Results) != 1 || summary.Results[0].Name != "slow" {
		t.Fatalf("scenario selection ignored: %+v %v", summary, err)
	}
	if _, err := BenchRun(context.Background(), BenchOptions{State: NewState(root), Runner: fake.runner(), Names: []string{"unknown"}}); err == nil {
		t.Fatal("unknown scenario accepted")
	}
}

func TestBenchCompareFlagsRegressionsAboveThreshold(t *testing.T) {
	base := BenchSummary{Results: []BenchResult{{Name: "fast", MedianSeconds: 1}, {Name: "slow", MedianSeconds: 10}}}
	candidate := BenchSummary{Results: []BenchResult{{Name: "fast", MedianSeconds: 1.05}, {Name: "slow", MedianSeconds: 12}, {Name: "new", MedianSeconds: 3, BudgetExceeded: true}}}
	deltas, regressed := BenchCompare(base, candidate, 0.10)
	if !regressed || len(deltas) != 3 {
		t.Fatalf("regression not flagged: %+v", deltas)
	}
	if deltas[0].Regressed || deltas[0].Change < 0.049 || deltas[0].Change > 0.051 {
		t.Errorf("fast within threshold flagged: %+v", deltas[0])
	}
	if !deltas[1].Regressed || deltas[1].BaseSeconds != 10 {
		t.Errorf("slow regression missed: %+v", deltas[1])
	}
	if deltas[2].BaseSeconds != 0 || deltas[2].Regressed || !deltas[2].BudgetExceeded {
		t.Errorf("unknown baseline handled wrongly: %+v", deltas[2])
	}
	if _, regressed := BenchCompare(base, BenchSummary{Results: []BenchResult{{Name: "fast", MedianSeconds: 1.2}}}, 0.25); regressed {
		t.Fatal("custom threshold ignored")
	}
}

func TestBenchGoRunsBenchmarksAndBenchstatAgainstBaseline(t *testing.T) {
	root := benchRoot(t)
	fake := &benchFake{t: t, root: root}
	var stdout strings.Builder
	output, err := BenchGo(context.Background(), BenchGoOptions{State: NewState(root), Runner: fake.runner(), Stdout: &stdout, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "BenchmarkExample") {
		t.Fatalf("benchmark output not printed: %q", stdout.String())
	}
	if !slices.Equal(fake.commands[0], []string{"go", "test", "-run", "^$", "-bench", ".", "-benchmem", "-count", "6", "./..."}) {
		t.Fatalf("unexpected benchmark invocation %v", fake.commands[0])
	}
	if data, err := os.ReadFile(output); err != nil || !strings.Contains(string(data), "BenchmarkExample") {
		t.Fatalf("benchmark output not recorded: %v", err)
	}
	writeFile(t, filepath.Join(root, ".cache/dev/bench/go-baseline.txt"), "BenchmarkExample-16 1000 1000 ns/op\n")
	fake.commands = nil
	output, err = BenchGo(context.Background(), BenchGoOptions{State: NewState(root), Runner: fake.runner(), Packages: []string{"./internal/ci"}, Count: 3, Stdout: &stdout, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.commands[1], []string{"go", "run", benchstatModule, filepath.Join(root, ".cache/dev/bench/go-baseline.txt"), output}) || !slices.Contains(fake.commands[0], "./internal/ci") || !slices.Contains(fake.commands[0], "3") {
		t.Fatalf("benchstat comparison missing: %v", fake.commands)
	}
}
