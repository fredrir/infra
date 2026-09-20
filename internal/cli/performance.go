package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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

func newTimelineCommand() *cobra.Command {
	var deploymentID uint64
	var budget time.Duration
	command := &cobra.Command{Use: "timeline REPOSITORY RUN_ID", Short: "Read workflow and revision-readiness timing", Args: cobra.ExactArgs(2), RunE: func(command *cobra.Command, args []string) error {
		id, err := strconv.ParseUint(args[1], 10, 64)
		if err != nil || budget <= 0 {
			return errors.New("positive run ID and budget required")
		}
		ctx, cancel := context.WithTimeout(command.Context(), 30*time.Second)
		defer cancel()
		runner := ci.Runner{Stderr: command.ErrOrStderr()}
		build, err := ci.ReadWorkflowTimeline(ctx, runner, args[0], id)
		if err != nil {
			return err
		}
		var deployment ci.WorkflowTimeline
		if deploymentID != 0 {
			deployment, err = ci.ReadWorkflowTimeline(ctx, runner, "fredrir/infra", deploymentID)
			if err != nil {
				return err
			}
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(ci.JoinDeploymentTimeline(build, deployment, budget))
	}}
	command.Flags().Uint64Var(&deploymentID, "deployment-run", 0, "Associated infra deployment run")
	command.Flags().DurationVar(&budget, "budget", time.Minute, "Workflow-to-readiness target")
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
