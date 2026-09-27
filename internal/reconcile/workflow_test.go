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

func TestHostedWorkflowsNeitherApplyNorRepair(t *testing.T) {
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
		for _, forbidden := range []string{"reconcile apply", "RUNNER_APP", "PUBLISHER_APP_PRIVATE_KEY", "gh workflow run reconcile.yml", "schedule:"} {
			if strings.Contains(string(data), forbidden) && (forbidden != "schedule:" || strings.HasPrefix(filepath.Base(path), "reconcile")) {
				t.Errorf("%s contains %q", filepath.Base(path), forbidden)
			}
		}
	}
	job := readWorkflow(t, "reconcile-job.yml")
	if jobs := slices.Sorted(maps.Keys(job.Jobs)); !slices.Equal(jobs, []string{"plan", "verify"}) {
		t.Fatalf("reconcile-job.yml runs %v, want plan and verify only", jobs)
	}
	if inputs := job.inputs(t, "workflow_call"); !reflect.DeepEqual(inputs, map[string]workflowInput{"verify": {Type: "boolean", Default: false}}) {
		t.Errorf("called inputs %+v", inputs)
	}
	caller := readWorkflow(t, "reconcile.yml")
	if triggers := slices.Sorted(maps.Keys(caller.On)); !slices.Equal(triggers, []string{"pull_request", "push", "workflow_dispatch"}) {
		t.Errorf("reconciliation triggers %v", triggers)
	}
	if inputs := caller.inputs(t, "workflow_dispatch"); !reflect.DeepEqual(inputs, map[string]workflowInput{"verify": {Type: "boolean", Default: false}, "repair": {Type: "boolean", Default: false}}) {
		t.Errorf("dispatch inputs %+v, want verify and the dispatcher's repair", inputs)
	}
	reconcile := caller.Jobs["reconcile"]
	if !reflect.DeepEqual(reconcile.With, map[string]string{"verify": "${{ inputs.verify || false }}"}) || !slices.Contains(topLevel(t, reconcile.If, "&&"), "github.event_name != 'push'") {
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

func TestVerificationRunsOnlyWhenRequestedOnProtectedMain(t *testing.T) {
	t.Parallel()
	verify := readWorkflow(t, "reconcile-job.yml").Jobs["verify"]
	requirements := []string{"!cancelled()", "github.repository_id == '1328085692'", "github.repository_owner_id == '114402558'", "github.ref == 'refs/heads/main'", "github.ref_protected", "github.event_name == 'workflow_dispatch'", "inputs.verify"}
	facts := map[string]bool{}
	for _, requirement := range requirements {
		facts[requirement] = true
	}
	if !conjunction(t, verify.If, facts) {
		t.Fatalf("verification never runs: %q", verify.If)
	}
	for _, requirement := range requirements {
		facts[requirement] = false
		if conjunction(t, verify.If, facts) {
			t.Errorf("verification runs without %s", requirement)
		}
		facts[requirement] = true
	}
	if verify.Environment != "infrastructure-apply" {
		t.Errorf("verification environment %q", verify.Environment)
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
	for name, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Uses, "dopplerhq/") && name != "verify" {
				t.Errorf("job %s fetches Doppler", name)
			}
		}
	}
}

