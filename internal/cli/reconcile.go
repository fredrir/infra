package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/objectstore"
	"github.com/fredrir/infra/internal/reconcile"
	"github.com/spf13/cobra"
)

func newReconcileCommand() *cobra.Command {
	var root, bucket, prefix, base, report string
	var full, deep, scopeHosts, scopeProjects, verifyArtifacts bool
	command := &cobra.Command{Use: "reconcile", Short: "Plan, apply, and verify managed infrastructure", RunE: missingCommand}
	command.PersistentFlags().StringVar(&root, "root", ".", "Source checkout")
	command.PersistentFlags().StringVar(&bucket, "state-bucket", "llunde-pyparser-bucket", "Reconciliation state bucket")
	command.PersistentFlags().StringVar(&prefix, "state-prefix", "reconciliation/production", "Reconciliation state prefix")
	command.PersistentFlags().StringVar(&report, "report", "", "Status or verification outcome report path")
	command.PersistentFlags().BoolVar(&full, "full", false, "Reconcile all systems, including external drift")
	command.PersistentFlags().BoolVar(&scopeHosts, "scope-hosts", false, "Limit host convergence to affected playbooks")
	command.PersistentFlags().BoolVar(&scopeProjects, "scope-projects", false, "Limit project deployment to affected owners")
	command.PersistentFlags().BoolVar(&verifyArtifacts, "verify-artifacts", false, "Verify artifact provenance and workload readiness")
	for _, action := range []string{"plan", "apply", "verify", "status", "requirements"} {
		child := &cobra.Command{Use: action, Args: cobra.NoArgs}
		if action == "plan" {
			child.Flags().StringVar(&base, "base", "", "Comparison revision")
		}
		if action == "verify" {
			child.Flags().BoolVar(&deep, "deep", false, "Also compare OpenTofu and every host play with production in check mode")
		}
		child.RunE = func(cmd *cobra.Command, _ []string) (err error) {
			var verified string
			if action == "verify" && report != "" {
				defer func() {
					err = errors.Join(err, writeReport(report, reconcile.VerificationOutcome(verified, deep, err)))
				}()
			}
			absolute, err := filepath.Abs(root)
			if err != nil {
				return err
			}
			runner := ci.Runner{Dir: absolute, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}
			store := reconcile.S3Store{Runner: runner, Bucket: bucket, Prefix: prefix}
			region := os.Getenv("AWS_REGION")
			if region == "" {
				region = os.Getenv("AWS_DEFAULT_REGION")
			}
			if region != "" && os.Getenv("AWS_ACCESS_KEY_ID") != "" && os.Getenv("AWS_SECRET_ACCESS_KEY") != "" {
				store.Client = &objectstore.Client{Endpoint: "https://s3." + region + ".amazonaws.com", Region: region, AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN"), HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
			}
			if action == "status" {
				status, err := store.Read(cmd.Context())
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
			}
			if action == "requirements" {
				status, err := store.Read(cmd.Context())
				if err != nil {
					return err
				}
				recovery := status.Desired != status.Applied || status.Failure != "" || (status.Stage != "" && status.Stage != "complete" && status.Stage != "evaluated")
				ops := &reconcile.Commands{Runner: runner}
				selected, err := ops.Select(cmd.Context(), status.Applied, full || recovery)
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(selected)
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
			ops := &reconcile.Commands{Runner: runner, Work: work, RequireMain: action == "apply", ScopeHosts: scopeHosts, ScopeProjects: scopeProjects, VerifyArtifacts: verifyArtifacts}
			if action == "apply" {
				engine := reconcile.Reconciler{Store: store, Ops: ops, Host: host, SkipUnchanged: scopeHosts, VerifyArtifacts: verifyArtifacts, Report: func(status reconcile.Status) error {
					fmt.Fprintf(cmd.OutOrStdout(), "%s desired=%s applied=%s\n", status.Stage, status.Desired, status.Applied)
					if report == "" {
						return nil
					}
					return writeReport(report, status)
				}}
				return engine.Apply(cmd.Context(), full)
			}
			revision, err := ops.Revision(cmd.Context())
			if err != nil {
				return err
			}
			if action == "verify" {
				published, err := ops.PublishedRevision(cmd.Context())
				if err != nil {
					return err
				}
				if published != revision {
					pending, err := ops.Select(cmd.Context(), published, false)
					if err != nil {
						return err
					}
					if pending.Tofu || pending.Kubernetes || pending.Ansible {
						return fmt.Errorf("checkout contains unpublished infrastructure changes")
					}
					revision = published
				}
			}
			selected, err := ops.Select(cmd.Context(), base, full)
			if err != nil {
				return err
			}
			plan := reconcile.Plan{Revision: revision, Base: base, Affected: selected, Host: host}
			if action == "verify" {
				verified = revision
				if deep {
					return ops.VerifyDeep(cmd.Context(), plan)
				}
				return ops.VerifyLive(cmd.Context(), plan)
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

func writeReport(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
