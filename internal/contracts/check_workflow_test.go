package contracts

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type checkStep struct {
	Run string `yaml:"run"`
}

type checkJob struct {
	Steps []checkStep `yaml:"steps"`
}

func (job checkJob) runs(command string) bool {
	return slices.ContainsFunc(job.Steps, func(step checkStep) bool { return strings.Contains(step.Run, command) })
}

func TestBazelCheckRunsWithinTheCheckBudget(t *testing.T) {
	var workflow struct {
		Jobs map[string]checkJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows/check.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	if !workflow.Jobs["check"].runs("ci measure --stage infra-fast --budget 10s --report-dir dist/reports/check-ledger") {
		t.Fatal("check does not measure the Bazel check against the ten-second budget")
	}
}

func TestCompiledPromotionChecksAreReusedOnlyForMatchingCodeAndFreshPlatformData(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows/check.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	steps := workflow.Jobs["check"].Steps
	_, restore := stepByID(t, steps, "promotion-cache")
	_, verified := stepByID(t, steps, "promotion")
	condition := workflowCondition(t, restore.If)
	for expression, expected := range map[string]bool{
		"rdeps(//..., set(//platform:promotion_testdata))":                            true,
		"rdeps(//..., set(//platform:policy_testdata //platform:promotion_testdata))": false,
		"rdeps(//..., set(//internal/platformops:all))":                               false,
		"//...": false,
		"set()": false,
	} {
		allowed, err := condition.allows(map[string]any{"steps": map[string]any{"affected": map[string]any{"outputs": map[string]any{"expression": expression}}}})
		if err != nil || allowed != expected {
			t.Fatalf("reuse for %s = %v, %v", expression, allowed, err)
		}
	}
	for _, input := range []string{"internal/**", "cmd/**", "go.mod", "go.sum", "MODULE.bazel", "MODULE.bazel.lock", "**/BUILD.bazel", "**/*.bzl", ".bazelrc", ".bazelversion", "build/toolchain.json", "runner.os", "runner.arch"} {
		if !strings.Contains(restore.With["key"], input) {
			t.Errorf("checker cache omits %s", input)
		}
	}
	if restore.With["restore-keys"] != "" || !strings.Contains(verified.Run, "sha256sum --check --strict SHA256SUMS") {
		t.Fatal("checker cache requires an exact key and valid checksum")
	}
	for _, step := range steps {
		if step.Name == "Check affected targets within aggregate budget" {
			if !strings.Contains(step.Run, `"$RUNNER_TEMP/promotion-check/platformops_test" -test.timeout=10s`) || !strings.Contains(step.Run, "infra pipeline check-fast") {
				t.Fatal("cached checks must execute and retain the uncached path")
			}
		}
		if strings.HasPrefix(step.Uses, "actions/cache/save@") && step.With["path"] == restore.With["path"] {
			writer := workflowCondition(t, step.If)
			for _, identity := range []string{"bazel-cache-check-writer", "bazel-cache-reader", ""} {
				allowed, err := writer.allows(map[string]any{"env": map[string]any{"BAZEL_CACHE_IDENTITY": identity}, "steps": map[string]any{"promotion-store": map[string]any{"outputs": map[string]any{"ready": "true"}}}})
				if err != nil || allowed != (identity == "bazel-cache-check-writer") {
					t.Fatalf("cache writer %s = %v, %v", identity, allowed, err)
				}
			}
		}
	}
}
