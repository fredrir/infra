package cli

import (
	"fmt"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/projects"
	"github.com/spf13/cobra"
)

func newProjectCommands() []*cobra.Command {
	provider := func(command *cobra.Command) projects.NativeProvider {
		return projects.NativeProvider{Runner: ci.Runner{Stdout: command.OutOrStdout(), Stderr: command.ErrOrStderr()}}
	}
	var options projects.OnboardOptions
	onboard := &cobra.Command{Use: "onboard REPOSITORY", Short: "Generate a restricted project and pinned workflow caller", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		options.Repository = args[0]
		if err := projects.Onboard(command.Context(), provider(command), options); err != nil {
			return err
		}
		_, err := fmt.Fprintf(command.OutOrStdout(), "Created %s; add encrypted project-registry/project-runtime Secrets before starting workloads\n", options.Output)
		return err
	}}
	flags := onboard.Flags()
	flags.StringVar(&options.Project, "project", "", "Project name")
	flags.StringVar(&options.Image, "image", "", "Immutable image reference")
	flags.StringVar(&options.SourceRevision, "source-revision", "", "Source commit")
	flags.StringVar(&options.WorkflowRef, "workflow-ref", "", "Infrastructure workflow commit")
	flags.IntVar(&options.Port, "port", 8080, "Application port")
	flags.StringVar(&options.HealthPath, "health-path", "/healthz", "Health endpoint")
	flags.StringVar(&options.Domain, "domain", "", "Shared gateway hostname")
	flags.StringVar(&options.Architecture, "architecture", "amd64", "Qualified architecture")
	flags.StringVar(&options.TestCommand, "test-command", "", "Build test command")
	flags.StringVar(&options.Output, "output", "", "Fresh output directory")
	for _, name := range []string{"project", "image", "source-revision", "workflow-ref", "domain", "test-command", "output"} {
		_ = onboard.MarkFlagRequired(name)
	}
	var rustOptions projects.RustOptions
	rust := &cobra.Command{Use: "onboard-rust REPOSITORY", Short: "Provision isolated Rust runner credentials and pinned callers", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		rustOptions.Repository = args[0]
		if err := projects.OnboardRust(command.Context(), provider(command), rustOptions); err != nil {
			return err
		}
		_, err := fmt.Fprintf(command.OutOrStdout(), "Onboarded %s as ci-%s; copy %s/project into the repository\n", rustOptions.Repository, rustOptions.Project, rustOptions.Output)
		return err
	}}
	rust.Flags().StringVar(&rustOptions.Project, "project", "", "Project name")
	rust.Flags().StringVar(&rustOptions.WorkflowRef, "workflow-ref", "", "Infrastructure workflow commit")
	rust.Flags().StringVar(&rustOptions.Output, "output", "", "Fresh caller output directory")
	rust.Flags().StringVar(&rustOptions.Root, "root", ".", "Infrastructure checkout")
	for _, name := range []string{"project", "workflow-ref", "output"} {
		_ = rust.MarkFlagRequired(name)
	}
	return []*cobra.Command{onboard, rust}
}
