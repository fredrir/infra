package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/release"
	"github.com/spf13/cobra"
)

func registerAutomationCommands(root, ciCommand *cobra.Command) {
	ciCommand.AddCommand(newTimelineCommand())
	root.AddCommand(newPackagesCommand())
	root.AddCommand(newProjectCommands()...)
	tag := &cobra.Command{Use: "tag-image", Short: "Tag a checksum-verified OCI manifest", Args: cobra.NoArgs}
	var tagOptions ci.TagOptions
	tag.Flags().StringVar(&tagOptions.Image, "image", os.Getenv("IMAGE"), "Immutable image reference")
	tag.Flags().StringVar(&tagOptions.Tag, "tag", os.Getenv("TAG"), "Destination tag")
	tag.Flags().StringVar(&tagOptions.Registry, "registry", environmentValue("REGISTRY_URL", "https://ghcr.io"), "Registry URL")
	tag.RunE = func(command *cobra.Command, _ []string) error {
		tagOptions.Actor, tagOptions.Token = os.Getenv("GITHUB_ACTOR"), os.Getenv("REGISTRY_TOKEN")
		if err := ci.TagImage(command.Context(), &http.Client{Timeout: 60 * time.Second}, tagOptions); err != nil {
			return err
		}
		_, err := fmt.Fprintf(command.OutOrStdout(), "Tagged %s as %s\n", tagOptions.Image, tagOptions.Tag)
		return err
	}
	var revision, buildInput string
	buildArgs := &cobra.Command{Use: "build-args", Short: "Validate public image build arguments", Args: cobra.NoArgs}
	buildArgs.Flags().StringVar(&revision, "revision", os.Getenv("GITHUB_SHA"), "Source revision")
	buildArgs.Flags().StringVar(&buildInput, "arguments", os.Getenv("BUILD_ARGS"), "Newline-separated public build arguments")
	buildArgs.RunE = func(command *cobra.Command, _ []string) error {
		arguments, err := ci.BuildArguments(revision, buildInput)
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(arguments)
	}
	ciCommand.AddCommand(tag, buildArgs)
	deploy := &cobra.Command{Use: "deploy ROOT", Short: "Publish a provenance-verified deployment", Args: cobra.ExactArgs(1)}
	deploy.RunE = func(command *cobra.Command, args []string) error {
		options := ci.DeployOptions{Root: args[0], RepositoryID: os.Getenv("SOURCE_REPOSITORY_ID"), Revision: os.Getenv("SOURCE_REVISION"), Image: os.Getenv("IMAGE_NAME"), Digest: os.Getenv("IMAGE_DIGEST"), Token: os.Getenv("DEPLOY_TOKEN")}
		return ci.DeployAuthenticated(command.Context(), ci.Runner{Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, options, os.Getenv("GITHUB_ACTOR"), os.Getenv("REGISTRY_TOKEN"))
	}
	ciCommand.AddCommand(deploy)
	reconcile := &cobra.Command{Use: "reconcile URL", Args: cobra.ExactArgs(1), Short: "Notify the deployment reconciler", RunE: func(command *cobra.Command, args []string) error {
		return ci.NotifyReconciler(command.Context(), args[0], command.ErrOrStderr())
	}}
	ciCommand.AddCommand(reconcile)
	var rustTemporary, rustRoot, clippyArguments, testArguments string
	rust := &cobra.Command{Use: "rust", Short: "Run reproducible Rust checks"}
	rust.PersistentFlags().StringVar(&rustRoot, "root", ".", "Source checkout")
	rust.PersistentFlags().StringVar(&rustTemporary, "temporary", os.Getenv("RUNNER_TEMP"), "Runner temporary directory")
	prepare := &cobra.Command{Use: "prepare", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return ci.PrepareRust(command.Context(), ci.Runner{Dir: rustRoot, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, rustTemporary, clippyArguments, testArguments)
	}}
	prepare.Flags().StringVar(&clippyArguments, "clippy-args", os.Getenv("CLIPPY_ARGS"), "Allowed Clippy arguments")
	prepare.Flags().StringVar(&testArguments, "test-args", os.Getenv("TEST_ARGS"), "Allowed test arguments")
	checkRust := &cobra.Command{Use: "check fast|deep|toolchain|prepare-fast|format|lint|test|unit|docs|minimal|msrv|audit", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		return ci.CheckRust(command.Context(), ci.Runner{Dir: rustRoot, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, rustTemporary, args[0])
	}}
	rust.AddCommand(prepare, checkRust)
	cacheRust := &cobra.Command{Use: "cache restore|save", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		return ci.RustCache(command.Context(), ci.Runner{Dir: rustRoot, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, rustTemporary, args[0])
	}}
	rust.AddCommand(cacheRust)
	ciCommand.AddCommand(rust)
	var postgresTemporary, postgresVariable string
	postgres := &cobra.Command{Use: "postgres start|stop", Args: cobra.ExactArgs(1), Short: "Manage the isolated CI database", RunE: func(command *cobra.Command, args []string) error {
		runner := ci.Runner{Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}
		switch args[0] {
		case "start":
			return ci.StartPostgres(command.Context(), runner, postgresTemporary, postgresVariable)
		case "stop":
			return ci.StopPostgres(command.Context(), runner, postgresTemporary)
		default:
			return fmt.Errorf("PostgreSQL action must be start or stop")
		}
	}}
	postgres.Flags().StringVar(&postgresTemporary, "temporary", os.Getenv("RUNNER_TEMP"), "Runner temporary directory")
	postgres.Flags().StringVar(&postgresVariable, "url-env", os.Getenv("URL_ENV"), "Database URL environment variable")
	ciCommand.AddCommand(postgres)
	var sdkTemporary string
	fetchSDK := &cobra.Command{Use: "fetch-macos-sdk", Args: cobra.NoArgs, Short: "Download and verify the macOS SDK", RunE: func(command *cobra.Command, _ []string) error {
		return ci.FetchSDK(command.Context(), sdkTemporary, command.OutOrStdout())
	}}
	fetchSDK.Flags().StringVar(&sdkTemporary, "temporary", os.Getenv("RUNNER_TEMP"), "Runner temporary directory")
	ciCommand.AddCommand(fetchSDK)
	var toolsTemporary, toolsPath string
	installTools := &cobra.Command{Use: "install-tools TOOL...", Args: cobra.MinimumNArgs(1), Short: "Install checksum-pinned CI tools", RunE: func(command *cobra.Command, args []string) error {
		return ci.InstallTools(command.Context(), toolsTemporary, toolsPath, args)
	}}
	installTools.Flags().StringVar(&toolsTemporary, "temporary", os.Getenv("RUNNER_TEMP"), "Runner temporary directory")
	installTools.Flags().StringVar(&toolsPath, "github-path", os.Getenv("GITHUB_PATH"), "GitHub Actions path output")
	ciCommand.AddCommand(installTools)
	var validationRoot, validationBefore string
	validate := &cobra.Command{Use: "validate", Short: "Validate changed infrastructure declarations", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return ci.Validate(command.Context(), ci.Runner{Dir: validationRoot, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, validationBefore)
	}}
	validate.Flags().StringVar(&validationRoot, "root", ".", "Source checkout")
	validate.Flags().StringVar(&validationBefore, "before", os.Getenv("BEFORE"), "Previous Git revision")
	ciCommand.AddCommand(validate)
	releaseCommand := &cobra.Command{Use: "release", Short: "Prepare and verify release artifacts"}
	var tagsPath string
	version := &cobra.Command{Use: "next-version MANIFEST", Short: "Update a Cargo release version from published tags", Args: cobra.ExactArgs(1)}
	version.Flags().StringVar(&tagsPath, "tags", "", "File containing published tags")
	version.MarkFlagRequired("tags")
	version.RunE = func(command *cobra.Command, args []string) error {
		tags, err := os.ReadFile(tagsPath)
		if err != nil {
			return err
		}
		next, err := release.UpdateVersion(args[0], strings.Fields(string(tags)))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(command.OutOrStdout(), next)
		return err
	}
	releaseCommand.AddCommand(version)
	var renderOptions release.RenderOptions
	render := &cobra.Command{Use: "render", Short: "Generate a Rust release configuration", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error { return release.Render(renderOptions) }}
	for name, value := range map[string]*string{"metadata": &renderOptions.Metadata, "root": &renderOptions.Root, "repository": &renderOptions.Repository, "dist": &renderOptions.Dist, "sdk": &renderOptions.SDK, "config": &renderOptions.Config, "summary": &renderOptions.Summary} {
		render.Flags().StringVar(value, name, "", name+" path or value")
		render.MarkFlagRequired(name)
	}
	var releaseRoot, cliffConfig string
	tagRelease := &cobra.Command{Use: "tag", Short: "Commit and publish the next Cargo release tag", Args: cobra.NoArgs}
	tagRelease.Flags().StringVar(&releaseRoot, "root", ".", "Source checkout")
	tagRelease.Flags().StringVar(&cliffConfig, "cliff-config", "", "Fallback git-cliff configuration")
	tagRelease.RunE = func(command *cobra.Command, _ []string) error {
		return release.Tag(command.Context(), ci.Runner{Dir: releaseRoot, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, cliffConfig)
	}
	releaseCommand.AddCommand(render, tagRelease)
	bundle := &cobra.Command{Use: "bundle DIST SUMMARY NOTES DESTINATION", Short: "Verify and stage release assets", Args: cobra.ExactArgs(4), RunE: func(command *cobra.Command, args []string) error {
		return release.Bundle(args[0], args[1], args[2], args[3])
	}}
	releaseCommand.AddCommand(bundle)
	var releaseTemporary, releaseRepository, releaseRefType, releaseRefName, releaseOutput, releaseCliff, prepareRoot string
	prepareRelease := &cobra.Command{Use: "prepare", Short: "Prepare release metadata and notes", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return release.Prepare(command.Context(), ci.Runner{Dir: prepareRoot, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, release.PrepareOptions{Temporary: releaseTemporary, Repository: releaseRepository, RefType: releaseRefType, RefName: releaseRefName, CliffConfig: releaseCliff, GitHubOutput: releaseOutput})
	}}
	prepareRelease.Flags().StringVar(&prepareRoot, "root", ".", "Source checkout")
	prepareRelease.Flags().StringVar(&releaseTemporary, "temporary", os.Getenv("RUNNER_TEMP"), "Runner temporary directory")
	prepareRelease.Flags().StringVar(&releaseRepository, "repository", os.Getenv("GITHUB_REPOSITORY"), "Source repository")
	prepareRelease.Flags().StringVar(&releaseRefType, "ref-type", os.Getenv("GITHUB_REF_TYPE"), "Source reference type")
	prepareRelease.Flags().StringVar(&releaseRefName, "ref-name", os.Getenv("GITHUB_REF_NAME"), "Source reference name")
	prepareRelease.Flags().StringVar(&releaseOutput, "github-output", os.Getenv("GITHUB_OUTPUT"), "GitHub Actions output file")
	prepareRelease.Flags().StringVar(&releaseCliff, "cliff-config", "", "Fallback git-cliff configuration")
	var buildTemporary, buildRoot string
	var snapshot bool
	buildRelease := &cobra.Command{Use: "build", Short: "Build all configured release targets", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return release.Build(command.Context(), ci.Runner{Dir: buildRoot, Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, buildTemporary, snapshot)
	}}
	buildRelease.Flags().StringVar(&buildRoot, "root", ".", "Source checkout")
	buildRelease.Flags().StringVar(&buildTemporary, "temporary", os.Getenv("RUNNER_TEMP"), "Runner temporary directory")
	buildRelease.Flags().BoolVar(&snapshot, "snapshot", os.Getenv("SNAPSHOT") == "true", "Create a snapshot release")
	var draftBundle, draftRepository, draftTag string
	var prerelease bool
	draftRelease := &cobra.Command{Use: "draft", Short: "Upload a verified draft release", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return release.Draft(command.Context(), ci.Runner{Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}, draftBundle, draftRepository, draftTag, prerelease)
	}}
	draftRelease.Flags().StringVar(&draftBundle, "bundle", "", "Verified release bundle")
	draftRelease.MarkFlagRequired("bundle")
	draftRelease.Flags().StringVar(&draftRepository, "repository", os.Getenv("GITHUB_REPOSITORY"), "Source repository")
	draftRelease.Flags().StringVar(&draftTag, "tag", os.Getenv("GITHUB_REF_NAME"), "Release tag")
	draftRelease.Flags().BoolVar(&prerelease, "prerelease", os.Getenv("PRERELEASE") == "true", "Publish a prerelease")
	releaseCommand.AddCommand(prepareRelease, buildRelease, draftRelease)
	root.AddCommand(releaseCommand)
}

func environmentValue(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
