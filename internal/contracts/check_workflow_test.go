package contracts

import (
	"fmt"
	"os/exec"
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
		{budget, "budget", `"$RUNNER_TEMP/reports/build"/check-ledger/*.json "$RUNNER_TEMP/reports/declarations"/check-ledger/*.json`},
	} {
		if !requirement.job.runs(requirement.command) {
			t.Errorf("%s does not run %s", requirement.name, requirement.command)
		}
	}
	type scenario struct {
		name, cli, artifact, check, validate, actor string
		cancelled, protected, allowed               bool
	}
	evaluate := func(condition workflowExpression, test scenario) {
		t.Helper()
		allowed, err := condition.allows(map[string]any{
			"github":          map[string]any{"repository_owner_id": "114402558", "repository_id": "1328085692", "ref_protected": test.protected, "actor": test.actor, "triggering_actor": test.actor},
			"inputs":          map[string]any{"cli-artifact": test.artifact},
			"needs":           map[string]any{"cli": map[string]any{"result": test.cli}, "check": map[string]any{"result": test.check}, "validate": map[string]any{"result": test.validate}},
			"cancelledStatus": test.cancelled,
		})
		if err != nil {
			t.Fatal(err)
		}
		if allowed != test.allowed {
			t.Errorf("%s: allowed = %v, want %v", test.name, allowed, test.allowed)
		}
	}
	validation := workflowCondition(t, validate.If)
	for _, test := range []scenario{
		{name: "built CLI while the Bazel check still runs", cli: "success", check: "", actor: "fredrir", protected: true, allowed: true},
		{name: "reused CLI artifact", cli: "skipped", artifact: "infra-cli", actor: "fredrir", protected: true, allowed: true},
		{name: "failed CLI build", cli: "failure", actor: "fredrir", protected: true},
		{name: "cancelled run", cli: "success", actor: "fredrir", protected: true, cancelled: true},
		{name: "unprotected reference", cli: "success", actor: "fredrir"},
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
	} {
		evaluate(aggregate, test)
	}
}
func TestCheckBudgetSelectsEachDependencyReportAcrossReruns(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Outputs map[string]string `yaml:"outputs"`
			Steps   []workflowStep    `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows/check.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	for _, job := range []string{"check", "validate"} {
		_, upload := stepByID(t, workflow.Jobs[job].Steps, "reports")
		if !strings.HasPrefix(upload.Uses, "actions/upload-artifact@") || !strings.Contains(upload.With["name"], "${{ github.run_attempt }}") || upload.With["overwrite"] == "true" {
			t.Fatalf("%s must preserve a separately named report from every attempt", job)
		}
	}
	var guard workflowStep
	downloads := map[string]workflowStep{}
	for _, step := range workflow.Jobs["budget"].Steps {
		if step.Name == "Require check report IDs" {
			guard = step
		}
		if strings.HasPrefix(step.Uses, "actions/download-artifact@") && strings.Contains(step.With["path"], "/reports/") {
			downloads[filepath.Base(step.With["path"])] = step
		}
	}
	if guard.Run == "" || len(downloads) != 2 {
		t.Fatal("two checked dependency report downloads required")
	}
	for _, test := range []struct {
		name, check, validate string
		valid                 bool
	}{
		{"first attempt", "10963905876", "10962973927", true},
		{"check rerun with a smaller artifact ID", "10963168708", "10962973927", true},
		{"declarations rerun with a smaller artifact ID", "10963905876", "10962970000", true},
		{"check upload missing", "", "10962973927", false},
		{"declarations upload missing", "10963168708", "", false},
		{"both uploads missing", "", "", false},
		{"invalid artifact ID", "1,2", "10962973927", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			needs := map[string]any{}
			for job, id := range map[string]string{"check": test.check, "validate": test.validate} {
				value, err := workflowValue(t, workflow.Jobs[job].Outputs["report-artifact"]).value(map[string]any{
					"steps": map[string]any{"reports": map[string]any{"outputs": map[string]any{"artifact-id": id}}},
				})
				if err != nil || value != id {
					t.Fatalf("%s report output = %v, %v; want %s", job, value, err, id)
				}
				needs[job] = map[string]any{"outputs": map[string]any{"report-artifact": value}}
			}
			context := map[string]any{"needs": needs}
			command := exec.Command("bash", "-eu", "-c", guard.Run)
			for key, expression := range guard.Env {
				value, err := workflowValue(t, expression).value(context)
				if err != nil {
					t.Fatal(err)
				}
				command.Env = append(command.Env, key+"="+fmt.Sprint(value))
			}
			if err := command.Run(); (err == nil) != test.valid {
				t.Fatalf("report ID validation = %v, want valid %v", err, test.valid)
			}
			for path, id := range map[string]string{"build": test.check, "declarations": test.validate} {
				step := downloads[path]
				value, err := workflowValue(t, step.With["artifact-ids"]).value(context)
				if err != nil || value != id || step.With["name"] != "" || step.With["pattern"] != "" {
					t.Fatalf("%s download selected %v, %v; want dependency artifact %s", path, value, err, id)
				}
			}
		})
	}
}
