package dev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

const (
	benchScenariosFile = "dev/bench/scenarios.yaml"
	benchstatModule    = "golang.org/x/perf/cmd/benchstat@v0.0.0-20260908200009-22c9c6c9d4da"
)

type Scenario struct {
	Name    string `yaml:"name"`
	Command string `yaml:"command"`
	Setup   string `yaml:"setup"`
	Warmup  int    `yaml:"warmup"`
	Runs    int    `yaml:"runs"`
	Budget  string `yaml:"budget"`
}

type BenchResult struct {
	Name            string  `json:"name"`
	Command         string  `json:"command"`
	Runs            int     `json:"runs"`
	MeanSeconds     float64 `json:"mean_seconds"`
	MedianSeconds   float64 `json:"median_seconds"`
	P95Seconds      float64 `json:"p95_seconds"`
	MinSeconds      float64 `json:"min_seconds"`
	MaxSeconds      float64 `json:"max_seconds"`
	UserSeconds     float64 `json:"user_seconds"`
	SystemSeconds   float64 `json:"system_seconds"`
	PeakMemoryBytes int64   `json:"peak_memory_bytes,omitempty"`
	BudgetSeconds   float64 `json:"budget_seconds,omitempty"`
	BudgetExceeded  bool    `json:"budget_exceeded"`
	Failed          bool    `json:"failed"`
}

type BenchSummary struct {
	Schema   int           `json:"schema"`
	Revision string        `json:"revision,omitempty"`
	Started  time.Time     `json:"started"`
	Results  []BenchResult `json:"results"`
}

type BenchDelta struct {
	Name           string  `json:"name"`
	BaseSeconds    float64 `json:"base_median_seconds"`
	Seconds        float64 `json:"median_seconds"`
	Change         float64 `json:"change"`
	Regressed      bool    `json:"regressed"`
	BudgetExceeded bool    `json:"budget_exceeded"`
}

type BenchOptions struct {
	State     State
	Runner    ci.Runner
	Names     []string
	Baseline  string
	Threshold float64
	Log       io.Writer
}

var scenarioNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func (s State) Bench() string { return filepath.Join(s.Cache, "bench") }

