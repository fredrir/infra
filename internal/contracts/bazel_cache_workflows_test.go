package contracts

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const (
	bazelCacheAction        = "./.github/actions/bazel-cache"
	bazelRepositoryCache    = "~/.cache/infra-bazel-repository"
	bazelRepositoryCacheKey = "infra-bazel-repository-v1-${{ hashFiles('build/toolchain.json', 'MODULE.bazel.lock', 'go.sum') }}"
)

type workflowStep struct {
	ID              string            `yaml:"id"`
	Name            string            `yaml:"name"`
	If              string            `yaml:"if"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	Env             map[string]string `yaml:"env"`
	With            map[string]string `yaml:"with"`
	ContinueOnError bool              `yaml:"continue-on-error"`
}

type workflowJob struct {
	Uses        string            `yaml:"uses"`
	Permissions map[string]string `yaml:"permissions"`
	Env         map[string]string `yaml:"env"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowFile struct {
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

func workflows(t *testing.T) map[string]workflowFile {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root(t), ".github/workflows/*.yml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("workflows unavailable: %v", err)
	}
	parsed := map[string]workflowFile{}
	for _, path := range paths {
		var workflow workflowFile
		if err := yaml.Unmarshal(read(t, path), &workflow); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		parsed[filepath.Base(path)] = workflow
	}
	return parsed
}

func compositeSteps(t *testing.T, path string) []workflowStep {
	t.Helper()
	var action struct {
		Runs struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), path)), &action); err != nil {
		t.Fatal(err)
	}
	return action.Runs.Steps
}

func stepByID(t *testing.T, steps []workflowStep, id string) (int, workflowStep) {
	t.Helper()
	for index, step := range steps {
		if step.ID == id {
			return index, step
		}
	}
	t.Fatalf("no step %s", id)
	return 0, workflowStep{}
}

func declaredIdentity(t *testing.T, name string) federatedIdentity {
	t.Helper()
	for _, identity := range federatedIdentities(t) {
		if identity.Name == name {
			return identity
		}
	}
	t.Fatalf("identity %s is not declared", name)
	return federatedIdentity{}
}

type bazelCacheEvent struct {
	name, repository, event, ref, head string
	protected                          bool
	role                               string
}

func (e bazelCacheEvent) github() map[string]any {
	return map[string]any{
		"repository":    e.repository,
		"event_name":    e.event,
		"ref":           e.ref,
		"ref_protected": e.protected,
		"event":         map[string]any{"pull_request": map[string]any{"head": map[string]any{"repo": map[string]any{"full_name": e.head}}}},
	}
}

var bazelCacheEvents = []bazelCacheEvent{
	{"main push", "fredrir/infra", "push", "refs/heads/main", "", true, "writer"},
	{"main dispatch", "fredrir/infra", "workflow_dispatch", "refs/heads/main", "", true, "writer"},
	{"main schedule", "fredrir/infra", "schedule", "refs/heads/main", "", true, "writer"},
	{"unprotected main push", "fredrir/infra", "push", "refs/heads/main", "", false, ""},
	{"same-repository pull request", "fredrir/infra", "pull_request", "refs/pull/7/merge", "fredrir/infra", false, "reader"},
	{"fork pull request", "fredrir/infra", "pull_request", "refs/pull/7/merge", "attacker/infra", false, ""},
	{"pull request target on main", "fredrir/infra", "pull_request_target", "refs/heads/main", "attacker/infra", true, ""},
	{"workflow run on main", "fredrir/infra", "workflow_run", "refs/heads/main", "", true, ""},
	{"merge queue", "fredrir/infra", "merge_group", "refs/heads/gh-readonly-queue/main/pr-7", "", true, ""},
	{"branch push", "fredrir/infra", "push", "refs/heads/feature", "", false, ""},
	{"protected branch named after main", "fredrir/infra", "push", "refs/heads/main-copy", "", true, ""},
	{"release tag", "fredrir/infra", "push", "refs/tags/infra-v1.2.3", "", true, ""},
	{"consumer main push", "fredrir/nsql", "push", "refs/heads/main", "", true, ""},
	{"consumer pull request", "fredrir/nsql", "pull_request", "refs/pull/7/merge", "fredrir/nsql", false, ""},
}

