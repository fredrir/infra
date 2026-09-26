package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/pipeline"
)

func TestChangedPackageIncludesDependentsAndGlobalInputsSelectEverything(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "example")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "BUILD.bazel"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := pipeline.ExpressionForPaths(root, []string{"internal/example/change.go", "internal/example/change_test.go"})
	if got != "rdeps(//..., set(//internal/example:all))" {
		t.Fatalf("dependent packages omitted: %s", got)
	}
	for _, path := range []string{"go.mod", "MODULE.bazel", "internal/deleted/old.go", "internal/example/quote\".go"} {
		if got := pipeline.ExpressionForPaths(root, []string{path}); got != "//..." {
			t.Errorf("%s must conservatively select everything: %s", path, got)
		}
	}
	if got := pipeline.ExpressionForPaths(root, nil); got != "set()" {
		t.Fatalf("empty change selected %s", got)
	}
}

func TestGeneratedBuildFilesAreCheckedForEveryGazelleInput(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"internal/example/example.go", "ansible/roles/k3s/tasks/main.yml"} {
		writeFile(t, filepath.Join(root, path), "")
	}
	for _, test := range []struct {
		path    string
		changed bool
	}{
		{"internal/example/example.go", true},
		{"internal/deleted/old.go", true},
		{"internal/example/testdata/fixture.json", true},
		{"internal/example/embedded.txt", true},
		{"platform/BUILD.bazel", true},
		{"images/rules.bzl", true},
		{"go.mod", true},
		{"MODULE.bazel", true},
		{"docs/../../go.mod", true},
		{"ansible/roles/k3s/tasks/main.yml", false},
		{"tofu/worker-04.tf", false},
		{"platform/components/policy/runner.yaml", false},
		{"", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			if got := pipeline.GeneratedBuildInputsChanged(root, []string{test.path}); got != test.changed {
				t.Fatalf("generated BUILD inputs changed = %t, want %t", got, test.changed)
			}
		})
	}
}

func TestGeneratedBuildCheckSkipsChangesOutsideGoPackages(t *testing.T) {
	root := t.TempDir()
	digest := strings.Repeat("a", 64)
	writeFile(t, filepath.Join(root, ".bazelversion"), "9.2.0\n")
	writeFile(t, filepath.Join(root, "build", "toolchain.json"), `{"bazel":"9.2.0","go":"1.27.1","dagger":"0.21.9","image":"golang@sha256:`+digest+`","bazel_sha256":"`+digest+`","engine_image":"registry.dagger.io/engine:v0.21.9@sha256:`+digest+`"}`)
	writeFile(t, filepath.Join(root, "internal", "example", "example.go"), "package example\n")
	writeFile(t, filepath.Join(root, "ansible", "site.yml"), "[]\n")
	git := func(arguments ...string) string {
		command := exec.Command("git", append([]string{"-c", "user.name=check", "-c", "user.email=check@example.com", "-c", "commit.gpgsign=false"}, arguments...)...)
		command.Dir, command.Env = root, append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet")
	git("add", ".")
	git("commit", "--quiet", "--message", "base")
	base := git("rev-parse", "HEAD")
	check := func() error {
		_, err := pipeline.Run(context.Background(), pipeline.Options{Root: root, Local: true, Bazel: filepath.Join(root, "missing-bazel"), Operation: "generate-check", Base: base})
		return err
	}
	writeFile(t, filepath.Join(root, "ansible", "site.yml"), "- hosts: all\n")
	if err := check(); err != nil {
		t.Fatalf("playbook change ran the generated BUILD check: %v", err)
	}
	writeFile(t, filepath.Join(root, "internal", "example", "example.go"), "package example\n\nconst changed = true\n")
	if err := check(); err == nil || !strings.Contains(err.Error(), "read Bazel version") {
		t.Fatalf("Go change skipped the generated BUILD check: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationFailureStillProducesReport(t *testing.T) {
	root := t.TempDir()
	report, err := pipeline.Run(context.Background(), pipeline.Options{Root: root, Operation: "test", Local: true})
	if err == nil || report.Success {
		t.Fatal("missing toolchain must fail")
	}
	data, err := os.ReadFile(filepath.Join(root, "dist", "reports", "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved pipeline.Report
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Success || saved.Error == "" || saved.Operation != "test" {
		t.Fatalf("incorrect report: %+v", saved)
	}
}

func TestImageRejectsEscapingDockerfileBeforeConnecting(t *testing.T) {
	_, err := pipeline.Image(context.Background(), pipeline.ImageOptions{Image: "registry.test/image:tag", Dockerfile: "../Dockerfile"})
	if err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("escaping Dockerfile accepted: %v", err)
	}
}

func TestNonGoInputsSelectOnlyDeclaredDataDependents(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
	}{
		{"docs/platform.md", "set()"},
		{"README.md", "set()"},
		{"build/evidence/run.json", "set()"},
		{"build/rollout/project.patch", "set()"},
		{".github/deployments/123.yaml", "set()"},
		{"platform/projects/example/release.yaml", "rdeps(//..., set(//platform:promotion_testdata))"},
		{"platform/components/runners/check/runner.yaml", "rdeps(//..., set(//platform:policy_testdata //platform:promotion_testdata))"},
		{"platform/components/backups/tools.Dockerfile", "rdeps(//..., set(//:image_contract_data //platform:promotion_testdata))"},
		{".github/workflows/check.yml", "rdeps(//..., set(//:image_contract_data))"},
		{"images/catalog.yaml", "rdeps(//..., set(//:image_contract_data))"},
		{"unknown/input.json", "//..."},
		{"platform/BUILD.bazel", "//..."},
		{"images/BUILD.bazel", "//..."},
		{"internal/new/BUILD.bazel", "//..."},
		{"images/new_rule.bzl", "//..."},
		{"docs/../../go.mod", "//..."},
		{"docs/BUILD.bazel", "//..."},
		{"build/evidence/BUILD.bazel", "//..."},
	} {
		t.Run(test.path, func(t *testing.T) {
			if got := pipeline.ExpressionForPaths(t.TempDir(), []string{test.path}); got != test.want {
				t.Fatalf("selection = %s, want %s", got, test.want)
			}
		})
	}
}

func TestFastChecksNeverReportCancellationAsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reports, err := pipeline.CheckFast(ctx, pipeline.Options{Root: t.TempDir(), Local: true})
	if !errors.Is(err, context.Canceled) || len(reports) != 1 || reports[0].Success {
		t.Fatalf("cancelled checks passed: reports=%+v error=%v", reports, err)
	}
}
