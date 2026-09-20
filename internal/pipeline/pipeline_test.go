package pipeline_test

import (
	"context"
	"encoding/json"
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
	for _, path := range []string{"go.mod", "MODULE.bazel", "images/catalog.yaml", "internal/deleted/old.go", "internal/example/quote\".go"} {
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
