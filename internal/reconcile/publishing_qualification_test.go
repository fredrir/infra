package reconcile

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/google/go-github/v88/github"
)

const (
	canaryBranch  = publishedBranch + "-canary"
	probeBranch   = canaryBranch + "-probe"
	probeWorkflow = ".github/workflows/" + probeBranch + ".yml"
	probeDeadline = 10 * time.Minute
)

type publishingQualification struct {
	t                 *testing.T
	ctx               context.Context
	root, repository  string
	owner, name       string
	admin             *github.Client
	adminToken        string
	publisherToken    string
	publisher         Publisher
	previous, current string
	earlier           string
}

func TestPublishingQualification(t *testing.T) {
	if os.Getenv("INFRA_PUBLISHING_QUALIFY") != "1" {
		t.Skip("INFRA_PUBLISHING_QUALIFY=1, GH_TOKEN of a repository administrator with the workflow scope and PUBLISHER_APP_PRIVATE_KEY_FILE apply and qualify the production rulesets")
	}
	q := newPublishingQualification(t)
	q.restore()
	t.Cleanup(q.restore)
	q.createRef(canaryBranch, q.previous)
	q.applyRulesets(canaryBranch)
	q.rejected("administrator", q.adminToken, q.current+":refs/heads/"+canaryBranch)
	q.rejectedWorkflowToken()
	q.rejected("publisher force-push", q.publisherToken, "--force", q.earlier+":refs/heads/"+canaryBranch)
	q.rejected("publisher deletion", q.publisherToken, ":refs/heads/"+canaryBranch)
	if output, err := q.push(q.publisherToken, q.current+":refs/heads/"+canaryBranch); err != nil {
		t.Fatalf("publisher fast-forward rejected: %v\n%s", err, output)
	}
	q.canaryAt(q.current)
	q.restore()
	if err := (&Commands{Runner: ci.Runner{Dir: q.root}, GitHub: q.admin}).VerifyRulesets(q.ctx); err != nil {
		t.Fatalf("live rulesets after qualification: %v", err)
	}
}

func newPublishingQualification(t *testing.T) *publishingQualification {
	t.Helper()
	q := &publishingQualification{t: t, ctx: context.Background(), root: cmp.Or(os.Getenv("INFRA_TEST_SOURCE_ROOT"), repositoryRoot), adminToken: os.Getenv("GH_TOKEN"), repository: t.TempDir()}
	if q.adminToken == "" || os.Getenv("PUBLISHER_APP_PRIVATE_KEY_FILE") == "" {
		t.Fatal("GH_TOKEN and PUBLISHER_APP_PRIVATE_KEY_FILE required")
	}
	key, err := ConsumePrivateKey(os.Getenv("PUBLISHER_APP_PRIVATE_KEY_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	if q.publisher, err = ReadPublisher(q.root); err != nil {
		t.Fatal(err)
	}
	q.publisher.PrivateKey = key
	if q.owner, q.name, err = q.publisher.repository(); err != nil {
		t.Fatal(err)
	}
	if q.admin, err = GitHubClient(GitHubAPI, q.adminToken); err != nil {
		t.Fatal(err)
	}
	repository, response, err := q.admin.Repositories.Get(q.ctx, q.owner, q.name)
	if err != nil {
		t.Fatal(err)
	}
	if !repository.GetPermissions().GetAdmin() {
		t.Fatalf("GH_TOKEN is not an administrator of %s", q.publisher.Repository)
	}
	if scopes := response.Header.Get("X-OAuth-Scopes"); scopes != "" && !slices.Contains(strings.Split(strings.ReplaceAll(scopes, " ", ""), ","), "workflow") {
		t.Fatalf("GH_TOKEN lacks the workflow scope (has %s); run gh auth refresh --scopes workflow", scopes)
	}
	declared, err := declaredRulesets(q.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"production", "production-history"} {
		if declared[name] == nil {
			t.Fatalf("ruleset %s is not declared", name)
		}
	}
	token, revoke, err := q.publisher.Token(q.ctx)
	if err != nil {
		t.Fatal(err)
	}
	q.publisherToken = token
	t.Cleanup(func() {
		if err := revoke(q.ctx); err != nil {
			t.Errorf("revoke publisher token: %v", err)
		}
	})
	q.git(nil, "init", "--quiet", "--bare")
	var output bytes.Buffer
	fetch := tokenGit(ci.Runner{Dir: q.repository, Stderr: &output}, q.adminToken)
	if err := fetch.Run(q.ctx, "git", "fetch", "--quiet", "--no-tags", "--depth=3", q.publisher.Remote, "refs/heads/main"); err != nil {
		t.Fatalf("fetch main: %v\n%s", err, output.String())
	}
	q.current, q.previous, q.earlier = q.git(nil, "rev-parse", "FETCH_HEAD"), q.git(nil, "rev-parse", "FETCH_HEAD~1"), q.git(nil, "rev-parse", "FETCH_HEAD~2")
	return q
}

func (q *publishingQualification) document(name string) map[string]any {
	q.t.Helper()
	paths, err := filepath.Glob(filepath.Join(q.root, ".github", "*-ruleset.json"))
	if err != nil {
		q.t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			q.t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			q.t.Fatal(err)
		}
		if document["name"] == name {
			return document
		}
	}
	q.t.Fatalf("ruleset %s is not declared", name)
	return nil
}

