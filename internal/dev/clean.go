package dev

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/fredrir/infra/internal/ci"
)

type CleanOptions struct {
	State  State
	Runner ci.Runner
	All    bool
	Log    io.Writer
}

func Clean(ctx context.Context, opts CleanOptions) error {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.All {
		for _, profile := range EngineProfiles() {
			if err := StopEngine(ctx, EngineOptions{State: opts.State, Runner: opts.Runner, Profile: profile, Log: opts.Log}, true); err != nil {
				return err
			}
		}
		if err := ClusterDown(ctx, ClusterOptions{State: opts.State, Runner: opts.Runner, Log: opts.Log}); err != nil {
			return err
		}
		if err := HostsDown(ctx, HostsOptions{State: opts.State, Runner: opts.Runner, Log: opts.Log}, true); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.RemoveAll(opts.State.Cache); err != nil {
		return err
	}
	fmt.Fprintln(opts.Log, "Removed:", opts.State.Cache)
	return nil
}
