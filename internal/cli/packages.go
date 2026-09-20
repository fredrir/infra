package cli

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/packages"
	"github.com/spf13/cobra"
)

func newPackagesCommand() *cobra.Command {
	root := &cobra.Command{Use: "packages", Short: "Build, verify and publish package repositories"}
	var source, temporary, binary string
	root.PersistentFlags().StringVar(&source, "root", ".", "Infrastructure checkout")
	root.PersistentFlags().StringVar(&temporary, "temporary", os.Getenv("RUNNER_TEMP"), "Runner temporary directory")
	root.PersistentFlags().StringVar(&binary, "infra-binary", "", "Prebuilt Linux amd64 infra binary")
	runner := func(command *cobra.Command) ci.Runner {
		return ci.Runner{Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}
	}
	options := func(command *cobra.Command) packages.PipelineOptions {
		return packages.PipelineOptions{Root: source, Temporary: temporary, Binary: binary, Log: command.ErrOrStderr(), Output: command.OutOrStdout()}
	}
	build := &cobra.Command{Use: "build", Short: "Build signed package repositories through Dagger", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return packages.Build(command.Context(), options(command))
	}}
	var key string
	index := &cobra.Command{Use: "index apt|rpm|apk DIRECTORY", Short: "Create a signed package index", Args: cobra.ExactArgs(2), RunE: func(command *cobra.Command, args []string) error {
		return packages.Index(command.Context(), runner(command), args[0], args[1], key)
	}}
	index.Flags().StringVar(&key, "key", "", "Signing key path")
	index.MarkFlagRequired("key")
	smoke := &cobra.Command{Use: "smoke SITE CHANNELS quick|full", Short: "Install packages across pinned distributions", Args: cobra.ExactArgs(3), RunE: func(command *cobra.Command, args []string) error {
		return packages.Smoke(command.Context(), options(command), args[0], args[1], args[2])
	}}
	install := &cobra.Command{Use: "smoke-install deb|rpm|apk PACKAGE BINARY [PACKAGE BINARY...]", Short: "Verify installation inside a test container", Args: cobra.MinimumNArgs(3), Hidden: true, RunE: func(command *cobra.Command, args []string) error {
		return packages.SmokeInstallBatch(command.Context(), runner(command), args[0], args[1:])
	}}
	var repository string
	pages := &cobra.Command{Use: "publish-pages SITE", Short: "Publish verified repositories to GitHub Pages", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		return packages.PublishPages(command.Context(), runner(command), args[0], repository)
	}}
	pages.Flags().StringVar(&repository, "repository", os.Getenv("GITHUB_REPOSITORY"), "Package site repository")
	var channels, knownHosts string
	publish := &cobra.Command{Use: "publish-channels", Short: "Publish Homebrew, NUR and AUR channels", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		return packages.PublishChannels(command.Context(), runner(command), channels, knownHosts)
	}}
	publish.Flags().StringVar(&channels, "channels", "", "Verified channel directory")
	publish.MarkFlagRequired("channels")
	publish.Flags().StringVar(&knownHosts, "known-hosts", "", "Override the pinned AUR host keys")
	var collectOptions packages.CollectOptions
	collect := &cobra.Command{Use: "collect", Short: "Collect and package attested stable releases", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		tools, err := packages.Collect(command.Context(), &packages.GitHub{Runner: runner(command)}, packages.NFPM{Runner: runner(command)}, collectOptions)
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(tools)
	}}
	for name, value := range map[string]*string{"registry": &collectOptions.Registry, "work": &collectOptions.Work, "site": &collectOptions.Site, "channels": &collectOptions.Channels} {
		collect.Flags().StringVar(value, name, "", name+" path")
		collect.MarkFlagRequired(name)
	}
	var sitePath, toolsPath string
	site := &cobra.Command{Use: "site", Short: "Generate package repository pages and installer", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		data, err := os.ReadFile(toolsPath)
		if err != nil {
			return err
		}
		var tools packages.Tools
		if err := json.Unmarshal(data, &tools); err != nil {
			return err
		}
		return packages.BuildSite(sitePath, tools, packages.PublicGPG, packages.PublicAPK)
	}}
	site.Flags().StringVar(&sitePath, "site", "", "Package site directory")
	site.MarkFlagRequired("site")
	site.Flags().StringVar(&toolsPath, "tools", filepath.Join(os.Getenv("RUNNER_TEMP"), "packages/channels/tools.json"), "Tool release catalog")
	root.AddCommand(build, index, smoke, install, pages, publish, collect, site)
	return root
}
