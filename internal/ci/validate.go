package ci

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func Validate(ctx context.Context, runner Runner, before string) error {
	var files []byte
	var err error
	if revisionPattern.MatchString(before) {
		_, present := runner.Output(ctx, "git", "cat-file", "-e", before+"^{commit}")
		if present != nil {
			runner.Run(ctx, "git", "fetch", "--quiet", "--depth=1", "origin", before)
			_, present = runner.Output(ctx, "git", "cat-file", "-e", before+"^{commit}")
		}
		if present == nil {
			files, err = runner.Output(ctx, "git", "diff", "--name-only", before, "HEAD")
			if err != nil {
				return err
			}
		}
	}
	if files == nil {
		files, err = runner.Output(ctx, "git", "ls-files")
		if err != nil {
			return err
		}
	}
	changed := func(pattern string) bool {
		expression := regexp.MustCompile(pattern)
		for _, path := range strings.Split(string(files), "\n") {
			if expression.MatchString(path) {
				return true
			}
		}
		return false
	}
	if changed(`^ansible/`) {
		if err := runner.Run(ctx, "ansible-playbook", "-i", "localhost,", "ansible/site.yml", "--syntax-check"); err != nil {
			return err
		}
	}
	if changed(`^\.github/`) {
		if err := runner.Run(ctx, "actionlint"); err != nil {
			return err
		}
	}
	if changed(`^(platform|charts)/`) {
		directories := []string{"platform/clusters/production"}
		for _, pattern := range []string{"platform/components/*", "platform/projects/*"} {
			matches, err := filepath.Glob(filepath.Join(runner.Dir, pattern))
			if err != nil {
				return err
			}
			for _, match := range matches {
				relative, err := filepath.Rel(runner.Dir, match)
				if err != nil {
					return err
				}
				directories = append(directories, relative)
			}
		}
		for _, directory := range directories {
			if _, err := os.Stat(filepath.Join(runner.Dir, directory, "kustomization.yaml")); err == nil {
				if _, err := runner.Output(ctx, "kubectl", "kustomize", directory); err != nil {
					return err
				}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	if changed(`^(charts/|tests/infra/fixtures/)`) {
		if err := runner.Run(ctx, "helm", "lint", "charts/project", "--strict", "-f", "tests/infra/fixtures/values.yaml"); err != nil {
			return err
		}
	}
	if changed(`^tofu/`) {
		for _, arguments := range [][]string{{"-chdir=tofu", "fmt", "-check", "-recursive"}, {"-chdir=tofu", "init", "-backend=false", "-lockfile=readonly", "-input=false"}, {"-chdir=tofu", "validate"}} {
			if err := runner.Run(ctx, "tofu", arguments...); err != nil {
				return err
			}
		}
	}
	return nil
}
