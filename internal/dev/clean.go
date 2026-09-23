package dev

import (
	"context"
	"fmt"
	"io"
	"os"
)

type CleanOptions struct {
	State State
	Log   io.Writer
}

func Clean(_ context.Context, opts CleanOptions) error {
	if err := os.RemoveAll(opts.State.Cache); err != nil {
		return err
	}
	if opts.Log != nil {
		fmt.Fprintln(opts.Log, "Removed:", opts.State.Cache)
	}
	return nil
}
