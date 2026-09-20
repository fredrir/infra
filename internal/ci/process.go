package ci

import (
	"context"
	"io"
	"os"

	"github.com/fredrir/infra/internal/process"
)

type Runner struct {
	Execute        func(context.Context, process.Options) (process.Result, error)
	Dir            string
	Env            []string
	Stdout, Stderr io.Writer
}

func (runner Runner) Run(ctx context.Context, name string, arguments ...string) error {
	_, err := runner.execute(ctx, process.Options{Name: name, Args: arguments, Dir: runner.Dir, Env: append(os.Environ(), runner.Env...), Stdout: runner.Stdout, Stderr: runner.Stderr})
	return err
}

func (runner Runner) Output(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	result, err := runner.execute(ctx, process.Options{Name: name, Args: arguments, Dir: runner.Dir, Env: append(os.Environ(), runner.Env...), Stderr: runner.Stderr})
	return result.Stdout, err
}

func (runner Runner) execute(ctx context.Context, options process.Options) (process.Result, error) {
	if runner.Execute != nil {
		return runner.Execute(ctx, options)
	}
	return process.Run(ctx, options)
}
