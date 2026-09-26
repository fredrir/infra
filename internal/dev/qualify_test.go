package dev

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/process"
)

func TestQualifyRunsGatedSuitesWithTheirPrerequisites(t *testing.T) {
	root := toolchainRoot(t)
	state := NewState(root)
	t.Setenv("PATH", t.TempDir())
	for _, tool := range []string{"age-keygen", "sops", "helm", "kustomize", "tofu", "go"} {
		executable(t, filepath.Join(state.Tools(), tool))
	}
	docker := &fakeDocker{}
	runner := docker.runner(t, root)
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := filepath.Abs(state.Tools())
	if err != nil {
		t.Fatal(err)
	}
	var tests, builds []process.Options
	capturing := runner
	capturing.Execute = func(ctx context.Context, options process.Options) (process.Result, error) {
		if options.Name == "go" && options.Args[0] == "test" {
			tests = append(tests, options)
		}
		if options.Name == "go" && options.Args[0] == "build" {
			builds = append(builds, options)
		}
		return runner.Execute(ctx, options)
	}
	var stdout strings.Builder
	if err := Qualify(context.Background(), QualifyOptions{State: state, Runner: capturing, Suite: "onboarding", Args: []string{"-timeout=5m"}, Stdout: &stdout}); err != nil {
		t.Fatal(err)
	}
	if len(tests) != 1 || !slices.Equal(tests[0].Args, []string{"test", "-count=1", "-v", "-run", "^TestNativeOnboardingQualification$", "./internal/projects", "-timeout=5m"}) {
		t.Fatalf("unexpected go test invocation %+v", tests)
	}
	for _, entry := range []string{"INFRA_ONBOARD_INTEGRATION=1", "INFRA_TEST_SOURCE_ROOT=" + absoluteRoot} {
		if !slices.Contains(tests[0].Env, entry) {
			t.Errorf("onboarding environment lacks %s", entry)
		}
	}
	if !slices.ContainsFunc(tests[0].Env, func(entry string) bool { return strings.HasPrefix(entry, "PATH="+tools+":") }) {
		t.Errorf("pinned tools are not first on PATH: %v", tests[0].Env)
	}
	if docker.count("docker") != 0 {
		t.Fatalf("suite without an engine touched Docker: %v", docker.commands)
	}
	if err := Qualify(context.Background(), QualifyOptions{State: state, Runner: capturing, Suite: "kata"}); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(absoluteRoot, ".cache/dev/bin/infra")
	if docker.count("docker", "run") != 1 || docker.count("go", "build", "-trimpath", "-o", binary, "./cmd/infra") != 1 {
		t.Fatalf("engine or binary preparation missing: %v", docker.commands)
	}
	for _, entry := range []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"} {
		if !slices.Contains(builds[0].Env, entry) {
			t.Errorf("qualification binary build lacks %s", entry)
		}
	}
	if docker.image == "" || !slices.ContainsFunc(docker.commands, func(command []string) bool {
		return slices.Contains(command, "infra-dagger-dev-kata") && slices.Contains(command, "--pids-limit=256")
	}) {
		t.Fatalf("kata suite did not start the kata engine profile: %v", docker.commands)
	}
	for _, entry := range []string{"INFRA_KATA_ENGINE_TEST=1", "INFRA_KATA_BINARY=" + binary, "_EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://infra-dagger-dev-kata"} {
		if !slices.Contains(tests[1].Env, entry) {
			t.Errorf("kata environment lacks %s", entry)
		}
	}
	if err := Qualify(context.Background(), QualifyOptions{State: state, Runner: capturing, Suite: "packages"}); err == nil || !strings.Contains(err.Error(), "nfpm") {
		t.Fatalf("missing tools not reported: %v", err)
	}
	if err := Qualify(context.Background(), QualifyOptions{State: state, Runner: capturing, Suite: "unknown"}); err == nil || !strings.Contains(err.Error(), "onboarding, image, reconcile-plan, publishing, kustomize, packages, kata") {
		t.Fatalf("unknown suite not listed: %v", err)
	}
	if len(tests) != 2 {
		t.Fatalf("failed suites ran tests: %d", len(tests))
	}
}

func TestSuiteHostsAreDeclaredGuests(t *testing.T) {
	spec, err := readHostsSpec(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, suite := range Suites {
		for _, host := range suite.Hosts {
			if !slices.ContainsFunc(spec.Nodes, func(node HostSpec) bool { return node.Name == host }) {
				t.Errorf("suite %s needs undeclared guest %s", suite.Name, host)
			}
		}
	}
	if suite := Suites[slices.IndexFunc(Suites, func(suite Suite) bool { return suite.Name == "reconciler" })]; !slices.Equal(suite.Hosts, []string{"dev-reconciler-1"}) || suite.BinaryVariable == "" {
		t.Fatalf("reconciler suite %+v", suite)
	}
}
