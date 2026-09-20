package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
