package dev

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

type Suite struct {
	Name           string
	Package        string
	Test           string
	Tools          []string
	Env            []string
	RootVariable   string
	BinaryVariable string
	Engine         string
}

var Suites = []Suite{
	{Name: "onboarding", Package: "./internal/projects", Test: "^TestNativeOnboardingQualification$", Tools: []string{"age-keygen", "sops", "helm", "kustomize"}, Env: []string{"INFRA_ONBOARD_INTEGRATION=1"}, RootVariable: "INFRA_TEST_SOURCE_ROOT"},
	{Name: "image", Package: "./internal/pipeline", Test: "^TestDaggerImageBuildSelectsStageAndPreservesLiteralBuildArguments$", RootVariable: "INFRA_DAGGER_IMAGE_TEST_ROOT", Engine: "build"},
	{Name: "reconcile-plan", Package: "./internal/reconcile", Test: "^TestSavedExpansionProofWithLocalTofu$", Tools: []string{"tofu"}, Env: []string{"INFRA_RECONCILE_PLAN_QUALIFY=1"}},
	{Name: "packages", Package: "./internal/packages", Test: "Qualification$", Tools: []string{"nfpm", "gpg", "openssl", "go"}, Env: []string{"INFRA_PACKAGE_QUALIFY=1"}, BinaryVariable: "INFRA_QUALIFICATION_BINARY", Engine: "build"},
	{Name: "kata", Package: "./internal/kata", Test: "^TestDaggerExecutesBoundedWorker$", Env: []string{"INFRA_KATA_ENGINE_TEST=1"}, BinaryVariable: "INFRA_KATA_BINARY", Engine: "kata"},
}

func SuiteNames() []string {
	names := make([]string, 0, len(Suites))
	for _, suite := range Suites {
		names = append(names, suite.Name)
	}
	return names
}

type QualifyOptions struct {
	State  State
	Runner ci.Runner
	Suite  string
	Args   []string
	Stdout io.Writer
	Stderr io.Writer
	Log    io.Writer
}

func Qualify(ctx context.Context, opts QualifyOptions) error {
	var suite Suite
	found := false
	for _, candidate := range Suites {
		if candidate.Name == opts.Suite {
			suite, found = candidate, true
		}
	}
	if !found {
		return fmt.Errorf("unknown suite %q; suites: %s", opts.Suite, strings.Join(SuiteNames(), ", "))
	}
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	var missing []string
	for _, tool := range suite.Tools {
		if _, err := opts.State.toolPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("suite %s requires %s; run infra dev setup or install them", suite.Name, strings.Join(missing, ", "))
	}
	root, err := filepath.Abs(opts.State.Root)
	if err != nil {
		return err
	}
	tools, err := filepath.Abs(opts.State.Tools())
	if err != nil {
		return err
	}
	env := append([]string{"PATH=" + opts.State.devPath(tools)}, suite.Env...)
	if suite.RootVariable != "" {
		env = append(env, suite.RootVariable+"="+root)
	}
	if suite.BinaryVariable != "" {
		binary, err := filepath.Abs(filepath.Join(opts.State.Bin(), "infra"))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
			return err
		}
		builder := opts.Runner
		builder.Env = append(append([]string{}, opts.Runner.Env...), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
		if err := builder.Run(ctx, "go", "build", "-trimpath", "-o", binary, "./cmd/infra"); err != nil {
			return fmt.Errorf("build qualification binary: %w", err)
		}
		fmt.Fprintln(opts.Log, "Built:", binary)
		env = append(env, suite.BinaryVariable+"="+binary)
	}
	if suite.Engine != "" {
		status, err := StartEngine(ctx, EngineOptions{State: opts.State, Runner: opts.Runner, Profile: EngineProfiles()[suite.Engine], Log: opts.Log})
		if err != nil {
			return err
		}
		env = append(env, "_EXPERIMENTAL_DAGGER_RUNNER_HOST="+status.RunnerHost)
	}
	arguments := append([]string{"test", "-count=1", "-v", "-run", suite.Test, suite.Package}, opts.Args...)
	fmt.Fprintln(opts.Log, "Suite:", suite.Name, strings.Join(arguments, " "))
	_, err = execute(ctx, opts.Runner, process.Options{Name: "go", Args: arguments, Env: env, Stdout: opts.Stdout, Stderr: opts.Stderr})
	return err
}
