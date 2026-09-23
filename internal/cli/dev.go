package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	clean := &cobra.Command{Use: "clean", Short: "Remove local development state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dev.Clean(cmd.Context(), dev.CleanOptions{State: dev.NewState(root), Log: cmd.ErrOrStderr()})
		}}
	cmd.AddCommand(doctor, setup, clean)
	return cmd
}
