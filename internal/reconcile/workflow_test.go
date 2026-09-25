package reconcile

import (
	"cmp"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/itchyny/gojq"
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

func TestDriftRepairDispatchRequiresRequestedRepairOfVerification(t *testing.T) {
	workflow := readWorkflow(t, "reconcile-job.yml")
	repair, ok := workflow.Jobs["repair"]
	if !ok || !reflect.DeepEqual(repair.Permissions, map[string]string{"actions": "write"}) {
		t.Fatalf("repair job permissions are not exactly actions: write: %+v", repair.Permissions)
	}
	facts := func(verify, repair bool) map[string]bool {
		return map[string]bool{"inputs.verify": verify, "inputs.repair": repair, "!cancelled()": true, "needs.apply.result == 'failure'": true, "needs.apply.outputs.reconciliation == 'failure'": true, "needs.apply.outputs.differences == 'true'": true}
	}
	for _, test := range []struct {
		name           string
		verify, repair bool
		dispatch       bool
	}{
		{name: "push"},
		{name: "on-demand verification", verify: true},
		{name: "repair without verification", repair: true},
		{name: "verification with repair", verify: true, repair: true, dispatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if dispatch := conjunction(t, repair.If, facts(test.verify, test.repair)); dispatch != test.dispatch {
				t.Fatalf("repair dispatched=%t for condition %q", dispatch, repair.If)
			}
		})
	}
	for _, requirement := range []string{"!cancelled()", "needs.apply.result == 'failure'", "needs.apply.outputs.reconciliation == 'failure'", "needs.apply.outputs.differences == 'true'"} {
		scenario := facts(true, true)
		scenario[requirement] = false
		if conjunction(t, repair.If, scenario) {
			t.Errorf("repair job condition %q does not require %s", repair.If, requirement)
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
	if verification := workflow.Jobs["apply"].Env["VERIFICATION"]; verification != "${{ inputs.verify }}" {
		t.Errorf("dispatched verification selects %q", verification)
	}
	caller := readWorkflow(t, "reconcile.yml")
	if forwarded := caller.Jobs["reconcile"].With; forwarded["verify"] != "${{ inputs.verify || false }}" || forwarded["repair"] != "${{ inputs.repair || false }}" {
		t.Errorf("reconciliation forwards %v", forwarded)
	}
	triggers := slices.Sorted(maps.Keys(caller.On))
	if !slices.Equal(triggers, []string{"pull_request", "push", "workflow_dispatch"}) {
		t.Errorf("reconciliation triggers %v, want push, pull_request and workflow_dispatch only", triggers)
	}
	inputs := caller.inputs(t, "workflow_dispatch")
	for name, want := range map[string]workflowInput{"full": {Type: "boolean", Default: true}, "verify": {Type: "boolean", Default: false}, "repair": {Type: "boolean", Default: false}} {
		if inputs[name] != want {
			t.Errorf("dispatch input %s is %+v, want %+v", name, inputs[name], want)
		}
	}
	for name, input := range readWorkflow(t, "reconcile-job.yml").inputs(t, "workflow_call") {
		if input != (workflowInput{Type: "boolean", Default: false}) {
			t.Errorf("called input %s is %+v", name, input)
		}
	}
	for _, name := range []string{"reconcile.yml", "reconcile-job.yml"} {
		data, err := os.ReadFile(filepath.Join("..", "..", ".github/workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "schedule") {
			t.Errorf("%s still depends on a GitHub schedule", name)
		}
	}
}

func repairQuery(t *testing.T, listing string) *gojq.Code {
	t.Helper()
	return jobQuery(t, "repair", listing)
}

func jobQueryText(t *testing.T, job, listing string) string {
	t.Helper()
	script := readWorkflow(t, "reconcile-job.yml").Jobs[job].script()
	for _, line := range strings.Split(script, "\n") {
		if match := regexp.MustCompile(`--jq '([^']+)'`).FindStringSubmatch(line); match != nil && strings.Contains(line, listing) {
			return match[1]
		}
	}
	t.Fatalf("%s job has no query over %s runs:\n%s", job, listing, script)
	return ""
}

func jobQuery(t *testing.T, job, listing string, options ...gojq.CompilerOption) *gojq.Code {
	t.Helper()
	query, err := gojq.Parse(jobQueryText(t, job, listing))
	if err != nil {
		t.Fatal(err)
	}
	program, err := gojq.Compile(query, options...)
	if err != nil {
		t.Fatal(err)
	}
	return program
}

func queryResults(t *testing.T, program *gojq.Code, input any) []any {
	t.Helper()
	var results []any
	iterator := program.Run(input)
	for {
		result, ok := iterator.Next()
		if !ok {
			return results
		}
		if err, ok := result.(error); ok {
			t.Fatal(err)
		}
		results = append(results, result)
	}
}

func TestRepairWaitsForPendingPushApplies(t *testing.T) {
	script := readWorkflow(t, "reconcile-job.yml").Jobs["repair"].script()
	for _, guard := range []string{"--workflow reconcile.yml --event push --branch main", "--json databaseId,status", `gh api --paginate "repos/$GH_REPO/actions/runs/$run/jobs"`} {
		if !strings.Contains(script, guard) {
			t.Errorf("repair dispatch lacks push guard %s:\n%s", guard, script)
		}
	}
	if strings.Contains(script, "--event push --commit") {
		t.Errorf("repair dispatch only waits for push reconciliations of its own commit:\n%s", script)
	}
	if strings.Index(script, "/jobs") > strings.Index(script, "gh workflow run") {
		t.Errorf("repair dispatches before checking pending push applies:\n%s", script)
	}
	pending := repairQuery(t, "--event push")
	for _, test := range []struct {
		statuses []string
		want     []any
	}{
		{},
		{statuses: []string{"completed", "completed"}},
		{statuses: []string{"queued", "completed", "in_progress"}, want: []any{0, 2}},
		{statuses: []string{"pending", "waiting", "requested"}, want: []any{0, 1, 2}},
	} {
		runs := []any{}
		for id, status := range test.statuses {
			runs = append(runs, map[string]any{"databaseId": id, "status": status})
		}
		if got := queryResults(t, pending, runs); !reflect.DeepEqual(got, test.want) {
			t.Errorf("push runs %v awaited %v, want %v", test.statuses, got, test.want)
		}
	}
	caller := readWorkflow(t, "reconcile.yml").Jobs["reconcile"]
	apply := readWorkflow(t, "reconcile-job.yml").Jobs["apply"]
	if caller.Uses != "./.github/workflows/reconcile-job.yml" {
		t.Fatalf("reconcile job calls %q", caller.Uses)
	}
	if name := cmp.Or(caller.Name, "reconcile") + " / " + cmp.Or(apply.Name, "apply"); !strings.Contains(script, `.name == "`+name+`"`) {
		t.Errorf("repair dispatch does not wait for the %q job:\n%s", name, script)
	}
	applied := repairQuery(t, "/jobs")
	for _, test := range []struct {
		name     string
		jobs     map[string]string
		dispatch bool
	}{
		{name: "jobs not yet created"},
		{name: "plan running", jobs: map[string]string{"reconcile / plan": "in_progress"}},
		{name: "apply queued behind the production group", jobs: map[string]string{"reconcile / plan": "completed", "reconcile / apply": "pending"}},
		{name: "apply waiting", jobs: map[string]string{"reconcile / apply": "waiting"}},
		{name: "apply running", jobs: map[string]string{"reconcile / apply": "in_progress"}},
		{name: "apply completed while an image build waits for its runner", jobs: map[string]string{"reconcile / apply": "completed", "images / image (runner) / build": "queued", "check / validate": "queued"}, dispatch: true},
		{name: "apply superseded while images build", jobs: map[string]string{"reconcile / apply": "completed", "images / plan": "in_progress"}, dispatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			jobs := []any{}
			for name, status := range test.jobs {
				jobs = append(jobs, map[string]any{"name": name, "status": status})
			}
			if completed := queryResults(t, applied, map[string]any{"jobs": jobs}); (len(completed) > 0) != test.dispatch {
				t.Fatalf("jobs %v found completed applies %q, want dispatch %t", test.jobs, completed, test.dispatch)
			}
		})
	}
}

func TestCalledReconciliationJobsRequestOnlyGrantedPermissions(t *testing.T) {
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

func TestApplyJobOutlivesTheApplyDeadline(t *testing.T) {
	apply := readWorkflow(t, "reconcile-job.yml").Jobs["apply"]
	flag := regexp.MustCompile(`infra reconcile apply --wait=(\S+) `).FindStringSubmatch(apply.step(t, func(step workflowStep) bool { return step.ID == "reconcile" }).Run)
	if flag == nil {
		t.Fatal("apply does not wait for an abandoned lease")
	}
	wait, err := time.ParseDuration(flag[1])
	if err != nil || wait <= leaseTTL {
		t.Fatalf("apply waits %s for a lease that expires after %s", flag[1], leaseTTL)
	}
	action, err := os.ReadFile(filepath.Join("..", "..", ".github/actions/setup-reconciliation-cli/action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	build := regexp.MustCompile(`deadline = time\.monotonic\(\) \+ (\d+)\n`).FindSubmatch(action)
	if build == nil {
		t.Fatal("shared CLI build wait is unbounded")
	}
	seconds, err := strconv.Atoi(string(build[1]))
	if err != nil {
		t.Fatal(err)
	}
	minutes, ok := apply.TimeoutMinutes.(int)
	if margin := time.Duration(minutes)*time.Minute - time.Duration(seconds)*time.Second - wait - applyDeadline; !ok || margin < 3*time.Minute {
		t.Fatalf("apply job timeout of %v minutes leaves %s beyond the shared CLI build wait, lease wait and apply deadline for setup and reporting", apply.TimeoutMinutes, margin)
	}
}

func TestRepairCapFollowsLatestDispatchedReconciliation(t *testing.T) {
	program := repairQuery(t, "--event workflow_dispatch")
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
			suppressed := queryResults(t, program, runs)
			if (len(suppressed) == 0) != test.dispatch {
				t.Fatalf("previous reconciliation %+v suppressed the repair with %q", runs, suppressed)
			}
		})
	}
}

func TestVerificationRunsWithReadOnlyCredentials(t *testing.T) {
	apply := readWorkflow(t, "reconcile-job.yml").Jobs["apply"]
	checkout := apply.step(t, func(step workflowStep) bool { return strings.HasPrefix(step.Uses, "actions/checkout@") })
	if persisted := checkout.With["persist-credentials"]; persisted != "false" {
		t.Errorf("checkout persists credentials with %q", persisted)
	}
	token := apply.step(t, func(step workflowStep) bool { return strings.HasPrefix(step.Uses, "actions/create-github-app-token@") })
	if administration := token.With["permission-administration"]; administration != "${{ env.VERIFICATION == 'true' && 'read' || 'write' }}" {
		t.Errorf("runner registration token administration permission is %q", administration)
	}
	setup := apply.step(t, func(step workflowStep) bool { return step.ID == "setup" })
	if full := setup.With["full"]; full != "${{ env.VERIFICATION == 'true' || inputs.full }}" {
		t.Errorf("verification prepares tooling with full=%q", full)
	}
	if check := readWorkflow(t, "reconcile.yml").Jobs["check"].If; !slices.Contains(topLevel(t, check, "&&"), "!inputs.verify") {
		t.Errorf("verification runs repository checks with condition %q", check)
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
		if pushIgnored(path) != ignored {
			t.Errorf("%s ignored=%t", path, !ignored)
		}
		if selected := Affected([]string{path}); ignored && (selected.Tofu || selected.Kubernetes || selected.Ansible || selected.Tooling) {
			t.Errorf("push-ignored %s selects %+v", path, selected)
		}
	}
}

func TestRetriedApplyRequiresANewerPendingPushApply(t *testing.T) {
	apply := readWorkflow(t, "reconcile-job.yml").Jobs["apply"]
	reconcile := apply.step(t, func(step workflowStep) bool { return step.ID == "reconcile" })
	for _, mapping := range []string{`if [ "$code" -eq 75 ]; then`, `echo 'retry=true' >> "$GITHUB_OUTPUT"`, `exit "$code"`} {
		if !strings.Contains(reconcile.Run, mapping) {
			t.Errorf("apply step lacks %s:\n%s", mapping, reconcile.Run)
		}
	}
	successor := apply.step(t, func(step workflowStep) bool { return strings.Contains(step.Run, "gh run list") })
	for _, guard := range []string{"--workflow reconcile.yml --event push --branch main", "--json databaseId,status", `for run in $newer; do`, `gh api --paginate "repos/$GH_REPO/actions/runs/$run/jobs"`, `if [ -z "$applied" ]; then`} {
		if !strings.Contains(successor.Run, guard) {
			t.Errorf("successor check lacks %s:\n%s", guard, successor.Run)
		}
	}
	if successor.If != "steps.reconcile.outputs.retry == 'true'" || strings.Index(successor.Run, "exit 0") > strings.Index(successor.Run, "exit 1") {
		t.Errorf("successor check does not fail without a pending apply:\n%s", successor.Run)
	}
	if applied, repaired := jobQueryText(t, "apply", "/jobs"), jobQueryText(t, "repair", "/jobs"); applied != repaired {
		t.Errorf("successor check reads apply jobs with %q, repair with %q", applied, repaired)
	}
	newer := jobQuery(t, "apply", "gh run list", gojq.WithEnvironLoader(func() []string { return []string{"GITHUB_RUN_ID=100"} }))
	for _, test := range []struct {
		runs map[int]string
		want []any
	}{
		{},
		{runs: map[int]string{100: "in_progress", 99: "queued"}},
		{runs: map[int]string{101: "completed"}},
		{runs: map[int]string{101: "queued"}, want: []any{101}},
		{runs: map[int]string{101: "in_progress", 100: "in_progress"}, want: []any{101}},
	} {
		runs := []any{}
		for id, status := range test.runs {
			runs = append(runs, map[string]any{"databaseId": id, "status": status})
		}
		if got := queryResults(t, newer, runs); !reflect.DeepEqual(got, test.want) {
			t.Errorf("runs %v deferred to %v, want %v", test.runs, got, test.want)
		}
	}
}

func TestProvenanceCredentialsReachOnlyTheApplyEngine(t *testing.T) {
	workflow := readWorkflow(t, "reconcile-job.yml")
	if _, ok := workflow.Env["PROVENANCE_TOKEN"]; ok {
		t.Error("every job receives the provenance token")
	}
	for name, job := range workflow.Jobs {
		if packages := job.Permissions["packages"]; packages != map[bool]string{true: "read"}[name == "apply"] {
			t.Errorf("job %s has packages permission %q", name, packages)
		}
		if _, ok := job.Env["PROVENANCE_TOKEN"]; ok {
			t.Errorf("job %s passes the provenance token to every step", name)
		}
		for _, step := range job.Steps {
			token, ok := step.Env["PROVENANCE_TOKEN"]
			if verifier := name == "apply" && (step.ID == "reconcile" || step.Name == provenanceGateStep); ok != verifier || (verifier && token != "${{ github.token }}") {
				t.Errorf("job %s step %q receives provenance token %q", name, cmp.Or(step.ID, step.Uses, step.Run), token)
			}
		}
	}
	if setup := workflow.Jobs["apply"].step(t, func(step workflowStep) bool { return step.ID == "setup" }); setup.Uses != "./.github/actions/setup-reconciliation" || setup.With["identity"] != "apply" {
		t.Fatalf("apply prepares tooling with %+v", setup)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", ".github/actions/setup-reconciliation/action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Runs struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(action.Runs.Steps, func(step workflowStep) bool {
		return step.Env["IDENTITY"] == "${{ inputs.identity }}" && strings.Contains(step.Run, `if [ "$IDENTITY" = apply ]; then infra ci install-tools gh cosign; fi`)
	}) {
		t.Error("apply tooling lacks the attestation verifiers")
	}
}

const provenanceGateStep = "Verify commit provenance"

func TestProvenanceGateRunsBeforeAnyCheckoutCode(t *testing.T) {
	apply := readWorkflow(t, "reconcile-job.yml").Jobs["apply"]
	gate := slices.IndexFunc(apply.Steps, func(step workflowStep) bool { return step.Name == provenanceGateStep })
	if gate < 0 {
		t.Fatal("apply has no provenance gate")
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

const publisherKey = "PUBLISHER_APP_PRIVATE_KEY"

func TestPublisherKeyReachesOnlyThePublishingEngine(t *testing.T) {
	workflow := readWorkflow(t, "reconcile-job.yml")
	if _, ok := workflow.Env[publisherKey]; ok {
		t.Error("every job receives the publisher key")
	}
	for name, job := range workflow.Jobs {
		if _, ok := job.Env[publisherKey]; ok {
			t.Errorf("job %s passes the publisher key to every step", name)
		}
		if contents := job.Permissions["contents"]; contents == "write" {
			t.Errorf("job %s can push with its workflow token", name)
		}
		for _, step := range job.Steps {
			key, ok := step.Env[publisherKey]
			if publisher := name == "apply" && step.ID == "reconcile"; ok != publisher || (publisher && key != "${{ env.VERIFICATION != 'true' && steps.doppler.outputs.PUBLISHER_APP_PRIVATE_KEY || '' }}") {
				t.Errorf("job %s step %q receives publisher key %q", name, cmp.Or(step.ID, step.Name, step.Uses), key)
			}
			if strings.Contains(step.Run, publisherKey) || slices.ContainsFunc(slices.Collect(maps.Values(step.With)), func(value string) bool { return strings.Contains(value, publisherKey) }) {
				t.Errorf("job %s step %q reads the publisher key outside the engine", name, cmp.Or(step.ID, step.Name, step.Uses))
			}
		}
	}
	apply := workflow.Jobs["apply"]
	if !reflect.DeepEqual(apply.Permissions, map[string]string{"contents": "read", "id-token": "write", "actions": "read", "packages": "read"}) {
		t.Errorf("apply permissions %v", apply.Permissions)
	}
	gate := apply.step(t, func(step workflowStep) bool { return step.Name == provenanceGateStep })
	reconcile := apply.step(t, func(step workflowStep) bool { return step.ID == "reconcile" })
	if _, ok := gate.Env[publisherKey]; ok || !strings.Contains(reconcile.Run, `if [ "$VERIFICATION" = true ]; then`) || !strings.Contains(reconcile.Run, "infra reconcile verify") || !strings.Contains(reconcile.Run, "infra reconcile apply") {
		t.Errorf("publisher key reaches the gate or the reconcile step no longer separates verification:\n%s", reconcile.Run)
	}
	for _, name := range []string{"reconcile.yml", "reconcile-job.yml"} {
		for job, definition := range readWorkflow(t, name).Jobs {
			if definition.Permissions["contents"] == "write" {
				t.Errorf("%s job %s grants contents: write", name, job)
			}
		}
	}
}

func TestRulesetDifferencesDoNotDispatchRepair(t *testing.T) {
	step := readWorkflow(t, "reconcile-job.yml").Jobs["apply"].step(t, func(step workflowStep) bool { return step.ID == "verification" })
	match := regexp.MustCompile(`jq -e '([^']+)' "\$report"`).FindStringSubmatch(step.Run)
	if match == nil {
		t.Fatalf("difference selection not found:\n%s", step.Run)
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
		systems []string
		repair  bool
	}{
		{},
		{systems: []string{"rulesets"}},
		{systems: []string{"rulesets", "rulesets"}},
		{systems: []string{"revision"}, repair: true},
		{systems: []string{"rulesets", "hosts"}, repair: true},
	} {
		differences := []any{}
		for _, system := range test.systems {
			differences = append(differences, map[string]any{"system": system, "item": "x"})
		}
		results := queryResults(t, program, map[string]any{"differences": differences})
		if len(results) != 1 || results[0] != test.repair {
			t.Errorf("differences in %v select repair %v, want %t", test.systems, results, test.repair)
		}
	}
}
