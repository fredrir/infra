package contracts

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"go.yaml.in/yaml/v3"
)

type checkStep struct {
	Run string `yaml:"run"`
}

type checkJob struct {
	Needs any         `yaml:"needs"`
	If    string      `yaml:"if"`
	Steps []checkStep `yaml:"steps"`
}

func (job checkJob) needs() []string {
	switch needs := job.Needs.(type) {
	case string:
		return []string{needs}
	case []any:
		var names []string
		for _, name := range needs {
			names = append(names, fmt.Sprint(name))
		}
		return names
	default:
		return nil
	}
}

func (job checkJob) runs(command string) bool {
	return slices.ContainsFunc(job.Steps, func(step checkStep) bool { return strings.Contains(step.Run, command) })
}

func TestDeclarationsRunAlongsideTheBazelCheckWithinOneAggregateBudget(t *testing.T) {
	var workflow struct {
		Jobs map[string]checkJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows/check.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	check, validate, budget := workflow.Jobs["check"], workflow.Jobs["validate"], workflow.Jobs["budget"]
	if !slices.Equal(validate.needs(), []string{"cli"}) {
		t.Fatalf("declarations wait for %v instead of running alongside the Bazel check", validate.needs())
	}
	if !slices.Contains(budget.needs(), "check") || !slices.Contains(budget.needs(), "validate") {
		t.Fatalf("aggregate budget needs %v", budget.needs())
	}
	for _, requirement := range []struct {
		job     checkJob
		name    string
		command string
	}{
		{check, "check", "ci measure --stage infra-fast --budget 10s --report-dir dist/reports/check-ledger"},
		{validate, "validate", `ci measure --stage declarations --budget 10s --report-dir "$RUNNER_TEMP/check-ledger"`},
		{budget, "budget", `ci check-budget --budget 10s --report-dir "$RUNNER_TEMP/check-ledger"`},
		{budget, "budget", `"$RUNNER_TEMP/reports/build-reports-$GITHUB_SHA"/check-ledger/*.json "$RUNNER_TEMP/reports/declaration-check-reports-$GITHUB_SHA"/check-ledger/*.json`},
	} {
		if !requirement.job.runs(requirement.command) {
			t.Errorf("%s does not run %s", requirement.name, requirement.command)
		}
	}
	type scenario struct {
		name, cli, artifact, check, validate, actor string
		cancelled, protected, allowed               bool
	}
	evaluate := func(program cel.Program, test scenario) {
		t.Helper()
		result, _, err := program.Eval(map[string]any{
			"github":          map[string]any{"repository_owner_id": "114402558", "repository_id": "1328085692", "ref_protected": test.protected, "actor": test.actor, "triggering_actor": test.actor},
			"inputs":          map[string]any{"cli-artifact": test.artifact},
			"needs":           map[string]any{"cli": map[string]any{"result": test.cli}, "check": map[string]any{"result": test.check}, "validate": map[string]any{"result": test.validate}},
			"cancelledStatus": test.cancelled,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result != types.Bool(test.allowed) {
			t.Errorf("%s: allowed = %v, want %v", test.name, result, test.allowed)
		}
	}
	validation := workflowCondition(t, validate.If)
	for _, test := range []scenario{
		{name: "built CLI while the Bazel check still runs", cli: "success", check: "", actor: "fredrir", protected: true, allowed: true},
		{name: "reused CLI artifact", cli: "skipped", artifact: "infra-cli", actor: "fredrir", protected: true, allowed: true},
		{name: "failed CLI build", cli: "failure", actor: "fredrir", protected: true},
		{name: "cancelled run", cli: "success", actor: "fredrir", protected: true, cancelled: true},
		{name: "unprotected reference", cli: "success", actor: "fredrir"},
		{name: "verification bot", cli: "success", actor: "fredrir-infra-verification[bot]", protected: true},
		{name: "deployment bot", cli: "success", actor: "octo-sts[bot]", protected: true},
	} {
		evaluate(validation, test)
	}
	aggregate := workflowCondition(t, budget.If)
	for _, test := range []scenario{
		{name: "both stages passed", check: "success", validate: "success", allowed: true},
		{name: "Bazel check failed", check: "failure", validate: "success"},
		{name: "declarations failed", check: "success", validate: "failure"},
		{name: "declarations skipped", check: "success", validate: "skipped"},
		{name: "cancelled run", check: "success", validate: "success", cancelled: true},
		{name: "verification bot", check: "success", validate: "success", actor: "fredrir-infra-verification[bot]"},
	} {
		evaluate(aggregate, test)
	}
}

func TestBazelCacheIsSavedAfterFailedMainChecksButNeverFromPullRequests(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string `yaml:"uses"`
				If   string `yaml:"if"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows/check.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	var saves []string
	for _, step := range workflow.Jobs["check"].Steps {
		if strings.HasPrefix(step.Uses, "actions/cache/save@") {
			saves = append(saves, step.If)
		}
	}
	if len(saves) != 1 {
		t.Fatalf("check job saves the Bazel cache %d times", len(saves))
	}
	save := workflowCondition(t, saves[0])
	for _, test := range []struct {
		name, event, ref, expression, key   string
		protected, failed, cancelled, saved bool
	}{
		{name: "passing main push", event: "push", ref: "refs/heads/main", expression: "//...", key: "cache", protected: true, saved: true},
		{name: "failed main push", event: "push", ref: "refs/heads/main", expression: "//...", key: "cache", protected: true, failed: true, saved: true},
		{name: "manual main run", event: "workflow_dispatch", ref: "refs/heads/main", expression: "//...", key: "cache", protected: true, saved: true},
		{name: "cancelled main push", event: "push", ref: "refs/heads/main", expression: "//...", key: "cache", protected: true, cancelled: true},
		{name: "pull request", event: "pull_request", ref: "refs/pull/7/merge", expression: "//...", key: "cache"},
		{name: "failed pull request", event: "pull_request", ref: "refs/pull/7/merge", expression: "//...", key: "cache", failed: true},
		{name: "push to another branch", event: "push", ref: "refs/heads/feature", expression: "//...", key: "cache"},
		{name: "no affected targets", event: "push", ref: "refs/heads/main", expression: "set()", key: "cache", protected: true},
		{name: "failure before the cache restore", event: "push", ref: "refs/heads/main", expression: "//...", protected: true, failed: true},
	} {
		result, _, err := save.Eval(map[string]any{
			"github":          map[string]any{"repository": "fredrir/infra", "event_name": test.event, "ref": test.ref, "ref_protected": test.protected},
			"steps":           map[string]any{"affected": map[string]any{"outputs": map[string]any{"expression": test.expression}}, "bazel-cache": map[string]any{"outputs": map[string]any{"cache-primary-key": test.key}}},
			"failedStatus":    test.failed,
			"cancelledStatus": test.cancelled,
		})
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if result != types.Bool(test.saved) {
			t.Errorf("%s: saved = %v, want %v", test.name, result, test.saved)
		}
	}
}

func TestReleaseChecksRestoreTheTaggedCommitsMainCheckFirst(t *testing.T) {
	type cacheStep struct {
		Uses string `yaml:"uses"`
		With struct {
			Key         string `yaml:"key"`
			RestoreKeys string `yaml:"restore-keys"`
		} `yaml:"with"`
	}
	var check, release struct {
		Jobs map[string]struct {
			Steps []cacheStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	for path, workflow := range map[string]any{".github/workflows/check.yml": &check, ".github/workflows/cli-release.yml": &release} {
		if err := yaml.Unmarshal(read(t, filepath.Join(root(t), path)), workflow); err != nil {
			t.Fatal(err)
		}
	}
	cache := func(steps []cacheStep, action string) cacheStep {
		t.Helper()
		for _, step := range steps {
			if strings.HasPrefix(step.Uses, action) {
				return step
			}
		}
		t.Fatalf("no %s step", action)
		return cacheStep{}
	}
	restored := cache(check.Jobs["check"].Steps, "actions/cache/restore@")
	if !strings.HasSuffix(restored.With.Key, "-check-${{ github.sha }}-${{ github.run_id }}-${{ github.run_attempt }}") {
		t.Fatalf("check cache key %q does not name the checked commit", restored.With.Key)
	}
	restoreKeys := strings.Split(strings.TrimSpace(cache(release.Jobs["build"].Steps, "actions/cache@").With.RestoreKeys), "\n")
	if !strings.Contains(restoreKeys[0], "format('infra-bazel-linux-amd64-v1-{0}-check-{1}-'") || !strings.Contains(restoreKeys[0], "github.sha") {
		t.Fatalf("release restores %q before the tagged commit's main check cache", restoreKeys[0])
	}
}

var (
	statusFunction     = regexp.MustCompile(`\b(success|failure|cancelled|always)\(\)`)
	hyphenatedProperty = regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_]*(?:-[A-Za-z0-9_]+)+)`)
)

func workflowCondition(t *testing.T, condition string) cel.Program {
	t.Helper()
	expression := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(condition), "${{"), "}}"))
	if !statusFunction.MatchString(expression) {
		expression = "success() && (" + expression + ")"
	}
	expression = strings.NewReplacer("success()", "(!failedStatus && !cancelledStatus)", "failure()", "failedStatus", "cancelled()", "cancelledStatus", "always()", "true").Replace(expression)
	expression = hyphenatedProperty.ReplaceAllString(expression, "['$1']")
	environment, err := cel.NewEnv(
		cel.Variable("github", cel.DynType),
		cel.Variable("inputs", cel.DynType),
		cel.Variable("needs", cel.DynType),
		cel.Variable("steps", cel.DynType),
		cel.Variable("failedStatus", cel.BoolType),
		cel.Variable("cancelledStatus", cel.BoolType),
		cel.Function("fromJSON", cel.Overload("fromJSON_string", []*cel.Type{cel.StringType}, cel.DynType, cel.UnaryBinding(func(value ref.Val) ref.Val {
			var decoded any
			if err := json.Unmarshal([]byte(value.(types.String)), &decoded); err != nil {
				return types.NewErr("invalid JSON: %v", err)
			}
			return types.DefaultTypeAdapter.NativeToValue(decoded)
		}))),
		cel.Function("contains", cel.Overload("contains_list", []*cel.Type{cel.ListType(cel.DynType), cel.DynType}, cel.BoolType, cel.BinaryBinding(func(values, value ref.Val) ref.Val {
			return values.(traits.Container).Contains(value)
		}))),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := environment.Compile(expression)
	if issues.Err() != nil {
		t.Fatalf("%s: %v", expression, issues.Err())
	}
	program, err := environment.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	return program
}
