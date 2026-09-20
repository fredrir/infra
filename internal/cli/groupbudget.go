package cli

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/spf13/cobra"
)

func newCheckBudgetCommand() *cobra.Command {
	var directory string
	var budget time.Duration
	command := &cobra.Command{Use: "check-budget", Short: "Validate the aggregate recorded check budget", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		report, err := ci.CheckGroupBudget(directory, budget, true)
		return errors.Join(err, json.NewEncoder(command.OutOrStdout()).Encode(report))
	}}
	command.Flags().StringVar(&directory, "report-dir", "", "Directory containing only check receipts")
	command.Flags().DurationVar(&budget, "budget", 10*time.Second, "Maximum aggregate check duration")
	return command
}

func newMeasureCheckCommand() *cobra.Command {
	var directory, stage, root string
	var budget time.Duration
	command := &cobra.Command{Use: "measure-check -- COMMAND [ARGS...]", Short: "Run a check within the remaining aggregate budget", Args: cobra.MinimumNArgs(1), RunE: func(command *cobra.Command, args []string) error {
		report, err := ci.MeasureCheck(command.Context(), ci.Runner{Dir: root, Stdout: command.ErrOrStderr(), Stderr: command.ErrOrStderr()}, stage, budget, directory, args)
		return errors.Join(err, json.NewEncoder(command.OutOrStdout()).Encode(report))
	}}
	command.Flags().StringVar(&directory, "report-dir", "", "Directory containing only check receipts")
	command.Flags().StringVar(&stage, "stage", "checks", "Unique check stage name")
	command.Flags().StringVar(&root, "root", ".", "Working directory")
	command.Flags().DurationVar(&budget, "budget", 10*time.Second, "Maximum aggregate check duration")
	return command
}
