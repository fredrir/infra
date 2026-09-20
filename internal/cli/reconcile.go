package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/reconcile"
	"github.com/spf13/cobra"
)

func newReconcileCommand() *cobra.Command {
	var root, bucket, prefix, base, report string
	var full bool
	command := &cobra.Command{Use: "reconcile", Short: "Plan, apply, and verify managed infrastructure", RunE: missingCommand}
	command.PersistentFlags().StringVar(&root, "root", ".", "Source checkout")
	command.PersistentFlags().StringVar(&bucket, "state-bucket", "llunde-pyparser-bucket", "Reconciliation state bucket")
	command.PersistentFlags().StringVar(&prefix, "state-prefix", "reconciliation/production", "Reconciliation state prefix")
	command.PersistentFlags().StringVar(&report, "report", "", "Status report path")
	command.PersistentFlags().BoolVar(&full, "full", false, "Reconcile all systems, including external drift")
	for _, action := range []string{"plan", "apply", "verify", "status"} {
		child := &cobra.Command{Use: action, Args: cobra.NoArgs}
		if action == "plan" {
			child.Flags().StringVar(&base, "base", "", "Comparison revision")
		}
		child.RunE = func(cmd *cobra.Command, _ []string) error {
			absolute, err := filepath.Abs(root)
			if err != nil {
				return err
			}
			runner := ci.Runner{Dir: absolute, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}
			store := reconcile.S3Store{Runner: runner, Bucket: bucket, Prefix: prefix}
			if action == "status" {
				status, err := store.Read(cmd.Context())
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
			}
			host, err := reconcile.Host(absolute)
			if err != nil {
				return err
			}
			work, err := os.MkdirTemp("", "infra-reconcile-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(work)
			ops := &reconcile.Commands{Runner: runner, Work: work, RequireMain: action == "apply"}
			if action == "apply" {
				engine := reconcile.Reconciler{Store: store, Ops: ops, Host: host, Report: func(status reconcile.Status) error {
					data, err := json.MarshalIndent(status, "", "  ")
					if err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s desired=%s applied=%s\n", status.Stage, status.Desired, status.Applied)
					if report == "" {
						return nil
					}
					if err := os.MkdirAll(filepath.Dir(report), 0700); err != nil {
						return err
					}
					return os.WriteFile(report, append(data, '\n'), 0600)
				}}
				return engine.Apply(cmd.Context(), full)
			}
			revision, err := ops.Revision(cmd.Context())
			if err != nil {
				return err
			}
			selected, err := ops.Select(cmd.Context(), base, full)
			if err != nil {
				return err
			}
			plan := reconcile.Plan{Revision: revision, Base: base, Affected: selected, Host: host}
			if action == "verify" {
				return ops.Verify(cmd.Context(), plan)
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(plan); err != nil {
				return err
			}
			return ops.Plan(cmd.Context(), plan)
		}
		command.AddCommand(child)
	}
	return command
}
