package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/deployment"
	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

type CIChannel struct {
	Schema   int    `json:"schema"`
	Tag      string `json:"tag"`
	Release  string `json:"release"`
	Revision string `json:"revision"`
}

type ChannelAPI struct {
	Client *http.Client
	Base   string
	Token  string
}

const ChannelFile = "build/ci-channel.json"

const (
	checksStartAttempts = 10
	checksStartInterval = 30 * time.Second
)

var ciReleasePattern = regexp.MustCompile(`^ci-v1\.[0-9]+\.[0-9]+$`)

func (api ChannelAPI) request(ctx context.Context, method, path string, payload, result any) (int, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(api.Base, "/")+"/repos/fredrir/infra/"+path, body)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	request.Header.Set("Authorization", "Bearer "+api.Token)
	client := *api.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("GitHub API redirect refused") }
	response, err := client.Do(request)
	if err != nil {
		return 0, errors.New("GitHub API unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("GitHub API returned HTTP %d", response.StatusCode)
	}
	if result != nil {
		err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(result)
	}
	return response.StatusCode, err
}

func (api ChannelAPI) ReleaseRevision(ctx context.Context, tag string) (string, error) {
	if !ciReleasePattern.MatchString(tag) {
		return "", errors.New("an exact ci-v1 release required")
	}
	var release struct {
		Immutable  bool   `json:"immutable"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Tag        string `json:"tag_name"`
	}
	if _, err := api.request(ctx, http.MethodGet, "releases/tags/"+tag, nil, &release); err != nil {
		return "", err
	}
	if !release.Immutable || release.Draft || release.Prerelease || release.Tag != tag {
		return "", errors.New("published immutable CI release required")
	}
	var ref struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if _, err := api.request(ctx, http.MethodGet, "git/ref/tags/"+tag, nil, &ref); err != nil {
		return "", err
	}
	for depth := 0; ref.Object.Type == "tag" && depth < 4; depth++ {
		if !revisionPattern.MatchString(ref.Object.SHA) {
			return "", errors.New("invalid release tag object")
		}
		if _, err := api.request(ctx, http.MethodGet, "git/tags/"+ref.Object.SHA, nil, &ref); err != nil {
			return "", err
		}
	}
	if ref.Object.Type != "commit" || !revisionPattern.MatchString(ref.Object.SHA) {
		return "", errors.New("CI release must resolve to a commit")
	}
	return ref.Object.SHA, nil
}

func (api ChannelAPI) Checked(ctx context.Context, revision string) error {
	if !revisionPattern.MatchString(revision) {
		return errors.New("invalid CI revision")
	}
	var workflows struct {
		Runs []struct {
			Branch     string `json:"head_branch"`
			Event      string `json:"event"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			SHA        string `json:"head_sha"`
		} `json:"workflow_runs"`
	}
	if _, err := api.request(ctx, http.MethodGet, "actions/workflows/reconcile.yml/runs?head_sha="+revision+"&branch=main&event=push&per_page=100", nil, &workflows); err != nil {
		return err
	}
	for _, workflow := range workflows.Runs {
		if workflow.Branch != "main" || workflow.Event != "push" || workflow.SHA != revision {
			continue
		}
		if workflow.Status == "completed" && workflow.Conclusion == "success" {
			return nil
		}
		break
	}
	return errors.New("infrastructure checks missing or failed")
}

