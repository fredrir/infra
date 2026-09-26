package reconciler

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fredrir/infra/internal/process"
	"github.com/fredrir/infra/internal/reconcile"
)

const publishedBranch = "production"

type executor struct {
	execute func(context.Context, process.Options) (process.Result, error)
	env     []string
	log     io.Writer
}

func (e executor) run(ctx context.Context, dir string, env []string, name string, args ...string) (process.Result, error) {
	execute := e.execute
	if execute == nil {
		execute = process.Run
	}
	return execute(ctx, process.Options{Name: name, Args: args, Dir: dir, Env: append(append([]string{}, e.env...), env...), Stdout: e.log, Stderr: e.log})
}

func (e executor) output(ctx context.Context, dir, name string, args ...string) (string, error) {
	quiet := e
	quiet.log = nil
	result, err := quiet.run(ctx, dir, nil, name, args...)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(result.Stderr)))
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

var hardenedGit = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_NO_REPLACE_OBJECTS=1",
	"GIT_CONFIG_COUNT=2",
	"GIT_CONFIG_KEY_0=core.hooksPath",
	"GIT_CONFIG_VALUE_0=/dev/null",
	"GIT_CONFIG_KEY_1=core.fsmonitor",
	"GIT_CONFIG_VALUE_1=false",
}

const reviewedBranch = "main"

type publication struct {
	Revision string
	Main     string
	OnMain   bool
}

func (e executor) clone(ctx context.Context, source, repository string, branches ...string) (map[string]string, error) {
	steps := [][]string{{"init", "--quiet", "--template=", source}, {"-C", source, "remote", "add", "origin", repository}}
	fetch := []string{"-C", source, "fetch", "--quiet", "--no-tags", "origin"}
	for _, branch := range branches {
		fetch = append(fetch, "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	}
	for _, step := range append(steps, fetch) {
		if _, err := e.output(ctx, "", "git", step...); err != nil {
			return nil, err
		}
	}
	revisions := map[string]string{}
	for _, branch := range branches {
		revision, err := e.output(ctx, "", "git", "-C", source, "rev-parse", "--verify", "refs/remotes/origin/"+branch+"^{commit}")
		if err != nil {
			return nil, err
		}
		if !revisionPattern.MatchString(revision) {
			return nil, fmt.Errorf("%s resolved to an invalid revision", branch)
		}
		revisions[branch] = revision
	}
	return revisions, nil
}

func (e executor) checkoutPublished(ctx context.Context, source, repository string) (publication, error) {
	revisions, err := e.clone(ctx, source, repository, publishedBranch, reviewedBranch)
	if err != nil {
		return publication{}, err
	}
	revision, main := revisions[publishedBranch], revisions[reviewedBranch]
	contained, err := e.ancestor(ctx, source, revision, main)
	switch {
	case err != nil:
		return publication{}, fmt.Errorf("%s ancestry: %w", publishedBranch, err)
	case !contained:
		return publication{Revision: revision, Main: main}, nil
	}
	if _, err := e.output(ctx, "", "git", "-C", source, "checkout", "--quiet", "--detach", revision); err != nil {
		return publication{}, err
	}
	return publication{Revision: revision, Main: main, OnMain: true}, nil
}

func (e executor) checkoutMain(ctx context.Context, source, repository string) (string, error) {
	revisions, err := e.clone(ctx, source, repository, reviewedBranch)
	if err != nil {
		return "", err
	}
	if _, err := e.output(ctx, "", "git", "-C", source, "checkout", "--quiet", "--detach", revisions[reviewedBranch]); err != nil {
		return "", err
	}
	return revisions[reviewedBranch], nil
}

func (e executor) ancestor(ctx context.Context, source, revision, descendant string) (bool, error) {
	quiet := e
	quiet.log = nil
	result, err := quiet.run(ctx, "", nil, "git", "-C", source, "merge-base", "--is-ancestor", revision, descendant)
	switch {
	case err == nil:
		return true, nil
	case result.ExitCode == 1:
		return false, nil
	default:
		return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(result.Stderr)))
	}
}

func (e executor) remoteMain(ctx context.Context, repository string) (string, error) {
	output, err := e.output(ctx, "", "git", "ls-remote", "--exit-code", repository, "refs/heads/"+reviewedBranch)
	if err != nil {
		return "", err
	}
	revision, _, _ := strings.Cut(output, "\t")
	if !revisionPattern.MatchString(revision) {
		return "", fmt.Errorf("%s resolved to an invalid revision", reviewedBranch)
	}
	return revision, nil
}

func offMain(revision string) *reconcile.Verification {
	return &reconcile.Verification{
		Revision:    revision,
		Scope:       reconcile.ScopeCloud,
		Outcome:     reconcile.OutcomeDiffers,
		Differences: []reconcile.Difference{{System: "revision", Item: publishedBranch + " at " + revision + " is not on " + reviewedBranch}},
		Errors:      []string{},
	}
}

func clearDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := removeTree(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func removeTree(path string) error {
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			return os.Chmod(current, 0o700)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(path)
}
