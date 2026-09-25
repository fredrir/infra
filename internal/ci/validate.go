package ci

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/fredrir/infra/internal/fluxartifacts"
	"github.com/fredrir/infra/internal/kustomize"
	"golang.org/x/sync/errgroup"
)

func validationInputs(ctx context.Context, runner Runner, before string) ([]byte, error) {
	var files []byte
	var err error
	if revisionPattern.MatchString(before) {
		_, present := runner.Output(ctx, "git", "cat-file", "-e", before+"^{commit}")
		if present != nil {
			runner.Run(ctx, "git", "fetch", "--quiet", "--depth=1", "origin", before)
			_, present = runner.Output(ctx, "git", "cat-file", "-e", before+"^{commit}")
		}
		if present == nil {
			files, err = runner.Output(ctx, "git", "diff", "--name-only", "--no-renames", before, "HEAD")
			if err != nil {
				return nil, err
			}
		}
	}
	if files == nil {
		files, err = runner.Output(ctx, "git", "ls-files")
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func declarationChanged(files []byte, pattern string) bool {
	expression := regexp.MustCompile(pattern)
	for _, path := range strings.Split(string(files), "\n") {
		if expression.MatchString(path) {
			return true
		}
	}
	return false
}

func PrepareValidation(ctx context.Context, runner Runner, before string) error {
	files, err := validationInputs(ctx, runner, before)
	if err != nil {
		return err
	}
	if declarationChanged(files, `^tofu/`) {
		return runner.Run(ctx, "tofu", "-chdir=tofu", "init", "-backend=false", "-lockfile=readonly", "-input=false")
	}
	return nil
}

type declarationCheck func(context.Context, Runner) error

func Validate(ctx context.Context, runner Runner, before string) error {
	files, err := validationInputs(ctx, runner, before)
	if err != nil {
		return err
	}
	checks, err := declarationChecks(runner.Dir, func(pattern string) bool { return declarationChanged(files, pattern) })
	if err != nil {
		return err
	}
	return runChecks(ctx, runner, checks)
}

func declarationChecks(root string, changed func(string) bool) ([]declarationCheck, error) {
	command := func(name string, arguments ...string) declarationCheck {
		return func(ctx context.Context, runner Runner) error { return runner.Run(ctx, name, arguments...) }
	}
	var checks []declarationCheck
	if changed(`^tofu/`) {
		checks = append(checks, command("tofu", "-chdir=tofu", "validate"), command("tofu", "-chdir=tofu", "fmt", "-check", "-recursive"))
	}
	if changed(`^ansible/`) {
		playbooks, err := filepath.Glob(filepath.Join(root, "ansible", "*.yml"))
		if err != nil {
			return nil, err
		}
		arguments := []string{"-i", "localhost,", "--syntax-check"}
		for _, playbook := range playbooks {
			relative, err := filepath.Rel(root, playbook)
			if err != nil {
				return nil, err
			}
			arguments = append(arguments, relative)
		}
		checks = append(checks, command("ansible-playbook", arguments...))
	}
	if changed(`^(platform/projects/|platform/components/(policy|backup-job|repository-maintenance)/|platform/clusters/production/(root|settings)\.yaml$|build/rollout/flux-artifacts/|internal/fluxartifacts/|internal/ci/validate\.go$)`) {
		checks = append(checks, func(context.Context, Runner) error { return fluxartifacts.Check(root, kustomize.Build) })
	}
	if changed(`^(platform/|charts/|build/rollout/flux-artifacts/)`) {
		directories, err := kustomizations(root)
		if err != nil {
			return nil, err
		}
		checks = append(checks, func(ctx context.Context, _ Runner) error {
			var failures []error
			for _, directory := range directories {
				if ctx.Err() != nil {
					break
				}
				_, err := kustomize.Build(filepath.Join(root, directory))
				failures = append(failures, err)
			}
			return errors.Join(append(failures, ctx.Err())...)
		})
	}
	if changed(`^(charts/|tests/infra/fixtures/)`) {
		checks = append(checks, command("helm", "lint", "charts/project", "--strict", "-f", "tests/infra/fixtures/values.yaml"))
	}
	if changed(`^\.github/`) {
		checks = append(checks, command("actionlint"))
	}
	return checks, nil
}

func kustomizations(root string) ([]string, error) {
	candidates := []string{"platform/clusters/production", "platform/components", "platform/projects", "build/rollout/flux-artifacts/cutover"}
	for _, pattern := range []string{"platform/components/*", "platform/projects/*", "platform/projects/llunde-pyparser/*"} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				continue
			}
			relative, err := filepath.Rel(root, match)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, relative)
		}
	}
	var directories []string
	for _, directory := range candidates {
		if _, err := os.Stat(filepath.Join(root, directory, "kustomization.yaml")); err == nil {
			directories = append(directories, directory)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return directories, nil
}

func runChecks(ctx context.Context, runner Runner, checks []declarationCheck) error {
	transcripts := make([]transcript, len(checks))
	failures := make([]error, len(checks))
	var group errgroup.Group
	group.SetLimit(runtime.GOMAXPROCS(0))
	for index, check := range checks {
		group.Go(func() error {
			isolated := runner
			isolated.Stdout, isolated.Stderr = transcripts[index].writer(runner.Stdout), transcripts[index].writer(runner.Stderr)
			failures[index] = check(ctx, isolated)
			return nil
		})
	}
	group.Wait()
	for index := range transcripts {
		transcripts[index].replay()
	}
	return errors.Join(failures...)
}

type transcript struct {
	lock   sync.Mutex
	writes []transcriptWrite
}

type transcriptWrite struct {
	destination io.Writer
	data        []byte
}

type transcriptWriter struct {
	transcript  *transcript
	destination io.Writer
}

func (t *transcript) writer(destination io.Writer) io.Writer {
	if destination == nil {
		return nil
	}
	return transcriptWriter{transcript: t, destination: destination}
}

func (w transcriptWriter) Write(data []byte) (int, error) {
	w.transcript.lock.Lock()
	defer w.transcript.lock.Unlock()
	w.transcript.writes = append(w.transcript.writes, transcriptWrite{destination: w.destination, data: bytes.Clone(data)})
	return len(data), nil
}

func (t *transcript) replay() {
	for _, write := range t.writes {
		write.destination.Write(write.data)
	}
}
