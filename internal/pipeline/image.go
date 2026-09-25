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
	"time"

	"github.com/fredrir/infra/internal/ci"

	"dagger.io/dagger"
)

type ImageOptions struct {
	CheckTarget     string
	CheckReportDir  string
	Target          string
	CheckOnly       bool
	InfraBinary     string
	SourceURL       string
	Revision        string
	Root            string
	Context         string
	Dockerfile      string
	Image           string
	Platform        string
	BuildArgs       map[string]string
	TestCommand     string
	TestShell       string
	RegistryUser    string
	RegistryToken   string
	Export          string
	ExportDirectory string
	Log             io.Writer
}

func Image(ctx context.Context, opts ImageOptions) (string, error) {
	if opts.Image == "" && opts.Export == "" && opts.ExportDirectory == "" && !opts.CheckOnly {
		return "", errors.New("image reference or export path required")
	}
	if !filepath.IsLocal(opts.Dockerfile) {
		return "", errors.New("Dockerfile must be relative to the working directory")
	}
	for _, target := range []string{opts.Target, opts.CheckTarget} {
		if target != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`).MatchString(target) {
			return "", errors.New("invalid Dockerfile target")
		}
	}
	if (opts.CheckTarget != "" || opts.TestCommand != "") && (opts.CheckReportDir == "" || opts.InfraBinary == "") {
		return "", errors.New("measured image checks require an infra binary and check report directory")
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
	engine, found, err := inspectEngine(ctx)
	if err != nil {
		return "", err
	}
	if found {
		if opts.BuildArgs == nil {
			opts.BuildArgs = map[string]string{}
		}
		for name, value := range goBuildArgs(engine) {
			if _, set := opts.BuildArgs[name]; !set {
				opts.BuildArgs[name] = value
			}
		}
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
	if opts.CheckTarget != "" {
		checks := source.DockerBuild(dagger.DirectoryDockerBuildOpts{Platform: dagger.Platform(opts.Platform), Dockerfile: ".infra.Containerfile", Target: opts.CheckTarget, BuildArgs: args})
		directory := filepath.Join(opts.CheckReportDir, "inline")
		if _, err := checks.Directory("/infra-checks").Export(ctx, directory); err != nil {
			return "", fmt.Errorf("export inline check receipts: %w", err)
		}
		if _, err := ci.CheckGroupBudget(directory, 10*time.Second, true); err != nil {
			return "", err
		}
		if _, err := ci.CheckGroupBudget(opts.CheckReportDir, 10*time.Second, true); err != nil {
			return "", err
		}
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
		if _, err := container.Sync(ctx); err != nil {
			return "", err
		}
		group, err := ci.CheckGroupBudget(opts.CheckReportDir, 10*time.Second, false)
		if err != nil {
			return "", err
		}
		budget := time.Duration(group.RemainingSeconds * float64(time.Second)).String()
		command := append([]string{"/tmp/infra-measure", "ci", "measure", "--stage", "image-smoke", "--budget", budget, "--report-dir", "/tmp/infra-checks", "--"}, arguments...)
		checked := container.WithMountedFile("/tmp/infra-measure", client.Host().File(opts.InfraBinary)).WithEnvVariable("GITHUB_SHA", opts.Revision).WithExec(command, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
		code, err := checked.ExitCode(ctx)
		if err != nil {
			return "", fmt.Errorf("image verification: %w", err)
		}
		_, exportErr := checked.Directory("/tmp/infra-checks").Export(ctx, filepath.Join(opts.CheckReportDir, "smoke"))
		_, checkErr := ci.CheckGroupBudget(opts.CheckReportDir, 10*time.Second, true)
		if code != 0 {
			return "", errors.Join(fmt.Errorf("image verification exited with status %d", code), exportErr, checkErr)
		}
		if err := errors.Join(exportErr, checkErr); err != nil {
			return "", err
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
	if opts.ExportDirectory != "" {
		if _, err := container.Rootfs().Export(ctx, opts.ExportDirectory); err != nil {
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
