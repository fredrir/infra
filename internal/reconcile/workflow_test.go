package reconcile

import (
	"cmp"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"
)

type workflowStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

type workflowJob struct {
	Name           string            `yaml:"name"`
	Environment    string            `yaml:"environment"`
	Uses           string            `yaml:"uses"`
	With           map[string]string `yaml:"with"`
	If             string            `yaml:"if"`
	TimeoutMinutes any               `yaml:"timeout-minutes"`
	Env            map[string]string `yaml:"env"`
	Permissions    map[string]string `yaml:"permissions"`
	Steps          []workflowStep    `yaml:"steps"`
}

type workflowInput struct {
	Type    string `yaml:"type"`
	Default any    `yaml:"default"`
}

type workflowFile struct {
	On   map[string]yaml.Node   `yaml:"on"`
	Env  map[string]string      `yaml:"env"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

func (w workflowFile) inputs(t *testing.T, trigger string) map[string]workflowInput {
	t.Helper()
	node, ok := w.On[trigger]
	if !ok {
		t.Fatalf("workflow is not triggered by %s", trigger)
	}
	var declaration struct {
		Inputs map[string]workflowInput `yaml:"inputs"`
	}
	if err := node.Decode(&declaration); err != nil {
		t.Fatal(err)
	}
	return declaration.Inputs
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

func conjunction(t *testing.T, condition string, facts map[string]bool) bool {
	t.Helper()
	expression := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(condition), "${{"), "}}"))
	if strings.Contains(expression, "||") {
		t.Fatalf("condition %q is not a conjunction", condition)
	}
	for _, term := range strings.Split(expression, "&&") {
		value, ok := facts[strings.TrimSpace(term)]
		if !ok {
			t.Fatalf("condition %q has unclassified term %q", condition, strings.TrimSpace(term))
		}
		if !value {
			return false
		}
	}
	return true
}

func TestHostedWorkflowsOnlyPlanInfrastructure(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("..", "..", ".github/workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"reconcile apply", "reconcile verify", "doppler", "infrastructure-apply", "fredrir-infra-verification", "RUNNER_APP", "PUBLISHER_APP_PRIVATE_KEY", "gh workflow run reconcile.yml", "schedule:"} {
			if forbidden == "doppler" && filepath.Base(path) == "project-images.yml" {
				continue
			}
			if strings.Contains(string(data), forbidden) && (forbidden != "schedule:" || strings.HasPrefix(filepath.Base(path), "reconcile")) {
				t.Errorf("%s contains %q", filepath.Base(path), forbidden)
			}
		}
	}
	job := readWorkflow(t, "reconcile-job.yml")
	if jobs := slices.Sorted(maps.Keys(job.Jobs)); !slices.Equal(jobs, []string{"plan"}) {
		t.Fatalf("reconcile-job.yml runs %v, want plan only", jobs)
	}
	if inputs := job.inputs(t, "workflow_call"); len(inputs) != 0 {
		t.Errorf("called inputs %+v", inputs)
	}
	caller := readWorkflow(t, "reconcile.yml")
	if triggers := slices.Sorted(maps.Keys(caller.On)); !slices.Equal(triggers, []string{"pull_request", "push"}) {
		t.Errorf("reconciliation triggers %v", triggers)
	}
	reconcile := caller.Jobs["reconcile"]
	if len(reconcile.With) != 0 || reconcile.If != "github.event_name == 'pull_request'" {
		t.Errorf("reconcile job runs when %q with %v", reconcile.If, reconcile.With)
	}
	for _, name := range []string{"reconcile.yml", "reconcile-job.yml"} {
		for jobName, definition := range readWorkflow(t, name).Jobs {
			if definition.Permissions["contents"] == "write" || definition.Permissions["actions"] == "write" {
				t.Errorf("%s job %s grants %v", name, jobName, definition.Permissions)
			}
		}
	}
}

func TestPlanReadsOnlyItsEnvironmentSecrets(t *testing.T) {
	t.Parallel()
	workflow := readWorkflow(t, "reconcile-job.yml")
	plan := workflow.Jobs["plan"]
	if plan.Environment != "infrastructure-plan" {
		t.Fatalf("plan environment %q", plan.Environment)
	}
	allowed := map[string]string{"AWS_ACCESS_KEY_ID": "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY": "AWS_SECRET_ACCESS_KEY", "CLOUDFLARE_API_TOKEN": "CLOUDFLARE_API_TOKEN", "TF_VAR_hcloud_token": "HCLOUD_TOKEN", "TF_VAR_platform_mail_recipient": "PLATFORM_MAIL_RECIPIENT", "KUBE_CONFIG": "KUBE_CONFIG"}
	secret := regexp.MustCompile(`\$\{\{\s*secrets\.([A-Z_]+)\s*\}\}`)
	read := map[string]bool{}
	for _, step := range plan.Steps {
		if strings.Contains(step.Uses, "doppler") || strings.Contains(step.Run, "doppler") {
			t.Errorf("plan step %q reads Doppler", cmp.Or(step.Name, step.Uses))
		}
		for name, value := range step.Env {
			if strings.Contains(value, "steps.doppler") {
				t.Errorf("plan step %q reads %s from Doppler", cmp.Or(step.Name, step.Uses), name)
			}
			if match := secret.FindStringSubmatch(value); match != nil {
				if allowed[name] != match[1] {
					t.Errorf("plan step %q maps secret %s to %s", cmp.Or(step.Name, step.Uses), match[1], name)
				}
				read[match[1]] = true
			}
		}
	}
	if want := []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "CLOUDFLARE_API_TOKEN", "HCLOUD_TOKEN", "KUBE_CONFIG", "PLATFORM_MAIL_RECIPIENT"}; !slices.Equal(slices.Sorted(maps.Keys(read)), want) {
		t.Errorf("plan reads secrets %v, want %v", slices.Sorted(maps.Keys(read)), want)
	}
}

func TestCalledReconciliationJobsRequestOnlyGrantedPermissions(t *testing.T) {
	t.Parallel()
	levels := map[string]int{"none": 0, "read": 1, "write": 2}
	caller := readWorkflow(t, "reconcile.yml").Jobs["reconcile"]
	for name, job := range readWorkflow(t, "reconcile-job.yml").Jobs {
		for scope, level := range job.Permissions {
			if levels[level] > levels[caller.Permissions[scope]] {
				t.Errorf("job %s requests %s: %s beyond the caller's %q", name, scope, level, caller.Permissions[scope])
			}
		}
	}
}

func TestSupersessionIgnoresExactlyThePushIgnoredPaths(t *testing.T) {
	t.Parallel()
	var push struct {
		PathsIgnore []string `yaml:"paths-ignore"`
	}
	trigger := readWorkflow(t, "reconcile.yml").On["push"]
	if err := trigger.Decode(&push); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pushIgnoredPaths, push.PathsIgnore) {
		t.Fatalf("supersession ignores %q, reconciliation pushes ignore %q", pushIgnoredPaths, push.PathsIgnore)
	}
	for _, pattern := range pushIgnoredPaths {
		if !doublestar.ValidatePattern(pattern) {
			t.Errorf("invalid pattern %q", pattern)
		}
	}
	for path, ignored := range map[string]bool{"README.md": true, "SECURITY.md": true, "docs/runbook.md": true, "docs/a/b.md": true, "build/evidence/run.json": true, "docs/diagram.svg": false, "platform/README.md": false, "build/evidence/a/run.json": false, "tofu/main.tf": false} {
		if PushIgnored(path) != ignored {
			t.Errorf("%s ignored=%t", path, !ignored)
		}
		if selected := Affected([]string{path}); ignored && (selected.Tofu || selected.Kubernetes || selected.Ansible || selected.Tooling) {
			t.Errorf("push-ignored %s selects %+v", path, selected)
		}
	}
}
