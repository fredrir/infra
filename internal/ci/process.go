package ci

import (
	"context"
	"io"
	"os"

	"github.com/fredrir/infra/internal/process"
)

type Runner struct {
	Dir            string
	Env            []string
	Stdout, Stderr io.Writer
}

func (runner Runner) Run(ctx context.Context, name string, arguments ...string) error {
	_, err := process.Run(ctx, process.Options{Name: name, Args: arguments, Dir: runner.Dir, Env: append(os.Environ(), runner.Env...), Stdout: runner.Stdout, Stderr: runner.Stderr})
	return err
}

func (runner Runner) Output(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	result, err := process.Run(ctx, process.Options{Name: name, Args: arguments, Dir: runner.Dir, Env: append(os.Environ(), runner.Env...), Stderr: runner.Stderr})
	return result.Stdout, err
}
