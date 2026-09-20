package release

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fredrir/infra/internal/ci"
)

type PrepareOptions struct{ Temporary, Repository, RefType, RefName, CliffConfig, GitHubOutput string }

func Prepare(ctx context.Context, runner ci.Runner, options PrepareOptions) error {
	root, err := filepath.Abs(runner.Dir)
	if err != nil {
		return err
	}
	runner.Dir = root
	if options.CliffConfig != "" {
		options.CliffConfig, err = filepath.Abs(options.CliffConfig)
		if err != nil {
			return err
		}
	}
	if options.Temporary == "" {
		return fmt.Errorf("runner temporary directory is required")
	}
	metadata, err := runner.Output(ctx, "cargo", "metadata", "--format-version", "1", "--no-deps", "--locked")
	if err != nil {
		return err
	}
	metadataPath := filepath.Join(options.Temporary, "cargo-metadata.json")
	if err := os.WriteFile(metadataPath, metadata, 0600); err != nil {
		return err
	}
	summaryPath := filepath.Join(options.Temporary, "release.json")
	if err := Render(RenderOptions{Metadata: metadataPath, Root: runner.Dir, Repository: options.Repository, Dist: filepath.Join(options.Temporary, "dist"), SDK: filepath.Join(options.Temporary, "macos-sdk/MacOSX.sdk"), Config: filepath.Join(options.Temporary, "goreleaser.yaml"), Summary: summaryPath}); err != nil {
		return err
	}
	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		return err
	}
	var settings Settings
	if err := json.Unmarshal(summary, &settings); err != nil {
		return err
	}
	if options.GitHubOutput != "" {
		file, err := os.OpenFile(options.GitHubOutput, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(file, "name=%s\nprerelease=%t\n", settings.Name, options.RefType == "tag" && strings.Contains(options.RefName, "-"))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	config := filepath.Join(runner.Dir, "cliff.toml")
	if _, err := os.Stat(config); os.IsNotExist(err) {
		config = options.CliffConfig
	} else if err != nil {
		return err
	}
	if config == "" {
		return fmt.Errorf("a git-cliff configuration is required")
	}
	arguments := []string{"--config", config}
	if options.RefType == "tag" {
		if !strings.HasPrefix(options.RefName, "v") || strings.ContainsAny(options.RefName, "\r\n\x00") {
			return fmt.Errorf("invalid release tag")
		}
		tags, err := runner.Output(ctx, "git", "tag", "--list", "v*", "--sort=-v:refname")
		if err != nil {
			return err
		}
		arguments = append(arguments, "--tag", options.RefName, "--strip", "all")
		for _, tag := range strings.Fields(string(tags)) {
			if tag != options.RefName && !strings.Contains(tag, "-") {
				arguments = append(arguments, tag+".."+options.RefName)
				break
			}
		}
	} else {
		arguments = append(arguments, "--unreleased", "--tag", "v0.0.0-dry-run", "--strip", "all")
	}
	notes, err := runner.Output(ctx, "git-cliff", arguments...)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(options.Temporary, "notes.md"), notes, 0644); err != nil {
		return err
	}
	status, err := runner.Output(ctx, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if len(status) > 0 {
		return fmt.Errorf("release preparation modified the source checkout")
	}
	return nil
}

func Build(ctx context.Context, runner ci.Runner, temporary string, snapshot bool) error {
	if temporary == "" {
		return fmt.Errorf("runner temporary directory is required")
	}
	arguments := []string{"release", "--config", filepath.Join(temporary, "goreleaser.yaml"), "--clean", "--skip=publish,announce", "--release-notes", filepath.Join(temporary, "notes.md")}
	if snapshot {
		arguments = append(arguments, "--snapshot")
	}
	return runner.Run(ctx, "goreleaser", arguments...)
}

func Draft(ctx context.Context, runner ci.Runner, bundle, repository, tag string, prerelease bool) error {
	if !releaseRepository.MatchString(repository) || !strings.HasPrefix(tag, "v") || strings.ContainsAny(tag, "\r\n\x00") {
		return fmt.Errorf("invalid release repository or tag")
	}
	if err := VerifyChecksums(filepath.Join(bundle, "assets")); err != nil {
		return err
	}
	arguments := []string{"release", "create", tag, "--repo", repository, "--draft", "--verify-tag", "--title", tag, "--notes-file", filepath.Join(bundle, "notes.md")}
	if prerelease {
		arguments = append(arguments, "--prerelease")
	}
	entries, err := os.ReadDir(filepath.Join(bundle, "assets"))
	if err != nil {
		return err
	}
	var assets []string
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			assets = append(assets, filepath.Join(bundle, "assets", entry.Name()))
		} else {
			return fmt.Errorf("release assets must be regular files")
		}
	}
	slices.Sort(assets)
	return runner.Run(ctx, "gh", append(arguments, assets...)...)
}