var bazelCacheJobs = []struct{ workflow, job, writer string }{
	{"check.yml", "check", "bazel-cache-check-writer"},
	{"infra-cli.yml", "build", "bazel-cache-cli-writer"},
}

func bazelCacheWriter(workflow string) string {
	for _, cache := range bazelCacheJobs {
		if cache.workflow == workflow {
			return cache.writer
		}
	}
	return ""
}

func TestBazelCacheIdentityFollowsTheTriggeringEvent(t *testing.T) {
	parsed := workflows(t)
	for _, cache := range bazelCacheJobs {
		identity := workflowValue(t, parsed[cache.workflow].Jobs[cache.job].Env["BAZEL_CACHE_IDENTITY"])
		for _, event := range bazelCacheEvents {
			want := map[string]string{"writer": cache.writer, "reader": "bazel-cache-reader", "": ""}[event.role]
			got, err := identity.value(map[string]any{"github": event.github()})
			if err != nil || got != want {
				t.Errorf("%s %s: identity %#v (%v), want %q", cache.workflow, event.name, got, err, want)
			}
		}
	}
}

func TestBazelCacheJoinsWithTheIdentitysOwnClient(t *testing.T) {
	variables := map[string]any{}
	for _, identity := range federatedIdentities(t) {
		variables[identity.ClientIDVariable] = identity.ClientIDVariable
	}
	joins := 0
	for name, workflow := range workflows(t) {
		for jobName, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if step.Uses != bazelCacheAction {
					continue
				}
				joins++
				permissions := job.Permissions
				if permissions == nil {
					permissions = workflow.Permissions
				}
				if permissions["id-token"] != "write" {
					t.Errorf("%s %s joins the Bazel cache without an OIDC token", name, jobName)
				}
				identities := []string{step.With["identity"]}
				if strings.Contains(step.With["identity"], "${{") {
					identities = []string{bazelCacheWriter(name), "bazel-cache-reader", ""}
				}
				for _, identity := range identities {
					context := map[string]any{
						"env":    map[string]any{"BAZEL_CACHE_IDENTITY": identity},
						"vars":   variables,
						"matrix": map[string]any{"platform": "linux_amd64"},
						"steps":  map[string]any{"affected": map[string]any{"outputs": map[string]any{"expression": "//..."}}, "cached": map[string]any{"outputs": map[string]any{"hit": "false"}}},
					}
					joined, err := workflowCondition(t, step.If).allows(context)
					if err != nil || joined != (identity != "") {
						t.Errorf("%s %s joins as %q: %v (%v)", name, jobName, identity, joined, err)
					}
					if identity == "" {
						continue
					}
					if expression := step.With["identity"]; strings.Contains(expression, "${{") {
						if selected, err := workflowValue(t, expression).value(context); err != nil || selected != identity {
							t.Errorf("%s %s selects identity %#v for %q (%v)", name, jobName, selected, identity, err)
						}
					}
					client, err := workflowValue(t, step.With["client-id"]).value(context)
					if want := declaredIdentity(t, identity).ClientIDVariable; err != nil || client != want {
						t.Errorf("%s %s joins as %s with client %#v, want %s (%v)", name, jobName, identity, client, want, err)
					}
				}
			}
		}
	}
	if joins != 3 {
		t.Errorf("Bazel cache joined by %d jobs, want the check, CLI build and release check", joins)
	}
}

