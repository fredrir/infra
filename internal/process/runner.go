package process

import (
	"context"
	"io"
	"os"
)

type Runner struct {
	Execute        func(context.Context, Options) (Result, error)
	Dir            string
	Env            []string
	Stdout, Stderr io.Writer
}

func (runner Runner) Run(ctx context.Context, name string, arguments ...string) error {
	_, err := runner.Invoke(ctx, Options{Name: name, Args: arguments, Dir: runner.Dir, Env: append(os.Environ(), runner.Env...), Stdout: runner.Stdout, Stderr: runner.Stderr})
	return err
}

func (runner Runner) Output(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	result, err := runner.Invoke(ctx, Options{Name: name, Args: arguments, Dir: runner.Dir, Env: append(os.Environ(), runner.Env...), Stderr: runner.Stderr})
	return result.Stdout, err
}

func (runner Runner) Invoke(ctx context.Context, options Options) (Result, error) {
	if runner.Execute != nil {
		return runner.Execute(ctx, options)
	}
	return Run(ctx, options)
}