func (opts BenchOptions) defaults() BenchOptions {
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	if opts.Threshold <= 0 {
		opts.Threshold = 0.10
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	return opts
}

func readScenarios(root string) ([]Scenario, error) {
	data, err := os.ReadFile(filepath.Join(root, benchScenariosFile))
	if err != nil {
		return nil, err
	}
	var scenarios []Scenario
	if err := yaml.Unmarshal(data, &scenarios); err != nil {
		return nil, fmt.Errorf("%s: %w", benchScenariosFile, err)
	}
	seen := make(map[string]bool)
	for _, scenario := range scenarios {
		if !scenarioNamePattern.MatchString(scenario.Name) || seen[scenario.Name] || strings.TrimSpace(scenario.Command) == "" || scenario.Runs < 1 || scenario.Warmup < 0 {
			return nil, fmt.Errorf("%s: scenario %q needs a unique name, a command and runs", benchScenariosFile, scenario.Name)
		}
		if scenario.Budget != "" {
			if _, err := time.ParseDuration(scenario.Budget); err != nil {
				return nil, fmt.Errorf("%s: scenario %s budget: %w", benchScenariosFile, scenario.Name, err)
			}
		}
		seen[scenario.Name] = true
	}
	if len(scenarios) == 0 {
		return nil, fmt.Errorf("%s declares no scenarios", benchScenariosFile)
	}
	return scenarios, nil
}

func (opts BenchOptions) binary(ctx context.Context) (string, error) {
	binary, err := filepath.Abs(filepath.Join(opts.State.Bin(), "infra"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		return "", err
	}
	if err := opts.Runner.Run(ctx, "go", "build", "-o", binary, "./cmd/infra"); err != nil {
		return "", fmt.Errorf("build benchmark binary: %w", err)
	}
	return binary, nil
}

func BenchRun(ctx context.Context, opts BenchOptions) (BenchSummary, error) {
	opts = opts.defaults()
	summary := BenchSummary{Schema: 1, Started: time.Now().UTC()}
	scenarios, err := readScenarios(opts.State.Root)
	if err != nil {
		return summary, err
	}
	for _, name := range opts.Names {
		if !slices.ContainsFunc(scenarios, func(scenario Scenario) bool { return scenario.Name == name }) {
			return summary, fmt.Errorf("unknown scenario %q", name)
		}
	}
	binary, err := opts.binary(ctx)
	if err != nil {
		return summary, err
	}
	if revision, err := capture(ctx, opts.Runner, "git", "rev-parse", "HEAD"); err == nil {
		summary.Revision = revision
	}
	directory := filepath.Join(opts.State.Bench(), summary.Started.Format("20060102-150405"))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return summary, err
	}
	tools, err := filepath.Abs(opts.State.Tools())
	if err != nil {
		return summary, err
	}
	env := []string{"PATH=" + opts.State.devPath(tools)}
	for _, scenario := range scenarios {
		if len(opts.Names) > 0 && !slices.Contains(opts.Names, scenario.Name) {
			continue
		}
		command := strings.Fields(strings.ReplaceAll(scenario.Command, "{infra}", binary))
		export, err := filepath.Abs(filepath.Join(directory, scenario.Name+".json"))
		if err != nil {
			return summary, err
		}
		arguments := []string{"--warmup", strconv.Itoa(scenario.Warmup), "--runs", strconv.Itoa(scenario.Runs), "--shell=none", "--ignore-failure", "--command-name", scenario.Name, "--export-json", export}
		if scenario.Setup != "" {
			arguments = append(arguments, "--setup", strings.ReplaceAll(scenario.Setup, "{infra}", binary))
		}
		arguments = append(arguments, "--", strings.Join(command, " "))
		fmt.Fprintln(opts.Log, "Scenario:", scenario.Name, strings.Join(command, " "))
		if _, err := execute(ctx, opts.Runner, process.Options{Name: opts.tool("hyperfine"), Args: arguments, Env: env, Stdout: opts.Log, Stderr: opts.Log}); err != nil {
			return summary, fmt.Errorf("scenario %s: %w", scenario.Name, err)
		}
		result, err := readHyperfine(export, scenario)
		if err != nil {
			return summary, err
		}
		summary.Results = append(summary.Results, result)
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return summary, err
	}
	for _, path := range []string{filepath.Join(directory, "summary.json"), filepath.Join(opts.State.Bench(), "latest.json")} {
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return summary, err
		}
	}
	fmt.Fprintln(opts.Log, "Summary:", filepath.Join(directory, "summary.json"))
	for _, result := range summary.Results {
		if result.Failed || result.BudgetExceeded {
			err = errors.Join(err, fmt.Errorf("scenario %s failed or exceeded its budget", result.Name))
		}
	}
	return summary, err
}

func (opts BenchOptions) tool(name string) string {
	if path, err := opts.State.toolPath(name); err == nil {
		return path
	}
	return name
}