func (q *publishingQualification) git(stdin []byte, args ...string) string {
	q.t.Helper()
	command := exec.CommandContext(q.ctx, "git", args...)
	command.Dir = q.repository
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_INDEX_FILE="+filepath.Join(q.repository, "probe.index"), "GIT_AUTHOR_NAME=infra", "GIT_AUTHOR_EMAIL=infra@fredrir.invalid", "GIT_COMMITTER_NAME=infra", "GIT_COMMITTER_EMAIL=infra@fredrir.invalid")
	command.Stdin = bytes.NewReader(stdin)
	output, err := command.Output()
	if err != nil {
		q.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}

func (q *publishingQualification) push(token string, args ...string) (string, error) {
	var output bytes.Buffer
	runner := tokenGit(ci.Runner{Dir: q.repository, Stdout: &output, Stderr: &output}, token)
	err := runner.Run(q.ctx, "git", append([]string{"push", "--no-verify", q.publisher.Remote}, args...)...)
	if strings.Contains(output.String(), token) {
		q.t.Fatal("git printed a push token")
	}
	return output.String(), err
}

func (q *publishingQualification) rejected(actor, token string, args ...string) {
	q.t.Helper()
	output, err := q.push(token, args...)
	if err == nil || !strings.Contains(output, "GH013") {
		q.t.Fatalf("%s push %q was not rejected by a ruleset: %v\n%s", actor, args, err, output)
	}
	q.t.Logf("%s push rejected", actor)
	q.canaryAt(q.previous)
}

func (q *publishingQualification) canaryAt(revision string) {
	q.t.Helper()
	ref, _, err := q.admin.Git.GetRef(q.ctx, q.owner, q.name, "heads/"+canaryBranch)
	if err != nil || ref.GetObject().GetSHA() != revision {
		q.t.Fatalf("%s at %s, want %s: %v", canaryBranch, ref.GetObject().GetSHA(), revision, err)
	}
}

func (q *publishingQualification) createRef(branch, revision string) {
	q.t.Helper()
	if _, _, err := q.admin.Git.CreateRef(q.ctx, q.owner, q.name, github.CreateRef{Ref: "refs/heads/" + branch, SHA: revision}); err != nil {
		q.t.Fatalf("create %s: %v", branch, err)
	}
}

func (q *publishingQualification) applyRulesets(extra ...string) {
	q.t.Helper()
	live, err := liveRulesets(q.ctx, q.admin, q.owner, q.name)
	if err != nil {
		q.t.Fatal(err)
	}
	for _, name := range []string{"production", "production-history"} {
		document := q.document(name)
		refs := document["conditions"].(map[string]any)["ref_name"].(map[string]any)
		for _, branch := range extra {
			refs["include"] = append(refs["include"].([]any), "refs/heads/"+branch)
		}
		method, endpoint := http.MethodPost, fmt.Sprintf("repos/%s/%s/rulesets", q.owner, q.name)
		if existing := live[name]; existing != nil {
			method, endpoint = http.MethodPut, fmt.Sprintf("%s/%d", endpoint, existing.GetID())
		}
		request, err := q.admin.NewRequest(q.ctx, method, endpoint, document)
		if err != nil {
			q.t.Fatal(err)
		}
		var applied github.RepositoryRuleset
		if _, err := q.admin.Do(request, &applied); err != nil {
			q.t.Fatalf("%s ruleset %s: %v", method, name, err)
		}
		q.t.Logf("ruleset %s %d targets %v", name, applied.GetID(), refs["include"])
	}
}

func (q *publishingQualification) probeCommit() string {
	q.t.Helper()
	workflow, err := os.ReadFile(filepath.Join(q.root, ".github/workflows/reconcile-job.yml"))
	if err != nil {
		q.t.Fatal(err)
	}
	checkout := regexp.MustCompile(`uses: (actions/checkout@[a-f0-9]{40})`).FindSubmatch(workflow)
	if checkout == nil {
		q.t.Fatal("pinned checkout action not found")
	}
	probe := fmt.Sprintf(`name: Production canary probe
on:
  push:
    branches: [%s]
permissions:
  contents: write
jobs:
  probe:
    runs-on: ubuntu-24.04
    timeout-minutes: 5
    steps:
    - uses: %s
      with:
        fetch-depth: 2
    - run: |
        set -o pipefail
        if git push origin HEAD~1:refs/heads/%s 2>&1 | tee "$RUNNER_TEMP/push.log"; then
          echo '::error::The workflow token updated %s'
          exit 1
        fi
        grep -q GH013 "$RUNNER_TEMP/push.log"
`, probeBranch, checkout[1], canaryBranch, canaryBranch)
	q.git(nil, "read-tree", q.current)
	blob := q.git([]byte(probe), "hash-object", "-w", "--stdin")
	q.git(nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+probeWorkflow)
	return q.git(nil, "commit-tree", q.git(nil, "write-tree"), "-p", q.current, "-m", "Probe "+canaryBranch+" with the workflow token")
}

func (q *publishingQualification) rejectedWorkflowToken() {
	q.t.Helper()
	commit := q.probeCommit()
	if output, err := q.push(q.adminToken, commit+":refs/heads/"+probeBranch); err != nil {
		q.t.Fatalf("push %s: %v\n%s", probeBranch, err, output)
	}
	deadline := time.Now().Add(probeDeadline)
	for {
		runs, _, err := q.admin.Actions.ListRepositoryWorkflowRuns(q.ctx, q.owner, q.name, &github.ListWorkflowRunsOptions{Branch: probeBranch, Event: "push", HeadSHA: commit})
		if err == nil && runs.GetTotalCount() > 0 && runs.WorkflowRuns[0].GetStatus() == "completed" {
			run := runs.WorkflowRuns[0]
			if run.GetConclusion() != "success" {
				q.t.Fatalf("workflow token probe %s concluded %s", run.GetHTMLURL(), run.GetConclusion())
			}
			q.t.Logf("workflow token push rejected: %s", run.GetHTMLURL())
			break
		}
		if time.Now().After(deadline) {
			q.t.Fatalf("workflow token probe on %s did not complete within %s: %v", commit, probeDeadline, err)
		}
		time.Sleep(10 * time.Second)
	}
	q.canaryAt(q.previous)
}

func (q *publishingQualification) restore() {
	q.applyRulesets()
	for _, branch := range []string{canaryBranch, probeBranch} {
		_, err := q.admin.Git.DeleteRef(q.ctx, q.owner, q.name, "heads/"+branch)
		var response *github.ErrorResponse
		if err != nil && !(errors.As(err, &response) && (response.Response.StatusCode == http.StatusNotFound || response.Response.StatusCode == http.StatusUnprocessableEntity)) {
			q.t.Errorf("delete %s: %v", branch, err)
		}
	}
}
