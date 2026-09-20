package ci

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestProjectInputsPreserveSharedDependenciesAndUnknownChanges(t *testing.T) {
	config := ProjectInputs{Schema: 1, Shared: []string{"Cargo.lock"}, Ignored: []string{"docs/", "Cargo.lock", "api/"}, Targets: map[string][]string{"api": {"api/", "common/"}, "web": {"web/", "common/"}}}
	for _, test := range []struct {
		name    string
		paths   []string
		changed bool
		targets []string
		reason  string
	}{
		{"unchanged", nil, false, []string{}, "unaffected"},
		{"documentation", []string{"docs/readme.md"}, false, []string{}, "unaffected"},
		{"other target", []string{"web/app.js"}, false, []string{"web"}, "unaffected"},
		{"target overrides ignore", []string{"api/main.go"}, true, []string{"api"}, "target-input"},
		{"shared overrides ignore", []string{"Cargo.lock"}, true, []string{"api", "web"}, "shared-input"},
		{"common dependency", []string{"common/types.go"}, true, []string{"api", "web"}, "target-input"},
		{"unknown", []string{"toolchain.toml"}, true, []string{"api", "web"}, "unmapped-input"},
		{"prefix boundary", []string{"api-backup/file.go"}, true, []string{"api", "web"}, "unmapped-input"},
		{"changed configuration", []string{"ci/targets.json"}, true, []string{"api", "web"}, "configuration-changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := projectInputsForPaths(config, "ci/targets.json", "api", test.paths)
			if got.Changed != test.changed || got.Reason != test.reason || !reflect.DeepEqual(got.AffectedTargets, test.targets) {
				t.Fatalf("plan=%+v, want changed=%t reason=%s targets=%v", got, test.changed, test.reason, test.targets)
			}
		})
	}
}

func TestProjectInputDiffIncludesDeletedAndRenamedSources(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, data)
		}
		return string(data)
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	config := ProjectInputs{Schema: 1, Targets: map[string][]string{"api": {"api/"}, "web": {"web/"}}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	write("ci/targets.json", data)
	write("api/main.go", []byte("package example\n"))
	git("init", "--quiet")
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "fixture")
	for _, base := range []string{"", "missing-revision"} {
		plan, err := PlanProjectInputs(context.Background(), root, "ci/targets.json", base, "api")
		if err != nil || !plan.Changed || len(plan.AffectedTargets) != 2 {
			t.Fatalf("base=%q plan=%+v error=%v", base, plan, err)
		}
	}
	write(".infra-build-recipe/BUILD.bazel", []byte("helper checkout\n"))
	write(".git/info/exclude", []byte("/.infra-build-recipe/\n"))
	plan, err := PlanProjectInputs(context.Background(), root, "ci/targets.json", "HEAD", "api")
	if err != nil || plan.Changed {
		t.Fatalf("selection included excluded helper checkout: %+v %v", plan, err)
	}
	write("untracked.conf", []byte("unknown input\n"))
	plan, err = PlanProjectInputs(context.Background(), root, "ci/targets.json", "HEAD", "api")
	if err != nil || !plan.Changed || plan.Reason != "unmapped-input" {
		t.Fatalf("selection missed unknown untracked input: %+v %v", plan, err)
	}
	if err := os.Remove(filepath.Join(root, "untracked.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "api/main.go")); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanProjectInputs(context.Background(), root, "ci/targets.json", "HEAD", "api")
	if err != nil || !plan.Changed || !reflect.DeepEqual(plan.AffectedTargets, []string{"api"}) {
		t.Fatalf("deleted input missed: %+v %v", plan, err)
	}
	write("web/main.go", []byte("package example\n"))
	git("add", "--all")
	plan, err = PlanProjectInputs(context.Background(), root, "ci/targets.json", "HEAD", "api")
	if err != nil || !reflect.DeepEqual(plan.AffectedTargets, []string{"api", "web"}) {
		t.Fatalf("rename failed to invalidate both targets: %+v %v", plan, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PlanProjectInputs(ctx, root, "ci/targets.json", "HEAD", "api"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled selection returned %v", err)
	}
}

func TestProjectInputConfigurationRejectsAmbiguousPatterns(t *testing.T) {
	for _, pattern := range []string{"", ".", "../src", "/src", "src/../api", "src/**", "src/*.go", "src\\api", "src\napi"} {
		config := ProjectInputs{Schema: 1, Targets: map[string][]string{"api": {pattern}}}
		if err := validateProjectInputs(config, "api"); err == nil {
			t.Errorf("accepted ambiguous input %q", pattern)
		}
	}
	config := ProjectInputs{Schema: 1, Targets: map[string][]string{"api": {"api/"}}}
	if err := validateProjectInputs(config, "missing"); err == nil {
		t.Fatal("unknown target accepted")
	}
}
