package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/dev"
	"github.com/spf13/cobra"
)

func newDevCommand() *cobra.Command {
	var root string
	cmd := &cobra.Command{Use: "dev", Short: "Develop, simulate and benchmark locally", RunE: missingCommand}
	cmd.PersistentFlags().StringVar(&root, "root", ".", "Repository directory")
	var bazel string
	doctor := &cobra.Command{Use: "doctor", Short: "Check pinned tools, Docker, KVM, kubeconfig and the Ansible environment", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := dev.Doctor(cmd.Context(), dev.DoctorOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: io.Discard}, Bazel: bazel})
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(checks); err != nil {
				return err
			}
			for _, check := range checks {
				if !check.OK {
					return errors.New("development environment diagnostics failed")
				}
			}
			return nil
		}}
	doctor.Flags().StringVar(&bazel, "bazel", "bazel", "Local Bazel executable")
	var timeout time.Duration
	setup := &cobra.Command{Use: "setup", Short: "Install pinned tools into .cache/dev/tools and sync the Ansible environment", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return errors.New("timeout must be positive")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			runner := ci.Runner{Stdout: cmd.ErrOrStderr(), Stderr: cmd.ErrOrStderr()}
			return dev.Setup(ctx, dev.SetupOptions{State: dev.NewState(root), Runner: runner, Client: &http.Client{Timeout: 2 * time.Minute}, Log: cmd.ErrOrStderr()})
		}}
	setup.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "Installation timeout")
	var all bool
	clean := &cobra.Command{Use: "clean", Short: "Remove local development state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dev.Clean(cmd.Context(), dev.CleanOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: io.Discard}, All: all, Log: cmd.ErrOrStderr()})
		}}
	clean.Flags().BoolVar(&all, "all", false, "Also stop the engine and remove its cache volume")
	var project, out string
	render := &cobra.Command{Use: "render", Short: "Render the platform tree offline with the controller's Flux build and settings substitution", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := dev.Render(cmd.Context(), dev.RenderOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: cmd.ErrOrStderr()}, Project: project, Output: out, Stdout: cmd.OutOrStdout()})
			if err != nil {
				return err
			}
			if out == "-" {
				return json.NewEncoder(cmd.ErrOrStderr()).Encode(report)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
		}}
	render.Flags().StringVar(&project, "project", "", "Render one project under platform/projects")
	render.Flags().StringVar(&out, "out", "", "Output file; - streams YAML to standard output")
	diff := &cobra.Command{Use: "diff", Short: "Server-side dry-run of the local platform tree against the KUBECONFIG cluster", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dev.Diff(cmd.Context(), dev.DiffOptions{State: dev.NewState(root), Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
		}}
	engine := &cobra.Command{Use: "engine", Short: "Bounded local Dagger engines: build (engine role limits) and kata (Kata worker limits)", RunE: missingCommand}
	var profile string
	engine.PersistentFlags().StringVar(&profile, "profile", "build", "Engine profile: build or kata")
	engineOptions := func(cmd *cobra.Command) (dev.EngineOptions, error) {
		selected, ok := dev.EngineProfiles()[profile]
		if !ok {
			return dev.EngineOptions{}, fmt.Errorf("unknown engine profile %q", profile)
		}
		return dev.EngineOptions{State: dev.NewState(root), Runner: ci.Runner{Stderr: io.Discard}, Profile: selected, Log: cmd.ErrOrStderr()}, nil
	}
	engineStart := &cobra.Command{Use: "start", Short: "Start the pinned engine image", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options, err := engineOptions(cmd)
			if err != nil {
				return err
			}
			status, err := dev.StartEngine(cmd.Context(), options)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "export _EXPERIMENTAL_DAGGER_RUNNER_HOST="+status.RunnerHost)
			return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
		}}
	var volumes bool
	engineStop := &cobra.Command{Use: "stop", Short: "Stop the engine", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options, err := engineOptions(cmd)
			if err != nil {
				return err
			}
			return dev.StopEngine(cmd.Context(), options, volumes)
		}}
	engineStop.Flags().BoolVar(&volumes, "volumes", false, "Also remove the engine cache volume")
	engineStatus := &cobra.Command{Use: "status", Short: "Print the engine state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options, err := engineOptions(cmd)
			if err != nil {
				return err
			}
			status, err := dev.InspectEngine(cmd.Context(), options)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
		}}
	engine.AddCommand(engineStart, engineStop, engineStatus)
	qualify := &cobra.Command{Use: "qualify SUITE [-- go test flags]", Short: "Run a gated qualification suite: " + strings.Join(dev.SuiteNames(), ", "), Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner := ci.Runner{Stdout: cmd.ErrOrStderr(), Stderr: cmd.ErrOrStderr()}
			return dev.Qualify(cmd.Context(), dev.QualifyOptions{State: dev.NewState(root), Runner: runner, Suite: args[0], Args: args[1:], Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(), Log: cmd.ErrOrStderr()})
		}}
	cmd.AddCommand(doctor, setup, clean, render, diff, engine, qualify)
	return cmd
}
