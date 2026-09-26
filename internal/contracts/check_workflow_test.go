package contracts

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
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
	environment, err := cel.NewEnv(cel.Variable("github", cel.DynType), cel.Variable("inputs", cel.DynType), cel.Variable("needs", cel.DynType), cel.Variable("cancelledStatus", cel.BoolType))
	if err != nil {
		t.Fatal(err)
	}
	condition := func(job checkJob) cel.Program {
		expression := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(job.If, "${{"), "}}"))
		expression = strings.NewReplacer("cancelled()", "cancelledStatus", "inputs.cli-artifact", "inputs['cli-artifact']").Replace(expression)
		ast, issues := environment.Compile(expression)
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}
		program, err := environment.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return program
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
	validation := condition(validate)
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
	aggregate := condition(budget)
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
