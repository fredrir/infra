package ci

import (
	"bytes"
	"context"
	"fmt"
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
	var suites []string
	python := `^(pyproject\.toml|uv\.lock)$`
	if changed(python + `|^(scripts/(ci|packages)/|\.github/|tests/(ci|fixtures|golden)/|images/|platform/components/(policy|runners)/)`) {
		suites = append(suites, "tests/ci")
	}
	if changed(python + `|^(tools/|tests/infra/|platform/components/runners/|charts/)`) {
		suites = append(suites, "tests/infra")
	}
	if changed(python + `|^(scripts/operations/|tests/operations/|platform/components/)`) {
		suites = append(suites, "tests/operations")
	}
	if len(suites) > 0 {
		if err := runner.Run(ctx, "uv", "sync", "--frozen", "--group", "ci", "--quiet"); err != nil {
			return err
		}
		type result struct {
			suite  string
			stdout bytes.Buffer
			stderr bytes.Buffer
			err    error
		}
		results := make(chan *result, len(suites))
		for _, suite := range suites {
			go func() {
				output := &result{suite: suite}
				local := runner
				local.Stdout, local.Stderr = &output.stdout, &output.stderr
				if library := os.Getenv("CHECK_LIBRARY_PATH"); library != "" {
					local.Env = append(local.Env, "LD_LIBRARY_PATH="+library)
				}
				output.err = local.Run(ctx, "uv", "run", "--no-sync", "python", "-m", "unittest", "discover", "-s", suite)
				results <- output
			}()
		}
		failed := false
		for range suites {
			output := <-results
			fmt.Fprintf(runner.Stdout, "::group::%s\n%s%s::endgroup::\n", output.suite, output.stdout.String(), output.stderr.String())
			failed = failed || output.err != nil
		}
		if failed {
			return fmt.Errorf("retained infrastructure test suites failed")
		}
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
