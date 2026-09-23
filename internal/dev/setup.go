package dev

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"golang.org/x/sync/errgroup"
)

type SetupOptions struct {
	State    State
	Runner   ci.Runner
	Client   *http.Client
	Platform string
	Assets   func(string) (ci.ToolAsset, bool)
	Log      io.Writer
}

func (opts SetupOptions) defaults() SetupOptions {
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 2 * time.Minute}
	}
	if opts.Platform == "" {
		opts.Platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	if opts.Assets == nil {
		opts.Assets = ci.Tool
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	return opts
}

func Setup(ctx context.Context, opts SetupOptions) error {
	opts = opts.defaults()
	if opts.Platform != "linux/amd64" {
		return fmt.Errorf("pinned tools are published for linux/amd64 only; install the tools reported by infra dev doctor on %s", opts.Platform)
	}
	if err := os.MkdirAll(opts.State.Tools(), 0o755); err != nil {
		return err
	}
	group, installContext := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for _, tool := range Tools {
		asset, ok := opts.Assets(tool.Name)
		if !ok {
			return fmt.Errorf("no pinned asset for %s", tool.Name)
		}
		destination := filepath.Join(opts.State.Tools(), tool.Name)
		group.Go(func() error {
			if err := ci.InstallTool(installContext, opts.Client, asset, destination); err != nil {
				return fmt.Errorf("install %s: %w", tool.Name, err)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	for _, tool := range Tools {
		asset, _ := opts.Assets(tool.Name)
		fmt.Fprintln(opts.Log, "Installed:", tool.Name, PinnedVersion(asset))
	}
	uv := filepath.Join(opts.State.Tools(), "uv")
	return opts.Runner.Run(ctx, uv, "sync", "--frozen", "--group", "ci", "--no-install-project")
}
