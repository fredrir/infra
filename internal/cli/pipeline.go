package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/pipeline"
	"github.com/spf13/cobra"
)

func newCheckCommand() *cobra.Command { return newBuildCommand("check", "test") }

func newPipelineCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "pipeline", Short: "Execute reproducible builds", RunE: missingCommand}
	cmd.AddCommand(newBuildCommand("build", "build"), newBuildCommand("check", "test"), newBuildCommand("check-fast", "fast-check"), newBuildCommand("check-deep", "test"), newBuildCommand("prepare-check", "prepare-check"), newBuildCommand("generate-check", "generate-check"), newBuildCommand("cache-gc", "cache-gc"), newImageCommand())
	var root, base string
	affected := &cobra.Command{Use: "affected", Short: "Print a conservative Bazel query for changed targets", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := pipeline.AffectedExpression(cmd.Context(), root, base)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result)
			return err
		}}
	affected.Flags().StringVar(&root, "root", ".", "Repository directory")
	affected.Flags().StringVar(&base, "base", "", "Git revision to compare against")
	cmd.AddCommand(affected)
	return cmd
}

func newBuildCommand(name, operation string) *cobra.Command {
	opts := pipeline.Options{Operation: operation}
	var timeout time.Duration
	cmd := &cobra.Command{Use: name + " [targets...]", Short: "Run Bazel through the pinned build environment", Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, targets []string) error {
			opts.Targets, opts.Log = targets, cmd.ErrOrStderr()
			if opts.RemoteCache != "" {
				if !cmd.Flags().Changed("disk-cache") {
					opts.DiskCache = ""
				}
				if !cmd.Flags().Changed("repository-cache") {
					opts.RepositoryCache = ""
				}
			}
			if timeout <= 0 {
				return errors.New("timeout must be positive")
			}
			if operation == "fast-check" && timeout > 10*time.Second {
				return errors.New("fast checks have a maximum aggregate budget of 10 seconds")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			if operation == "fast-check" {
				reports, err := pipeline.CheckFast(ctx, opts)
				return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(reports))
			}
			report, err := pipeline.Run(ctx, opts)
			printErr := json.NewEncoder(cmd.OutOrStdout()).Encode(report)
			return errors.Join(err, printErr)
		}}
	defaultTimeout := 30 * time.Minute
	if operation == "fast-check" {
		defaultTimeout = 10 * time.Second
	}
	cmd.Flags().DurationVar(&timeout, "timeout", defaultTimeout, "Pipeline execution timeout")
	cmd.Flags().StringVar(&opts.Root, "root", ".", "Repository directory")
	cmd.Flags().BoolVar(&opts.Local, "local", name == "check-deep", "Run installed Bazel directly")
	cmd.Flags().StringVar(&opts.Bazel, "bazel", "bazel", "Local Bazel executable")
	cmd.Flags().StringVar(&opts.Base, "base", "", "Select targets affected since this Git revision")
	cacheRoot, _ := os.UserCacheDir()
	diskCache, repositoryCache := "", ""
	if cacheRoot != "" {
		diskCache = filepath.Join(cacheRoot, "infra-bazel-actions")
		repositoryCache = filepath.Join(cacheRoot, "infra-bazel-repository")
	}
	cmd.Flags().StringVar(&opts.DiskCache, "disk-cache", diskCache, "Local Bazel action cache directory")
	cmd.Flags().StringVar(&opts.RepositoryCache, "repository-cache", repositoryCache, "Local Bazel repository download cache directory")
	cmd.Flags().StringVar(&opts.RemoteCache, "remote-cache", os.Getenv("BAZEL_REMOTE_CACHE"), "Bazel remote cache URL")
	cmd.Flags().StringVar(&opts.RemoteExecutor, "remote-executor", os.Getenv("BAZEL_REMOTE_EXECUTOR"), "Optional Bazel remote execution endpoint")
	cmd.Flags().BoolVar(&opts.ForwardLocalCache, "forward-local-cache", false, "Forward a client-host loopback cache into Dagger")
	cmd.Flags().BoolVar(&opts.ReadOnlyCache, "read-only-cache", false, "Disable remote cache writes")
	cmd.Flags().StringVar(&opts.ReportDir, "report-dir", "", "Directory for result, build events and timing profile")
	return cmd
}

