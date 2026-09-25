package reconcile

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/itchyny/gojq"
	"go.yaml.in/yaml/v3"
)

type workflowStep struct {
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	Run  string            `yaml:"run"`
}

type workflowJob struct {
	If          string            `yaml:"if"`
	Env         map[string]string `yaml:"env"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowFile struct {
	On struct {
		Schedule []struct {
			Cron string `yaml:"cron"`
		} `yaml:"schedule"`
		Dispatch struct {
			Inputs map[string]struct {
				Type string `yaml:"type"`
			} `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
	} `yaml:"on"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) workflowFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github/workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var workflow workflowFile
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func (j workflowJob) step(t *testing.T, match func(workflowStep) bool) workflowStep {
	t.Helper()
	for _, step := range j.Steps {
		if match(step) {
			return step
		}
	}
	t.Fatal("workflow step not found")
	return workflowStep{}
}

func (j workflowJob) script() string {
	var script strings.Builder
	for _, step := range j.Steps {
		script.WriteString(step.Run)
	}
	return script.String()
}

func TestDriftRepairDispatchIsScopedToHourlyVerification(t *testing.T) {
	workflow := readWorkflow(t, "reconcile-job.yml")
	repair, ok := workflow.Jobs["repair"]
	if !ok || !reflect.DeepEqual(repair.Permissions, map[string]string{"actions": "write"}) {
		t.Fatalf("repair job permissions are not exactly actions: write: %+v", repair.Permissions)
	}
	for _, condition := range []string{"github.event.schedule == '47 * * * *'", "needs.apply.result == 'failure'", "!cancelled()", "needs.apply.outputs.differences == 'true'"} {
		if !strings.Contains(repair.If, condition) || strings.Contains(repair.If, "||") {
			t.Errorf("repair job condition %q does not require %s", repair.If, condition)
		}
	}
	for _, guard := range []string{"--workflow reconcile.yml", "--event workflow_dispatch", "--user 'github-actions[bot]'", `--commit "$GITHUB_SHA"`, "--limit 1", "--json conclusion,createdAt"} {
		if !strings.Contains(repair.script(), guard) {
			t.Errorf("repair dispatch lacks guard %s:\n%s", guard, repair.script())
		}
	}
	for name, job := range workflow.Jobs {
		if name != "repair" && job.Permissions["actions"] == "write" {
			t.Errorf("job %s can dispatch workflows", name)
		}
	}
	if verification := workflow.Jobs["apply"].Env["VERIFICATION"]; verification != "${{ github.event.schedule == '47 * * * *' || inputs.verify }}" {
		t.Errorf("hourly and dispatched verification select %q", verification)
	}
	trigger := readWorkflow(t, "reconcile.yml")
	if len(trigger.On.Schedule) != 1 || trigger.On.Schedule[0].Cron != "47 * * * *" || trigger.On.Dispatch.Inputs["verify"].Type != "boolean" {
		t.Errorf("reconciliation is scheduled beyond hourly verification or lacks on-demand verification: %+v", trigger.On)
	}
}

func TestRepairCapFollowsLatestDispatchedReconciliation(t *testing.T) {
	script := readWorkflow(t, "reconcile-job.yml").Jobs["repair"].script()
	match := regexp.MustCompile(`--jq '([^']+)'`).FindStringSubmatch(script)
	if match == nil {
		t.Fatalf("repair dispatch has no cap:\n%s", script)
	}
	query, err := gojq.Parse(match[1])
	if err != nil {
		t.Fatal(err)
	}
	program, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		conclusion string
		age        time.Duration
		dispatch   bool
	}{
		{name: "failed a week ago", conclusion: "failure", age: 7 * 24 * time.Hour},
		{name: "timed out a week ago", conclusion: "timed_out", age: 7 * 24 * time.Hour},
		{name: "failed to start a week ago", conclusion: "startup_failure", age: 7 * 24 * time.Hour},
		{name: "succeeded within six hours", conclusion: "success", age: time.Hour},
		{name: "in progress", age: time.Hour},
		{name: "succeeded over six hours ago", conclusion: "success", age: 7 * time.Hour, dispatch: true},
		{name: "cancelled within six hours", conclusion: "cancelled", age: time.Hour, dispatch: true},
		{name: "cancelled over six hours ago", conclusion: "cancelled", age: 7 * time.Hour, dispatch: true},
		{name: "never dispatched", dispatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runs := []any{}
			if test.age > 0 {
				runs = append(runs, map[string]any{"conclusion": test.conclusion, "createdAt": time.Now().Add(-test.age).UTC().Format(time.RFC3339)})
			}
			var suppressed []any
			results := program.Run(runs)
			for {
				result, ok := results.Next()
				if !ok {
					break
				}
				if err, ok := result.(error); ok {
					t.Fatal(err)
				}
				suppressed = append(suppressed, result)
			}
			if (len(suppressed) == 0) != test.dispatch {
				t.Fatalf("previous reconciliation %+v suppressed the repair with %q", runs, suppressed)
			}
		})
	}
}

func TestVerificationRunsWithReadOnlyCredentials(t *testing.T) {
	apply := readWorkflow(t, "reconcile-job.yml").Jobs["apply"]
	checkout := apply.step(t, func(step workflowStep) bool { return strings.HasPrefix(step.Uses, "actions/checkout@") })
	if persisted := checkout.With["persist-credentials"]; persisted != "${{ env.VERIFICATION != 'true' }}" {
		t.Errorf("verification checkout persists credentials with %q", persisted)
	}
	token := apply.step(t, func(step workflowStep) bool { return strings.HasPrefix(step.Uses, "actions/create-github-app-token@") })
	if administration := token.With["permission-administration"]; administration != "${{ env.VERIFICATION == 'true' && 'read' || 'write' }}" {
		t.Errorf("runner registration token administration permission is %q", administration)
	}
	setup := apply.step(t, func(step workflowStep) bool { return step.ID == "setup" })
	if full := setup.With["full"]; full != "${{ env.VERIFICATION == 'true' || inputs.full }}" {
		t.Errorf("verification prepares tooling with full=%q", full)
	}
	if check := readWorkflow(t, "reconcile.yml").Jobs["check"].If; check != "github.event_name != 'schedule' && !inputs.verify" {
		t.Errorf("verification runs repository checks with condition %q", check)
	}
}
