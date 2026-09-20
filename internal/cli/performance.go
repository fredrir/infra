package cli

import (
	"encoding/json"
	"errors"
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