func newDoctorCommand() *cobra.Command {
	var root, bazel string
	var engine, engineOnly bool
	cmd := &cobra.Command{Use: "doctor", Short: "Check the local toolchain and optional Dagger connection", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if engineOnly {
				bazel, engine = "", true
			}
			checks := pipeline.Doctor(cmd.Context(), root, bazel, engine)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(checks); err != nil {
				return err
			}
			for _, check := range checks {
				if !check.OK {
					return errors.New("toolchain diagnostics failed")
				}
			}
			return nil
		}}
	cmd.Flags().StringVar(&root, "root", ".", "Repository directory")
	cmd.Flags().StringVar(&bazel, "bazel", "bazel", "Local Bazel executable")
	cmd.Flags().BoolVar(&engine, "engine", false, "Connect to Dagger and verify engine version")
	cmd.Flags().BoolVar(&engineOnly, "engine-only", false, "Verify Dagger without requiring local Bazel")
	return cmd
}

func newImageCommand() *cobra.Command {
	var opts pipeline.ImageOptions
	var buildArgs []string
	var argsJSON, output string
	var timeout time.Duration
	cmd := &cobra.Command{Use: "image", Short: "Build, verify, and publish an image with Dagger", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.BuildArgs = make(map[string]string)
			if argsJSON != "" {
				if err := json.Unmarshal([]byte(argsJSON), &opts.BuildArgs); err != nil {
					return err
				}
			}
			for _, arg := range buildArgs {
				key, value, ok := strings.Cut(arg, "=")
				if !ok || key == "" {
					return fmt.Errorf("invalid build argument %q", arg)
				}
				opts.BuildArgs[key] = value
			}
			opts.Log = cmd.ErrOrStderr()
			opts.RegistryToken = os.Getenv("REGISTRY_TOKEN")
			if timeout <= 0 {
				return errors.New("timeout must be positive")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			result, err := pipeline.Image(ctx, opts)
			if err != nil {
				return err
			}
			if output != "" && result != "" {
				file, err := os.OpenFile(output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				_, digest, _ := strings.Cut(result, "@")
				_, err = fmt.Fprintf(file, "image=%s\ndigest=%s\n", result, digest)
				if err := errors.Join(err, file.Close()); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result)
			return err
		}}
	cmd.Flags().DurationVar(&timeout, "timeout", 45*time.Minute, "Image build and publication timeout")
	cmd.Flags().StringVar(&opts.Root, "root", ".", "Infrastructure repository directory")
	cmd.Flags().StringVar(&opts.SourceURL, "source-url", "", "Source repository URL")
	cmd.Flags().StringVar(&opts.Revision, "revision", "", "Source revision")
	cmd.Flags().StringVar(&opts.Context, "context", ".", "Image build context")
	cmd.Flags().StringVar(&opts.Dockerfile, "dockerfile", "Dockerfile", "Dockerfile relative to the working directory")
	cmd.Flags().StringVar(&opts.CheckTarget, "check-target", "", "Dockerfile stage exporting measured receipts at /infra-checks")
	cmd.Flags().StringVar(&opts.CheckReportDir, "check-report-dir", "", "Directory for aggregate image check receipts")
	cmd.Flags().StringVar(&opts.Target, "target", "", "Dockerfile build stage")
	cmd.Flags().BoolVar(&opts.CheckOnly, "check-only", false, "Build and verify without export or publication")
	cmd.Flags().StringVar(&opts.InfraBinary, "infra-binary", "", "Prebuilt infrastructure binary for runner images")
	cmd.Flags().StringVar(&opts.Image, "image", "", "Image reference to publish")
	cmd.Flags().StringVar(&opts.Platform, "platform", "linux/amd64", "Target platform")
	cmd.Flags().StringArrayVar(&buildArgs, "build-arg", nil, "Build argument KEY=VALUE")
	cmd.Flags().StringVar(&argsJSON, "build-args", "", "JSON object of build arguments")
	cmd.Flags().StringVar(&opts.TestCommand, "test-command", "", "Command to verify the built image")
	cmd.Flags().StringVar(&opts.TestShell, "test-shell", "sh", "Shell used for image verification")
	cmd.Flags().StringVar(&opts.Export, "export", "", "Export an OCI image archive")
	cmd.Flags().StringVar(&opts.ExportDirectory, "export-directory", "", "Export the image filesystem to a directory")
	cmd.Flags().StringVar(&opts.RegistryUser, "registry-user", os.Getenv("GITHUB_ACTOR"), "Registry username for REGISTRY_TOKEN")
	cmd.Flags().StringVar(&output, "github-output", os.Getenv("GITHUB_OUTPUT"), "GitHub Actions output file")
	return cmd
}
