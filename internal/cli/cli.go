package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/fredrir/infra/internal/images"
	"github.com/fredrir/infra/internal/kata"
	"github.com/spf13/cobra"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if filepath.Base(os.Args[0]) == "MAKEDEV" {
		return kata.MakeDevices(ctx, args, stderr)
	}
	if filepath.Base(os.Args[0]) == "infra-runner-hook" {
		args = []string{"platform", "runner-hook"}
	}
	root := &cobra.Command{Use: "infra", Short: "Build, verify, and operate infrastructure", SilenceUsage: true, SilenceErrors: true}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	root.CompletionOptions.DisableDefaultCmd = true
	ci := &cobra.Command{Use: "ci", Short: "Continuous integration commands", RunE: missingCommand}
	ci.AddCommand(newPlanImagesCommand(), newCheckCommand())
	root.AddCommand(ci, newPipelineCommand(), newDoctorCommand(), &cobra.Command{
		Use: "version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			info, ok := debug.ReadBuildInfo()
			if !ok {
				return errors.New("build information unavailable")
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), info.String())
			return err
		},
	})
	root.AddCommand(newKataCommand(), newPlatformCommand(), newArtifactCommand())
	registerAutomationCommands(root, ci)
	return root.ExecuteContext(ctx)
}

func missingCommand(cmd *cobra.Command, _ []string) error {
	return fmt.Errorf("%s requires a subcommand", cmd.CommandPath())
}

func newPlanImagesCommand() *cobra.Command {
	var root, catalog, registry, output string
	var refresh bool
	var timeout time.Duration
	cmd := &cobra.Command{Use: "plan-images", Short: "Print the pending image build matrix", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return errors.New("timeout must be positive")
			}
			planner := images.Planner{Root: root, Registry: registry, Refresh: refresh, Client: &http.Client{Timeout: timeout}, Log: cmd.ErrOrStderr()}
			plan, err := planner.Plan(cmd.Context(), catalog)
			if err != nil {
				return err
			}
			data, err := json.Marshal(plan)
			if err != nil {
				return err
			}
			if output != "" {
				file, err := os.OpenFile(output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				_, writeErr := fmt.Fprintf(file, "images=%s\n", data)
				if err := errors.Join(writeErr, file.Close()); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", data)
			return err
		}}
	cmd.Flags().StringVar(&root, "root", ".", "Repository directory")
	cmd.Flags().StringVar(&catalog, "catalog", "images/catalog.yaml", "Image catalog relative to repository")
	cmd.Flags().StringVar(&registry, "registry", envDefault("REGISTRY_URL", "https://ghcr.io"), "Registry URL")
	cmd.Flags().BoolVar(&refresh, "refresh", os.Getenv("REFRESH") == "true", "Build all images")
	cmd.Flags().StringVar(&output, "github-output", os.Getenv("GITHUB_OUTPUT"), "GitHub Actions output file")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "Registry request timeout")
	return cmd
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
