package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"dagger.io/dagger"
)

type ImageOptions struct {
	Target        string
	CheckOnly     bool
	InfraBinary   string
	SourceURL     string
	Revision      string
	Root          string
	Context       string
	Dockerfile    string
	Image         string
	Platform      string
	BuildArgs     map[string]string
	TestCommand   string
	TestShell     string
	RegistryUser  string
	RegistryToken string
	Export        string
	Log           io.Writer
}

func Image(ctx context.Context, opts ImageOptions) (string, error) {
	if opts.Image == "" && opts.Export == "" && !opts.CheckOnly {
		return "", errors.New("image reference or export path required")
	}
	if !filepath.IsLocal(opts.Dockerfile) {
		return "", errors.New("Dockerfile must be relative to the working directory")
	}
	if opts.Target != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`).MatchString(opts.Target) {
		return "", errors.New("invalid Dockerfile target")
	}
	if info, err := os.Stat(opts.Dockerfile); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("Dockerfile must be an existing regular file")
	}
	config, err := ReadToolchain(opts.Root)
	if err != nil {
		return "", err
	}
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(opts.Log))
	if err != nil {
		return "", err
	}
	defer client.Close()
	version, err := client.Version(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimPrefix(version, "v") != config.Dagger {
		return "", fmt.Errorf("Dagger %s required, got %s", config.Dagger, version)
	}
	var args []dagger.BuildArg
	for name, value := range opts.BuildArgs {
		args = append(args, dagger.BuildArg{Name: name, Value: value})
	}
	sort.Slice(args, func(i, j int) bool { return args[i].Name < args[j].Name })
	source := client.Host().Directory(opts.Context, dagger.HostDirectoryOpts{Gitignore: true, Exclude: []string{".git", "dist", "bazel-*", ".cache", ".direnv", ".venv", "**/.terraform", ".env", ".env.*"}}).WithFile(".infra.Containerfile", client.Host().File(opts.Dockerfile))
	if opts.InfraBinary != "" {
		source = source.WithFile(".infra-artifacts/infra", client.Host().File(opts.InfraBinary), dagger.DirectoryWithFileOpts{Permissions: 0o755})
	}
	container := source.DockerBuild(dagger.DirectoryDockerBuildOpts{Platform: dagger.Platform(opts.Platform), Dockerfile: ".infra.Containerfile", Target: opts.Target, BuildArgs: args})
	if opts.SourceURL != "" {
		container = container.WithLabel("org.opencontainers.image.source", opts.SourceURL)
	}
	if opts.Revision != "" {
		container = container.WithLabel("org.opencontainers.image.revision", opts.Revision)
	}
	if opts.TestCommand != "" {
		shell := opts.TestShell
		if shell == "" {
			shell = "sh"
		}
		arguments := []string{shell, "-euc", opts.TestCommand}
		if shell == "bash" {
			arguments = []string{shell, "-euo", "pipefail", "-c", opts.TestCommand}
		}
		if _, err := container.WithExec(arguments).Sync(ctx); err != nil {
			return "", fmt.Errorf("image verification: %w", err)
		}
	}
	if opts.CheckOnly {
		_, err := container.Sync(ctx)
		return "", err
	}
	if opts.Export != "" {
		if _, err := container.Export(ctx, opts.Export); err != nil {
			return "", err
		}
	}
	if opts.Image == "" {
		return "", nil
	}
	if opts.RegistryToken != "" {
		host, _, found := strings.Cut(opts.Image, "/")
		if !found || opts.RegistryUser == "" {
			return "", errors.New("registry credentials require a qualified image and registry user")
		}
		container = container.WithRegistryAuth(host, opts.RegistryUser, client.SetSecret("registry-token", opts.RegistryToken))
	}
	return container.Publish(ctx, opts.Image)
}
