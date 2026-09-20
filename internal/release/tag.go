package release

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fredrir/infra/internal/ci"
)

func Tag(ctx context.Context, runner ci.Runner, cliffConfig string) error {
	root, err := filepath.Abs(runner.Dir)
	if err != nil {
		return err
	}
	runner.Dir = root
	if cliffConfig != "" {
		cliffConfig, err = filepath.Abs(cliffConfig)
		if err != nil {
			return err
		}
	}
	tags, err := runner.Output(ctx, "git", "tag", "--list", "v*")
	if err != nil {
		return err
	}
	next, err := UpdateVersion(filepath.Join(runner.Dir, "Cargo.toml"), strings.Fields(string(tags)))
	if err != nil {
		return err
	}
	for _, tag := range strings.Fields(string(tags)) {
		if tag == "v"+next {
			_, err := fmt.Fprintf(runner.Stdout, "v%s already exists\n", next)
			return err
		}
	}
	files := []string{"Cargo.toml", "CHANGELOG.md"}
	if _, err := os.Stat(filepath.Join(runner.Dir, "Cargo.lock")); err == nil {
		if err := runner.Run(ctx, "cargo", "update", "--workspace", "--quiet"); err != nil {
			return err
		}
		files = append(files, "Cargo.lock")
	} else if !os.IsNotExist(err) {
		return err
	}
	config := filepath.Join(runner.Dir, "cliff.toml")
	if _, err := os.Stat(config); os.IsNotExist(err) {
		config = cliffConfig
	} else if err != nil {
		return err
	}
	if config == "" {
		return fmt.Errorf("a git-cliff configuration is required")
	}
	if err := runner.Run(ctx, "git-cliff", "--config", config, "--tag", "v"+next, "--output", "CHANGELOG.md"); err != nil {
		return err
	}
	if err := runner.Run(ctx, "git", "add", "--", files[0], files[1]); err != nil {
		return err
	}
	if len(files) > 2 {
		if err := runner.Run(ctx, "git", "add", "--", "Cargo.lock"); err != nil {
			return err
		}
	}
	changed, err := runner.Output(ctx, "git", "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	identity := []string{"-c", "user.name=github-actions[bot]", "-c", "user.email=41898282+github-actions[bot]@users.noreply.github.com"}
	if len(changed) > 0 {
		if err := runner.Run(ctx, "git", append(identity, "commit", "--quiet", "--message", "release: v"+next)...); err != nil {
			return err
		}
		if err := runner.Run(ctx, "git", "push", "--quiet", "origin", "HEAD:main"); err != nil {
			return err
		}
	}
	if err := runner.Run(ctx, "git", append(identity, "tag", "--annotate", "v"+next, "--message", "v"+next)...); err != nil {
		return err
	}
	if err := runner.Run(ctx, "git", "push", "--quiet", "origin", "refs/tags/v"+next); err != nil {
		return err
	}
	_, err = fmt.Fprintf(runner.Stdout, "Tagged v%s\n", next)
	return err
}
