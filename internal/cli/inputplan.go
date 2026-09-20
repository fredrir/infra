package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/fredrir/infra/internal/ci"
	"github.com/spf13/cobra"
)

func newAffectedProjectCommand() *cobra.Command {
	var root, config, base, target, output string
	command := &cobra.Command{Use: "affected-project", Short: "Select project targets from changed inputs", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		plan, err := ci.PlanProjectInputs(command.Context(), root, config, base, target)
		if err != nil {
			return err
		}
		if output != "" {
			file, err := os.OpenFile(output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			targets, err := json.Marshal(plan.AffectedTargets)
			if err != nil {
				return errors.Join(err, file.Close())
			}
			_, err = fmt.Fprintf(file, "changed=%t\nreason=%s\naffected_targets=%s\n", plan.Changed, plan.Reason, targets)
			if err := errors.Join(err, file.Close()); err != nil {
				return err
			}
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(plan)
	}}
	command.Flags().StringVar(&root, "root", ".", "Source checkout")
	command.Flags().StringVar(&config, "config", "ci/targets.json", "Input configuration relative to the checkout")
	command.Flags().StringVar(&base, "base", "", "Git revision to compare against")
	command.Flags().StringVar(&target, "target", "", "Project target name")
	command.Flags().StringVar(&output, "github-output", os.Getenv("GITHUB_OUTPUT"), "GitHub Actions output file")
	return command
}