func (api ChannelAPI) Publish(ctx context.Context, tag, revision string) error {
	if !ciReleasePattern.MatchString(tag) || !revisionPattern.MatchString(revision) {
		return errors.New("invalid CI release")
	}
	if err := api.Checked(ctx, revision); err != nil {
		return err
	}
	var settings struct {
		Enabled bool `json:"enabled"`
	}
	if _, err := api.request(ctx, http.MethodGet, "immutable-releases", nil, &settings); err != nil {
		return err
	}
	if !settings.Enabled {
		return errors.New("enable immutable releases before publishing CI")
	}
	var existing struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	status, err := api.request(ctx, http.MethodGet, "git/ref/tags/"+tag, nil, &existing)
	if status == 404 {
		if _, err = api.request(ctx, http.MethodPost, "git/refs", map[string]string{"ref": "refs/tags/" + tag, "sha": revision}, nil); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if existing.Object.SHA != revision {
		return errors.New("CI release tag already names another commit")
	}
	status, err = api.request(ctx, http.MethodGet, "releases/tags/"+tag, nil, nil)
	if status == 404 {
		_, err = api.request(ctx, http.MethodPost, "releases", map[string]any{"tag_name": tag, "target_commitish": revision, "name": tag, "body": "Shared CI workflows.", "draft": false, "prerelease": false}, nil)
	}
	if err != nil {
		return err
	}
	resolved, err := api.ReleaseRevision(ctx, tag)
	if err != nil {
		return err
	}
	if resolved != revision {
		return errors.New("published CI release revision mismatch")
	}
	return nil
}

func ProposeChannel(ctx context.Context, runner process.Runner) error {
	data, err := os.ReadFile(filepath.Join(runner.Dir, ChannelFile))
	if err != nil {
		return err
	}
	var channel CIChannel
	if err := json.Unmarshal(data, &channel); err != nil {
		return err
	}
	if !revisionPattern.MatchString(channel.Revision) {
		return errors.New("invalid promotion revision")
	}
	branch := "ci-promotion-" + channel.Revision[:12]
	paths, err := filepath.Glob(filepath.Join(runner.Dir, ".github/chainguard/deploy-*.sts.yaml"))
	if err != nil {
		return err
	}
	arguments := []string{"add", "--", ChannelFile}
	for _, path := range paths {
		relative, err := filepath.Rel(runner.Dir, path)
		if err != nil {
			return err
		}
		arguments = append(arguments, relative)
	}
	if err := runner.Run(ctx, "git", "checkout", "-b", branch); err != nil {
		return err
	}
	if err := runner.Run(ctx, "git", arguments...); err != nil {
		return err
	}
	if err := runner.Run(ctx, "git", "-c", "user.name=github-actions", "-c", "user.email=github-actions@users.noreply.github.com", "commit", "-m", "ci: promote "+channel.Release); err != nil {
		return err
	}
	if err := runner.Run(ctx, "git", "push", "origin", branch); err != nil {
		return err
	}
	body, err := os.CreateTemp("", "infra-ci-promotion-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(body.Name())
	_, writeErr := fmt.Fprintf(body, "Promote `%s` to `ci-v1` and approve deployment provenance from `%s`.\n", channel.Release, channel.Revision)
	if err := errors.Join(writeErr, body.Close()); err != nil {
		return err
	}
	if err := runner.Run(ctx, "gh", "pr", "create", "--repo", "fredrir/infra", "--base", "main", "--head", branch, "--title", "ci: promote "+channel.Release, "--body-file", body.Name()); err != nil {
		return err
	}
	return mergeProposal(ctx, runner, branch)
}

func mergeProposal(ctx context.Context, runner process.Runner, branch string) error {
	for attempt := 1; ; attempt++ {
		err := runner.Run(ctx, "gh", "pr", "checks", branch, "--repo", "fredrir/infra", "--watch", "--fail-fast")
		if err == nil {
			data, readErr := runner.Output(ctx, "gh", "pr", "checks", branch, "--repo", "fredrir/infra", "--json", "name,state")
			if readErr != nil {
				return readErr
			}
			var checks []struct{ Name, State string }
			if err := json.Unmarshal(data, &checks); err != nil {
				return err
			}
			for _, check := range checks {
				if !slices.Contains([]string{"SUCCESS", "SKIPPED", "NEUTRAL"}, check.State) {
					err = fmt.Errorf("promotion check %s is %s", check.Name, check.State)
					break
				}
			}
			for _, name := range []string{"check / budget", "reconcile / plan"} {
				if !slices.ContainsFunc(checks, func(check struct{ Name, State string }) bool {
					return check.Name == name && (check.State == "SUCCESS" || name == "check / budget" && check.State == "SKIPPED")
				}) {
					err = fmt.Errorf("promotion check %s has not completed successfully", name)
					break
				}
			}
			if err == nil {
				break
			}
		}
		if attempt == checksStartAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(checksStartInterval):
		}
	}
	return runner.Run(ctx, "gh", "pr", "merge", branch, "--repo", "fredrir/infra", "--merge", "--delete-branch")
}

func ChannelFiles(root fs.FS, channel CIChannel) (map[string][]byte, error) {
	if channel.Schema != 1 || channel.Tag != "ci-v1" || !ciReleasePattern.MatchString(channel.Release) || !revisionPattern.MatchString(channel.Revision) {
		return nil, errors.New("invalid CI channel")
	}
	paths, err := fs.Glob(root, ".github/chainguard/deploy-*.sts.yaml")
	if err != nil || len(paths) == 0 {
		return nil, errors.New("deployment trust policies missing")
	}
	updates := map[string][]byte{}
	for _, path := range paths {
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return nil, err
		}
		var policy map[string]any
		if err := yaml.Unmarshal(data, &policy); err != nil {
			return nil, err
		}
		claims, ok := policy["claim_pattern"].(map[string]any)
		if !ok {
			return nil, errors.New("deployment trust claims missing")
		}
		pattern, ok := claims["job_workflow_sha"].(string)
		if !ok {
			return nil, errors.New("deployment workflow revisions missing")
		}
		revisions, err := deployment.WorkflowRevisions(pattern)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(revisions, channel.Revision) {
			revisions = append(revisions, channel.Revision)
		}
		slices.Sort(revisions)
		claims["job_workflow_sha"] = "^(" + strings.Join(revisions, "|") + ")$"
		claims["job_workflow_ref"] = `^fredrir/infra/\.github/workflows/build-image\.yml@(refs/tags/ci-v1|` + strings.Join(revisions, "|") + ")$"
		claims["event_name"] = "^(push|workflow_dispatch)$"
		if _, err := deployment.WorkflowRevisions(claims["job_workflow_sha"].(string)); err != nil {
			return nil, err
		}
		updates[path], err = yaml.Marshal(policy)
		if err != nil {
			return nil, err
		}
	}
	data, err := json.MarshalIndent(channel, "", "  ")
	if err != nil {
		return nil, err
	}
	updates[ChannelFile] = append(data, '\n')
	return updates, nil
}