func readHyperfine(path string, scenario Scenario) (BenchResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return BenchResult{}, err
	}
	var export struct {
		Results []struct {
			Command   string    `json:"command"`
			Mean      float64   `json:"mean"`
			Median    float64   `json:"median"`
			Min       float64   `json:"min"`
			Max       float64   `json:"max"`
			User      float64   `json:"user"`
			System    float64   `json:"system"`
			Times     []float64 `json:"times"`
			ExitCodes []int     `json:"exit_codes"`
			Memory    []int64   `json:"memory_usage_byte"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &export); err != nil || len(export.Results) != 1 {
		return BenchResult{}, fmt.Errorf("hyperfine export %s: %v", path, err)
	}
	measured := export.Results[0]
	result := BenchResult{Name: scenario.Name, Command: scenario.Command, Runs: len(measured.Times), MeanSeconds: measured.Mean, MedianSeconds: measured.Median, MinSeconds: measured.Min, MaxSeconds: measured.Max, UserSeconds: measured.User, SystemSeconds: measured.System, P95Seconds: percentile(measured.Times, 0.95)}
	for _, memory := range measured.Memory {
		result.PeakMemoryBytes = max(result.PeakMemoryBytes, memory)
	}
	for _, code := range measured.ExitCodes {
		if code != 0 {
			result.Failed = true
		}
	}
	if scenario.Budget != "" {
		budget, _ := time.ParseDuration(scenario.Budget)
		result.BudgetSeconds = budget.Seconds()
		result.BudgetExceeded = result.MaxSeconds > result.BudgetSeconds
	}
	return result, nil
}

func percentile(times []float64, fraction float64) float64 {
	if len(times) == 0 {
		return 0
	}
	sorted := slices.Clone(times)
	sort.Float64s(sorted)
	index := int(math.Ceil(fraction*float64(len(sorted)))) - 1
	return sorted[max(0, min(index, len(sorted)-1))]
}

func ReadBenchSummary(path string) (BenchSummary, error) {
	var summary BenchSummary
	data, err := os.ReadFile(path)
	if err != nil {
		return summary, err
	}
	if err := json.Unmarshal(data, &summary); err != nil {
		return summary, fmt.Errorf("%s: %w", path, err)
	}
	return summary, nil
}

func BenchCompare(base, candidate BenchSummary, threshold float64) ([]BenchDelta, bool) {
	var deltas []BenchDelta
	regressed := false
	for _, result := range candidate.Results {
		delta := BenchDelta{Name: result.Name, Seconds: result.MedianSeconds, BudgetExceeded: result.BudgetExceeded}
		index := slices.IndexFunc(base.Results, func(entry BenchResult) bool { return entry.Name == result.Name })
		if index >= 0 && base.Results[index].MedianSeconds > 0 {
			delta.BaseSeconds = base.Results[index].MedianSeconds
			delta.Change = result.MedianSeconds/delta.BaseSeconds - 1
			delta.Regressed = delta.Change > threshold
		}
		regressed = regressed || delta.Regressed || delta.BudgetExceeded
		deltas = append(deltas, delta)
	}
	return deltas, regressed
}

type BenchGoOptions struct {
	State    State
	Runner   ci.Runner
	Packages []string
	Count    int
	Stdout   io.Writer
	Log      io.Writer
}

func BenchGo(ctx context.Context, opts BenchGoOptions) (string, error) {
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	if opts.Count <= 0 {
		opts.Count = 6
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	packages := opts.Packages
	if len(packages) == 0 {
		packages = []string{"./..."}
	}
	if err := os.MkdirAll(opts.State.Bench(), 0o755); err != nil {
		return "", err
	}
	output := filepath.Join(opts.State.Bench(), "go-"+time.Now().UTC().Format("20060102-150405")+".txt")
	arguments := append([]string{"test", "-run", "^$", "-bench", ".", "-benchmem", "-count", strconv.Itoa(opts.Count)}, packages...)
	if err := runToFile(ctx, opts.Runner, output, process.Options{Name: "go", Args: arguments, Stderr: opts.Log}); err != nil {
		return output, fmt.Errorf("go benchmarks: %w", err)
	}
	baseline := filepath.Join(opts.State.Bench(), "go-baseline.txt")
	if _, err := os.Stat(baseline); err == nil {
		if _, err := execute(ctx, opts.Runner, process.Options{Name: "go", Args: []string{"run", benchstatModule, baseline, output}, Stdout: opts.Stdout, Stderr: opts.Log}); err != nil {
			return output, fmt.Errorf("benchstat: %w", err)
		}
		return output, nil
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return output, err
	}
	_, err = opts.Stdout.Write(data)
	return output, err
}
