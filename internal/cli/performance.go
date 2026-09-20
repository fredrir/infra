package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/spf13/cobra"
)

func newMeasureCommand() *cobra.Command {
	var stage, directory, root string
	var budget time.Duration
	command := &cobra.Command{Use: "measure -- COMMAND [ARGS...]", Short: "Run a command within a measured budget", Args: cobra.MinimumNArgs(1), RunE: func(command *cobra.Command, args []string) error {
		report, err := ci.Measure(command.Context(), ci.Runner{Dir: root, Stdout: command.ErrOrStderr(), Stderr: command.ErrOrStderr()}, stage, budget, directory, args)
		return errors.Join(err, json.NewEncoder(command.OutOrStdout()).Encode(report))
	}}
	command.Flags().StringVar(&stage, "stage", "checks", "Stage name")
	command.Flags().StringVar(&directory, "report-dir", "", "Timing report directory")
	command.Flags().StringVar(&root, "root", ".", "Working directory")
	command.Flags().DurationVar(&budget, "budget", 10*time.Second, "Aggregate execution budget")
	return command
}

func newReadinessCommand() *cobra.Command {
	var address, revision string
	var budget time.Duration
	command := &cobra.Command{Use: "wait-revision", Short: "Wait for the expected served revision", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		if budget <= 0 {
			return errors.New("positive readiness budget required")
		}
		ctx, cancel := context.WithTimeout(command.Context(), budget)
		defer cancel()
		return ci.WaitRevision(ctx, &http.Client{Timeout: time.Second}, address, revision, 500*time.Millisecond)
	}}
	command.Flags().StringVar(&address, "url", "", "Revision endpoint")
	command.Flags().StringVar(&revision, "revision", "", "Expected source revision")
	command.Flags().DurationVar(&budget, "budget", 20*time.Second, "Readiness budget")
	return command
}
