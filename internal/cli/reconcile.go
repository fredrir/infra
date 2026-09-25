package cli

import (
	"cmp"
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
	var full, deep bool
	command := &cobra.Command{Use: "reconcile", Short: "Plan, apply, and verify managed infrastructure", RunE: missingCommand}
	command.AddCommand(newRequestVerificationCommand())
	for _, action := range []string{"plan", "apply", "verify", "status", "requirements"} {
		child := &cobra.Command{Use: action, Args: cobra.NoArgs}
		child.Flags().StringVar(&root, "root", ".", "Source checkout")
		child.Flags().StringVar(&bucket, "state-bucket", "llunde-pyparser-bucket", "Reconciliation state bucket")
		child.Flags().StringVar(&prefix, "state-prefix", "reconciliation/production", "Reconciliation state prefix")
		child.Flags().StringVar(&report, "report", "", "Status or verification outcome report path")
		child.Flags().BoolVar(&full, "full", false, "Reconcile all systems, including external drift")
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
				ops := &reconcile.Commands{Runner: runner}
				selected, err := ops.Select(cmd.Context(), status.Applied, full || status.NeedsRecovery())
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
			ops := &reconcile.Commands{Runner: runner, Work: work, RequireMain: action == "apply"}
			if action == "apply" {
				engine := reconcile.Reconciler{Store: store, Ops: ops, Host: host, Report: func(status reconcile.Status) error {
					fmt.Fprintf(cmd.OutOrStdout(), "%s desired=%s applied=%s\n", status.Stage, status.Desired, status.Applied)
					if report == "" {
						return nil
					}
					return writeReport(report, status)
				}}
				return engine.Apply(cmd.Context(), full)
			}
			if action == "verify" {
				verified, err = reconcile.Verifier{Store: store, Ops: ops, Host: host, Deep: deep}.Verify(cmd.Context())
				return err
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
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(plan); err != nil {
				return err
			}
			return ops.Plan(cmd.Context(), plan)
		}
		command.AddCommand(child)
	}
	return command
}

func newRequestVerificationCommand() *cobra.Command {
	request := reconcile.VerificationRequest{API: "https://api.github.com"}
	var key string
	command := &cobra.Command{Use: "request-verification", Short: "Dispatch deep verification with drift repair as a GitHub App", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if key == "" {
			return errors.New("--private-key is required outside a systemd credential directory")
		}
		var err error
		if request.PrivateKey, err = os.ReadFile(key); err != nil {
			return err
		}
		run, err := reconcile.RequestVerification(cmd.Context(), request)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Requested verification:", cmp.Or(run, request.Repository+" "+request.Workflow+"@"+request.Ref))
		return err
	}}
	var credentials string
	if directory := os.Getenv("CREDENTIALS_DIRECTORY"); directory != "" {
		credentials = filepath.Join(directory, "github-app-key")
	}
	command.Flags().Int64Var(&request.AppID, "app-id", 0, "GitHub App ID")
	command.Flags().Int64Var(&request.InstallationID, "installation-id", 0, "GitHub App installation ID")
	command.Flags().StringVar(&key, "private-key", credentials, "GitHub App private key file")
	command.Flags().StringVar(&request.Repository, "repository", "fredrir/infra", "Repository as OWNER/NAME")
	command.Flags().StringVar(&request.Workflow, "workflow", "reconcile.yml", "Workflow file name")
	command.Flags().StringVar(&request.Ref, "ref", "main", "Workflow revision")
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