func PrepareChannel(root string, channel CIChannel) error {
	updates, err := ChannelFiles(os.DirFS(root), channel)
	if err != nil {
		return err
	}
	for path, data := range updates {
		target := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

func (api ChannelAPI) Promote(ctx context.Context, root string) error {
	data, err := os.ReadFile(filepath.Join(root, ChannelFile))
	if err != nil {
		return err
	}
	var channel CIChannel
	if err := json.Unmarshal(data, &channel); err != nil {
		return err
	}
	if channel.Schema != 1 || channel.Tag != "ci-v1" || !revisionPattern.MatchString(channel.Revision) {
		return errors.New("invalid CI channel")
	}
	revision, err := api.ReleaseRevision(ctx, channel.Release)
	if err != nil {
		return err
	}
	if revision != channel.Revision {
		return errors.New("CI release revision mismatch")
	}
	if err := api.Checked(ctx, revision); err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(root, ".github/chainguard/deploy-*.sts.yaml"))
	if err != nil || len(paths) == 0 {
		return errors.New("deployment trust missing")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var trust struct {
			Claims struct {
				SHA string `yaml:"job_workflow_sha"`
				Ref string `yaml:"job_workflow_ref"`
			} `yaml:"claim_pattern"`
		}
		if err := yaml.Unmarshal(data, &trust); err != nil {
			return err
		}
		revisions, err := deployment.WorkflowRevisions(trust.Claims.SHA)
		if err != nil || !slices.Contains(revisions, revision) {
			return errors.New("candidate deployment trust missing")
		}
		pattern, err := regexp.Compile(trust.Claims.Ref)
		if err != nil || !pattern.MatchString("fredrir/infra/.github/workflows/build-image.yml@refs/tags/ci-v1") {
			return errors.New("CI channel deployment identity missing")
		}
	}
	status, err := api.request(ctx, http.MethodGet, "git/ref/tags/ci-v1", nil, nil)
	if status == 404 {
		_, err = api.request(ctx, http.MethodPost, "git/refs", map[string]string{"ref": "refs/tags/ci-v1", "sha": revision}, nil)
	} else if err == nil {
		_, err = api.request(ctx, http.MethodPatch, "git/refs/tags/ci-v1", map[string]any{"sha": revision, "force": true}, nil)
	}
	return err
}