func TestBazelCacheActionMapsEachIdentityToItsTailnetRole(t *testing.T) {
	policy := parseTailnetPolicy(t, read(t, filepath.Join(root(t), "tailscale/policy.hujson")))
	steps := compositeSteps(t, ".github/actions/bazel-cache/action.yml")
	_, role := stepByID(t, steps, "role")
	assign := func(identity string) (map[string]string, error) {
		output := filepath.Join(t.TempDir(), "output")
		command := exec.Command("bash", "-eo", "pipefail", "-c", role.Run)
		command.Env = append(os.Environ(), "IDENTITY="+identity, "GITHUB_OUTPUT="+output)
		if err := command.Run(); err != nil {
			return nil, err
		}
		assigned := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(read(t, output))), "\n") {
			key, value, _ := strings.Cut(line, "=")
			assigned[key] = value
		}
		return assigned, nil
	}
	cacheIdentities := 0
	for _, identity := range federatedIdentities(t) {
		assigned, err := assign(identity.Name)
		if !slices.ContainsFunc(identity.Tags, func(tag string) bool { return strings.HasPrefix(tag, "tag:ci-bazel-") }) {
			if err == nil {
				t.Errorf("%s joins the Bazel cache as %v", identity.Name, assigned)
			}
			continue
		}
		cacheIdentities++
		if err != nil || !slices.Equal(identity.Tags, []string{assigned["tag"]}) {
			t.Errorf("%s joins as %v (%v), want its tags %v", identity.Name, assigned, err, identity.Tags)
			continue
		}
		writes, err := policy.allows(assigned["tag"], "tcp", "bazel-cache:9093")
		if err != nil {
			t.Fatal(err)
		}
		reaches, err := policy.allows(assigned["tag"], "tcp", "bazel-cache:"+assigned["port"])
		if want := map[bool]string{true: "9093", false: "9092"}[writes]; err != nil || !reaches || assigned["port"] != want {
			t.Errorf("%s dials port %s, want %s the tailnet grants", identity.Name, assigned["port"], want)
		}
	}
	if cacheIdentities != 4 {
		t.Errorf("%d Bazel cache identities, want two readers and two writers", cacheIdentities)
	}
	if _, err := assign("bazel-cache-writer"); err == nil {
		t.Error("an undeclared identity joins the Bazel cache")
	}

	_, probe := stepByID(t, steps, "probe")
	host, port, err := net.SplitHostPort(probe.Env["ADDRESS"])
	if ip := net.ParseIP(host); err != nil || ip == nil || ip.To4() == nil || host != policy.Hosts["bazel-cache"] || port != "${{ steps.role.outputs.port }}" {
		t.Errorf("probe dials %q, want the IPv4 literal of the bazel-cache host %s", probe.Env["ADDRESS"], policy.Hosts["bazel-cache"])
	}
	if !strings.Contains(probe.Run, `endpoint=grpc://%s`) || !strings.Contains(probe.Run, `"$ADDRESS"`) {
		t.Error("the probed address is not the published endpoint")
	}
	_, join := stepByID(t, steps, "join")
	var pinned workflowStep
	for _, step := range compositeSteps(t, ".github/actions/setup-reconciliation/action.yml") {
		if strings.HasPrefix(step.Uses, "tailscale/github-action@") {
			pinned = step
		}
	}
	if join.Uses != pinned.Uses || join.With["version"] != pinned.With["version"] || join.With["sha256sum"] != pinned.With["sha256sum"] {
		t.Errorf("cache join runs %s %s, reconciliation %s %s", join.Uses, join.With["version"], pinned.Uses, pinned.With["version"])
	}
	if !join.ContinueOnError || join.With["audience"] != "${{ inputs.identity }}" || join.With["tags"] != "${{ steps.role.outputs.tag }}" || !strings.Contains(join.With["args"], "--accept-dns=false") {
		t.Errorf("cache join %+v may fail the build, resolve names or pick its own role", join.With)
	}
}

