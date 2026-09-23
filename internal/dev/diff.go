package dev

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

var ErrDifferences = errors.New("cluster differs from the local declarations")

type DiffOptions struct {
	State  State
	Runner ci.Runner
	Stdout io.Writer
	Stderr io.Writer
}

func Diff(ctx context.Context, opts DiffOptions) error {
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	t, err := selectTarget("")
	if err != nil {
		return err
	}
	arguments := append([]string{"diff"}, append(t.arguments(), "--ignore-paths=**/*.sops.yaml", "--progress-bar=false")...)
	result, err := execute(ctx, opts.Runner, process.Options{Name: "flux", Args: arguments, Stdout: opts.Stdout, Stderr: opts.Stderr})
	if result.ExitCode == 1 {
		return ErrDifferences
	}
	if err != nil {
		return fmt.Errorf("flux diff: %w", err)
	}
	return nil
}