func TestVerificationRunsWithReadOnlyCredentials(t *testing.T) {
	t.Parallel()
	verify := readWorkflow(t, "reconcile-job.yml").Jobs["verify"]
	checkout := verify.step(t, func(step workflowStep) bool { return strings.HasPrefix(step.Uses, "actions/checkout@") })
	if persisted := checkout.With["persist-credentials"]; persisted != "false" {
		t.Errorf("checkout persists credentials with %q", persisted)
	}
	setup := verify.step(t, func(step workflowStep) bool { return step.ID == "setup" })
	if setup.With["full"] != "true" || setup.With["identity"] != "apply" {
		t.Errorf("verification prepares tooling with %v", setup.With)
	}
	if check := readWorkflow(t, "reconcile.yml").Jobs["check"].If; !slices.Contains(topLevel(t, check, "&&"), "!inputs.verify") {
		t.Errorf("verification runs repository checks with condition %q", check)
	}
	tokens := slices.DeleteFunc(slices.Clone(verify.Steps), func(step workflowStep) bool { return !strings.HasPrefix(step.Uses, "actions/create-github-app-token@") })
	if len(tokens) != 1 || tokens[0].ID != "observer-token" || tokens[0].If != "" || tokens[0].With["app-id"] != "${{ steps.doppler.outputs.OBSERVER_APP_ID }}" || tokens[0].With["private-key"] != "${{ steps.doppler.outputs.OBSERVER_APP_PRIVATE_KEY }}" || tokens[0].With["permission-administration"] != "read" {
		t.Fatalf("verification mints %+v", tokens)
	}
	for _, step := range verify.Steps {
		token, ok := step.Env["GH_TOKEN"]
		if reads := strings.Contains(token, "steps.observer-token"); reads != (step.Name == "Verify the published revision") || (reads && token != "${{ steps.observer-token.outputs.token }}") || (ok && !reads && token != "${{ github.token }}") {
			t.Errorf("step %q receives GH_TOKEN %q", cmp.Or(step.Name, step.Uses), token)
		}
	}
}

func TestProvenanceCredentialsReachOnlyTheVerifier(t *testing.T) {
	t.Parallel()
	workflow := readWorkflow(t, "reconcile-job.yml")
	if _, ok := workflow.Env["PROVENANCE_TOKEN"]; ok {
		t.Error("every job receives the provenance token")
	}
	for name, job := range workflow.Jobs {
		if packages := job.Permissions["packages"]; packages != map[bool]string{true: "read"}[name == "verify"] {
			t.Errorf("job %s has packages permission %q", name, packages)
		}
		if _, ok := job.Env["PROVENANCE_TOKEN"]; ok {
			t.Errorf("job %s passes the provenance token to every step", name)
		}
		for _, step := range job.Steps {
			token, ok := step.Env["PROVENANCE_TOKEN"]
			if verifier := name == "verify" && (step.Name == "Verify the published revision" || step.Name == provenanceGateStep); ok != verifier || (verifier && token != "${{ github.token }}") {
				t.Errorf("job %s step %q receives provenance token %q", name, cmp.Or(step.Name, step.Uses, step.Run), token)
			}
		}
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

const (
	verificationBot      = "github.triggering_actor != 'fredrir-infra-verification[bot]'"
	verificationBotCheck = "(inputs.verify || " + verificationBot + ")"
)

func topLevel(t *testing.T, condition, operator string) []string {
	t.Helper()
	expression := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(condition), "${{"), "}}"))
	var parts []string
	depth, quoted, start := 0, false, 0
	for index := 0; index < len(expression); index++ {
		switch character := expression[index]; {
		case character == '\'':
			quoted = !quoted
		case quoted:
		case character == '(':
			depth++
		case character == ')':
			depth--
		case depth == 0 && strings.HasPrefix(expression[index:], operator):
			parts = append(parts, strings.TrimSpace(expression[start:index]))
			start = index + len(operator)
		}
	}
	if depth != 0 || quoted {
		t.Fatalf("unbalanced condition %q", condition)
	}
	return append(parts, strings.TrimSpace(expression[start:]))
}