func TestPullRequestsNeverUploadToTheBazelCache(t *testing.T) {
	parsed := workflows(t)
	readOnly := regexp.MustCompile(`--read-only-cache(=\$\{\{(.+?)\}\})?(\s|$)`)
	for name, workflow := range parsed {
		for jobName, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if _, remote := step.Env["BAZEL_REMOTE_CACHE"]; !remote || !strings.Contains(step.Run, "infra") || strings.Contains(step.Run, `"$RUNNER_TEMP/bazel" --batch`) {
					continue
				}
				match := readOnly.FindStringSubmatch(step.Run)
				if match == nil {
					t.Errorf("%s %s %q may upload to the Bazel cache", name, jobName, step.Name)
					continue
				}
				if match[2] == "" {
					continue
				}
				for _, identity := range []string{bazelCacheWriter(name), "bazel-cache-reader", ""} {
					enabled, err := workflowValue(t, match[2]).allows(map[string]any{"env": map[string]any{"BAZEL_CACHE_IDENTITY": identity}})
					if want := identity != bazelCacheWriter(name); err != nil || enabled != want {
						t.Errorf("%s %s %q as %q: read-only %v (%v), want %v", name, jobName, step.Name, identity, enabled, err, want)
					}
				}
			}
		}
	}

	var build workflowStep
	for _, step := range parsed["infra-cli.yml"].Jobs["build"].Steps {
		if step.Name == "Build CLI" {
			build = step
		}
	}
	bazel := filepath.Join(t.TempDir(), "bazel")
	if err := os.WriteFile(bazel, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		identity, endpoint string
		uploads, remote    bool
	}{
		{"bazel-cache-cli-writer", "grpc://100.87.168.66:9093", true, true},
		{"bazel-cache-reader", "grpc://100.87.168.66:9092", false, true},
		{"bazel-cache-reader", "", false, false},
		{"", "", false, false},
	} {
		command := exec.Command("bash", "-eo", "pipefail", "-c", build.Run)
		command.Env = append(os.Environ(), "RUNNER_TEMP="+filepath.Dir(bazel), "BAZEL_CACHE_IDENTITY="+test.identity, "BAZEL_REMOTE_CACHE="+test.endpoint)
		output, err := command.Output()
		arguments := strings.Split(strings.TrimSpace(string(output)), "\n")
		if err != nil || slices.Contains(arguments, "--remote_upload_local_results=false") == test.uploads || slices.Contains(arguments, "--remote_cache="+test.endpoint) != test.remote {
			t.Errorf("CLI build as %q with %q runs %v (%v)", test.identity, test.endpoint, arguments, err)
		}
	}
}

func TestReleaseBinariesNeverTakeResultsFromTheBazelCache(t *testing.T) {
	steps := workflows(t)["cli-release.yml"].Jobs["build"].Steps
	join, cache := stepByID(t, steps, "bazel-cache")
	if cache.With["identity"] != "bazel-cache-release-reader" {
		t.Errorf("release checks join the Bazel cache as %s", cache.With["identity"])
	}
	built := -1
	for index, step := range steps {
		if strings.Contains(step.Run, "--batch build") {
			built = index
			if strings.Contains(strings.ToLower(step.Run+fmt.Sprint(step.Env)), "remote") {
				t.Errorf("release binaries build against a remote cache: %s", step.Run)
			}
		}
		if index > join && regexp.MustCompile(`(cp|mv|install) [^\n]*release`).MatchString(step.Run) {
			t.Errorf("%q writes release files after joining the Bazel cache", step.Name)
		}
	}
	if built < 0 || built > join {
		t.Error("release binaries are built after joining the Bazel cache")
	}
	for _, line := range strings.Split(string(read(t, filepath.Join(root(t), ".bazelrc"))), "\n") {
		if strings.Contains(line, "remote") || strings.Contains(line, "disk_cache") {
			t.Errorf(".bazelrc configures a shared cache for every build: %s", line)
		}
	}
}

