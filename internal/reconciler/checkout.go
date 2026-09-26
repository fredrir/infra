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

func (e executor) checkoutPublished(ctx context.Context, source, repository string) (string, error) {
	tracking := "refs/remotes/origin/" + publishedBranch
	for _, step := range [][]string{
		{"init", "--quiet", "--template=", source},
		{"-C", source, "remote", "add", "origin", repository},
		{"-C", source, "fetch", "--quiet", "--no-tags", "origin", "+refs/heads/" + publishedBranch + ":" + tracking},
	} {
		if _, err := e.output(ctx, "", "git", step...); err != nil {
			return "", err
		}
	}
	revision, err := e.output(ctx, "", "git", "-C", source, "rev-parse", "--verify", tracking+"^{commit}")
	if err != nil {
		return "", err
	}
	if !revisionPattern.MatchString(revision) {
		return "", fmt.Errorf("%s resolved to an invalid revision", publishedBranch)
	}
	if _, err := e.output(ctx, "", "git", "-C", source, "checkout", "--quiet", "--detach", revision); err != nil {
		return "", err
	}
	return revision, nil
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