func TestVerificationAppStartsOnlyVerification(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("..", "..", ".github/workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var dispatchable []string
	for _, path := range paths {
		name := filepath.Base(path)
		workflow := readWorkflow(t, name)
		if _, ok := workflow.On["workflow_dispatch"]; !ok {
			continue
		}
		dispatchable = append(dispatchable, name)
		for job, definition := range workflow.Jobs {
			if len(topLevel(t, definition.If, "||")) != 1 {
				t.Errorf("%s job %s condition %q has a top-level alternative", name, job, definition.If)
			}
			conjuncts := topLevel(t, definition.If, "&&")
			if !slices.Contains(conjuncts, verificationBot) && (name != "reconcile.yml" || !slices.Contains(conjuncts, verificationBotCheck)) {
				t.Errorf("%s job %s runs for the verification App: %q", name, job, definition.If)
			}
		}
	}
	for _, name := range []string{"check.yml", "cosign-release.yml", "deploy-runner-image.yml", "deploy.yml", "images.yml", "reconcile.yml", "source-watcher-image.yml", "tag-image.yml"} {
		if !slices.Contains(dispatchable, name) {
			t.Errorf("%s is not found as a dispatchable workflow", name)
		}
	}
	reconcile := readWorkflow(t, "reconcile.yml")
	for _, job := range []string{"cli", "reconcile"} {
		if !slices.Contains(topLevel(t, reconcile.Jobs[job].If, "&&"), verificationBotCheck) {
			t.Errorf("reconcile.yml job %s refuses requested verification: %q", job, reconcile.Jobs[job].If)
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

const provenanceGateStep = "Verify commit provenance"

func TestProvenanceGateRunsBeforeAnyCheckoutCode(t *testing.T) {
	t.Parallel()
	apply := readWorkflow(t, "reconcile-job.yml").Jobs["verify"]
	gate := slices.IndexFunc(apply.Steps, func(step workflowStep) bool { return step.Name == provenanceGateStep })
	if gate < 0 {
		t.Fatal("verification has no provenance gate")
	}
	pinnedAction := regexp.MustCompile(`@[a-f0-9]{40}$`)
	trusted := apply.Steps[:gate]
	if len(trusted) != 3 || !strings.HasPrefix(trusted[0].Uses, "actions/checkout@") || trusted[1].Name != "Install provenance gate" || !strings.HasPrefix(trusted[2].Uses, "dopplerhq/secrets-fetch-action@") || !pinnedAction.MatchString(trusted[0].Uses) || !pinnedAction.MatchString(trusted[2].Uses) {
		t.Fatalf("steps before the provenance gate: %+v", trusted)
	}
	install, verify := trusted[1], apply.Steps[gate]
	if !regexp.MustCompile(`^infra-v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(install.Env["GATE_RELEASE"]) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(install.Env["GATE_SHA256"]) {
		t.Fatalf("provenance gate pin %q %q", install.Env["GATE_RELEASE"], install.Env["GATE_SHA256"])
	}
	for _, fragment := range []string{`gate="$RUNNER_TEMP/provenance-gate/infra"`, `"https://github.com/fredrir/infra/releases/download/$GATE_RELEASE/infra-linux-amd64" --output "$gate"`, `printf '%s  %s\n' "$GATE_SHA256" "$gate" | sha256sum --check --strict`, `"$gate" ci install-tools gh cosign`} {
		if !strings.Contains(install.Run, fragment) {
			t.Errorf("gate installation lacks %s:\n%s", fragment, install.Run)
		}
	}
	if strings.Contains(install.Run, "build/") || strings.Contains(install.Run, "./") {
		t.Errorf("gate installation reads the checkout:\n%s", install.Run)
	}
	if verify.If != "" || strings.TrimSpace(verify.Run) != `"$RUNNER_TEMP/provenance-gate/infra" reconcile provenance --report "$RUNNER_TEMP/reconciliation/provenance.json"` {
		t.Errorf("provenance gate runs %q when %q", verify.Run, verify.If)
	}
	for _, credential := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if verify.Env[credential] != "${{ steps.doppler.outputs."+credential+" }}" {
			t.Errorf("provenance gate reads state with %s=%q", credential, verify.Env[credential])
		}
	}
	for index, step := range apply.Steps {
		if index <= gate && (strings.HasPrefix(step.Uses, "./") || regexp.MustCompile(`(^|\s)infra `).MatchString(step.Run)) {
			t.Errorf("step %d runs checkout code before the provenance gate: %+v", index, step)
		}
	}
	if summary := apply.step(t, func(step workflowStep) bool { return step.Name == "Summarize reconciliation" }); !strings.Contains(summary.Run, "for report in provenance ") {
		t.Errorf("provenance report is not summarized:\n%s", summary.Run)
	}
}