func TestBazelActionResultsLiveOnlyInTheRemoteCache(t *testing.T) {
	saves := 0
	for name, workflow := range workflows(t) {
		data := string(read(t, filepath.Join(root(t), ".github/workflows", name)))
		if strings.Contains(data, "infra-bazel-actions") || strings.Contains(data, "disk_cache") {
			t.Errorf("%s keeps a Bazel action cache on GitHub", name)
		}
		for jobName, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if !strings.HasPrefix(step.Uses, "actions/cache") || !strings.Contains(step.With["path"], "infra-bazel") {
					continue
				}
				key := step.With["key"]
				if strings.HasPrefix(step.Uses, "actions/cache/save@") {
					restore, _ := strings.CutSuffix(strings.TrimPrefix(key, "${{ steps."), ".outputs.cache-primary-key }}")
					_, restored := stepByID(t, job.Steps, restore)
					key = restored.With["key"]
				}
				if step.With["path"] != bazelRepositoryCache || key != bazelRepositoryCacheKey || step.With["restore-keys"] != "" || strings.HasPrefix(step.Uses, "actions/cache@") {
					t.Errorf("%s %s caches %q under %q", name, jobName, step.With["path"], key)
				}
				if !strings.HasPrefix(step.Uses, "actions/cache/save@") {
					continue
				}
				saves++
				if name != "check.yml" || jobName != "check" {
					t.Errorf("%s %s saves the repository cache", name, jobName)
					continue
				}
				save := workflowCondition(t, step.If)
				for _, test := range []struct {
					name, identity, key, hit string
					failed, cancelled, saved bool
				}{
					{"passing main check", "bazel-cache-check-writer", "key", "false", false, false, true},
					{"failed main check", "bazel-cache-check-writer", "key", "false", true, false, true},
					{"cancelled main check", "bazel-cache-check-writer", "key", "false", false, true, false},
					{"existing entry", "bazel-cache-check-writer", "key", "true", false, false, false},
					{"no restore", "bazel-cache-check-writer", "", "", false, false, false},
					{"pull request", "bazel-cache-reader", "key", "false", false, false, false},
					{"no cache identity", "", "key", "false", false, false, false},
				} {
					saved, err := save.allows(map[string]any{
						"env":             map[string]any{"BAZEL_CACHE_IDENTITY": test.identity},
						"steps":           map[string]any{"repository-cache": map[string]any{"outputs": map[string]any{"cache-primary-key": test.key, "cache-hit": test.hit}}},
						"failedStatus":    test.failed,
						"cancelledStatus": test.cancelled,
					})
					if err != nil || saved != test.saved {
						t.Errorf("%s: saved = %v (%v), want %v", test.name, saved, err, test.saved)
					}
				}
			}
		}
	}
	if saves != 1 {
		t.Errorf("repository cache saved by %d steps", saves)
	}
}

func TestReusableWorkflowCallersGrantTheirCalleesOIDCTokens(t *testing.T) {
	parsed := workflows(t)
	var mints func(name string, seen []string) bool
	mints = func(name string, seen []string) bool {
		if slices.Contains(seen, name) {
			return false
		}
		workflow := parsed[name]
		for _, job := range workflow.Jobs {
			permissions := job.Permissions
			if permissions == nil {
				permissions = workflow.Permissions
			}
			callee, local := strings.CutPrefix(job.Uses, "./.github/workflows/")
			if permissions["id-token"] == "write" || local && mints(callee, append(seen, name)) {
				return true
			}
		}
		return false
	}
	for name, workflow := range parsed {
		for jobName, job := range workflow.Jobs {
			callee, local := strings.CutPrefix(job.Uses, "./.github/workflows/")
			if !local || !mints(callee, nil) {
				continue
			}
			permissions := job.Permissions
			if permissions == nil {
				permissions = workflow.Permissions
			}
			if permissions["id-token"] != "write" {
				t.Errorf("%s %s calls %s without granting id-token: write", name, jobName, callee)
			}
		}
	}
}
