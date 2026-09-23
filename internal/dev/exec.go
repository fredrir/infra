package dev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func execute(ctx context.Context, runner ci.Runner, options process.Options) (process.Result, error) {
	if options.Dir == "" {
		options.Dir = runner.Dir
	}
	options.Env = append(os.Environ(), append(runner.Env, options.Env...)...)
	if options.Stderr == nil {
		options.Stderr = runner.Stderr
	}
	if runner.Execute != nil {
		return runner.Execute(ctx, options)
	}
	return process.Run(ctx, options)
}

func capture(ctx context.Context, runner ci.Runner, name string, args ...string) (string, error) {
	var stderr bytes.Buffer
	result, err := execute(ctx, runner, process.Options{Name: name, Args: args, Stderr: &stderr})
	if err != nil {
		if detail := firstLine(stderr.String()); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

func runToFile(ctx context.Context, runner ci.Runner, path string, options process.Options) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	options.Stdout = file
	_, err = execute(ctx, runner, options)
	return errors.Join(err, file.Close())
}
