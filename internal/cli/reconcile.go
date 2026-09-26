package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/objectstore"
	"github.com/fredrir/infra/internal/platformops"
	"github.com/fredrir/infra/internal/reconcile"
	"github.com/fredrir/infra/internal/reconciler"
	"github.com/spf13/cobra"
)

func newReconcileCommand() *cobra.Command {
	var root, bucket, prefix, base, report, provenanceBase, scope string
	var full bool
	var wait time.Duration
	command := &cobra.Command{Use: "reconcile", Short: "Plan, apply, and verify managed infrastructure", RunE: missingCommand}
	command.AddCommand(newRequestVerificationCommand(), newRunCommand())
	for _, action := range []string{"plan", "apply", "verify", "status", "requirements", "provenance"} {
		child := &cobra.Command{Use: action, Args: cobra.NoArgs}
		child.Flags().StringVar(&root, "root", ".", "Source checkout")
		child.Flags().StringVar(&bucket, "state-bucket", "llunde-pyparser-bucket", "Reconciliation state bucket")
		child.Flags().StringVar(&prefix, "state-prefix", "reconciliation/production", "Reconciliation state prefix")
		child.Flags().StringVar(&report, "report", "", "Status, verification or provenance outcome report path")
		if action != "provenance" {
			child.Flags().BoolVar(&full, "full", false, "Reconcile all systems, including external drift")
		}
		if action == "plan" {
			child.Flags().StringVar(&base, "base", "", "Comparison revision")
		}
		if action == "apply" {
			child.Flags().DurationVar(&wait, "wait", 0, "Wait up to this long for another reconciliation to release its lease")
		}
		if action == "apply" || action == "provenance" {
			child.Flags().StringVar(&provenanceBase, "provenance-base", "", "Verify commit provenance from this revision instead of the applied one")
		}
		if action == "verify" {
			child.Flags().StringVar(&scope, "scope", "", "cloud: without host access; full: also host plays in check mode")
			child.MarkFlagRequired("scope")
		}
		child.RunE = func(cmd *cobra.Command, _ []string) (err error) {
			runnerToken, publisherKeyFile := os.Getenv("GH_TOKEN"), os.Getenv("PUBLISHER_APP_PRIVATE_KEY_FILE")
			if err := errors.Join(os.Unsetenv("GH_TOKEN"), os.Unsetenv("PUBLISHER_APP_PRIVATE_KEY_FILE")); err != nil {
				return err
			}
			var publisherKey []byte
			if publisherKeyFile != "" {
				if publisherKey, err = reconcile.ConsumePrivateKey(publisherKeyFile); err != nil {
					return fmt.Errorf("PUBLISHER_APP_PRIVATE_KEY_FILE: %w", err)
				}
			}
			if action == "apply" && publisherKey == nil {
				return errors.New("PUBLISHER_APP_PRIVATE_KEY_FILE required")
			}
			var verified string
			var compared error
			var verification reconcile.Scope
			if action == "verify" {
				if verification, err = reconcile.ParseScope(scope); err != nil {
					return err
				}
				if report != "" {
					defer func() {
						err = errors.Join(err, writeReport(report, reconcile.VerificationOutcome(verified, verification, errors.Join(compared, err))))
					}()
				}
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
				store.Client = &objectstore.Client{Endpoint: cmp.Or(os.Getenv("AWS_ENDPOINT_URL_S3"), "https://s3."+region+".amazonaws.com"), Region: region, AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN"), HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
			}
			if action == "status" {
				status, err := store.Read(cmd.Context())
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
			}
			token, attestations, removeCredentials, err := githubCredentials()
			if err != nil {
				return err
			}
			defer removeCredentials()
			client, err := reconcile.GitHubClient(reconcile.GitHubAPI, token)
			if err != nil {
				return err
			}
			pullRequests := reconcile.GitHubPullRequests{Client: client, Owner: "fredrir", Name: "infra"}
			if action == "provenance" {
				return verifyProvenance(cmd, store, &reconcile.Commands{Runner: runner, ProvenanceEnv: attestations, PullRequests: pullRequests}, provenanceBase, report)
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
			ops := &reconcile.Commands{Runner: runner, Work: work, RequireMain: action == "apply", ProvenanceEnv: attestations, GitHub: client, PullRequests: pullRequests, RunnerToken: runnerToken}
			if action == "apply" {
				publisher, err := reconcile.ReadPublisher(absolute)
				if err != nil {
					return err
				}
				publisher.PrivateKey = publisherKey
				ops.Publisher = &publisher
				engine := reconcile.Reconciler{Store: store, Ops: ops, Host: host, LockWait: wait, Log: cmd.ErrOrStderr(), ProvenanceBase: provenanceBase, Report: func(status reconcile.Status) error {
					fmt.Fprintf(cmd.OutOrStdout(), "%s desired=%s applied=%s\n", status.Stage, status.Desired, status.Applied)
					if report == "" {
						return nil
					}
					return writeReport(report, status)
				}}
				return engine.Apply(cmd.Context(), full)
			}
			if action == "verify" {
				verified, compared = reconcile.Verifier{Store: store, Ops: ops, Host: host, Scope: verification}.Verify(cmd.Context())
				return reconcile.WithoutDegraded(compared)
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

func githubCredentials() (string, []string, func(), error) {
	token := os.Getenv("PROVENANCE_TOKEN")
	if err := os.Unsetenv("PROVENANCE_TOKEN"); err != nil || token == "" {
		return "", nil, func() {}, err
	}
	directory, err := ci.RegistryConfig(cmp.Or(os.Getenv("GITHUB_ACTOR"), "provenance"), token)
	if err != nil {
		return "", nil, nil, err
	}
	return token, []string{"GH_TOKEN=" + token, "DOCKER_CONFIG=" + directory}, func() { os.RemoveAll(directory) }, nil
}

func verifyProvenance(cmd *cobra.Command, store reconcile.S3Store, ops *reconcile.Commands, override, report string) (err error) {
	var outcome struct {
		reconcile.ProvenanceRange
		Error string `json:"error,omitempty"`
	}
	defer func() {
		if err != nil {
			outcome.Error = err.Error()
		}
		err = errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(outcome))
		if report != "" {
			err = errors.Join(err, writeReport(report, outcome))
		}
	}()
	status, err := store.Read(cmd.Context())
	if err != nil {
		return err
	}
	revision, err := ops.Revision(cmd.Context())
	if err != nil {
		return err
	}
	if outcome.ProvenanceRange, err = reconcile.NewProvenanceRange(status.Applied, override, revision); err != nil {
		return err
	}
	return ops.Provenance(cmd.Context(), outcome.ProvenanceRange)
}

func newRequestVerificationCommand() *cobra.Command {
	request := reconcile.VerificationRequest{API: "https://api.github.com", Poll: 30 * time.Second, Deadline: 150 * time.Minute}
	var key, token string
	command := &cobra.Command{Use: "request-verification", Short: "Dispatch deep verification with drift repair and report its conclusion to Gatus", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var err error
		if request.PrivateKey, err = os.ReadFile(key); err != nil {
			return err
		}
		heartbeat, err := os.ReadFile(token)
		if err != nil {
			return err
		}
		request.HeartbeatToken, request.Log = strings.TrimSpace(string(heartbeat)), cmd.OutOrStdout()
		return reconcile.RequestVerification(cmd.Context(), request)
	}}
	command.Flags().Int64Var(&request.AppID, "app-id", 0, "GitHub App ID")
	command.Flags().Int64Var(&request.InstallationID, "installation-id", 0, "GitHub App installation ID")
	command.Flags().StringVar(&key, "private-key", "", "GitHub App private key file")
	command.Flags().StringVar(&token, "heartbeat-token", "", "Gatus external endpoint token file")
	command.Flags().StringVar(&request.Gatus, "gatus", platformops.GatusURL, "Gatus URL")
	command.Flags().StringVar(&request.Heartbeat, "heartbeat", "reconciliation_verification", "Gatus external endpoint key as GROUP_NAME")
	command.Flags().StringVar(&request.Repository, "repository", "fredrir/infra", "Repository as OWNER/NAME")
	command.Flags().StringVar(&request.Workflow, "workflow", "reconcile.yml", "Workflow file name")
	command.Flags().StringVar(&request.Ref, "ref", "main", "Workflow revision")
	for _, name := range []string{"private-key", "heartbeat-token"} {
		_ = command.MarkFlagRequired(name)
	}
	return command
}

func newRunCommand() *cobra.Command {
	var config, credentials string
	run := &cobra.Command{Use: "run", Short: "Run a reconciler host unit at the current main revision", RunE: missingCommand}
	verify := &cobra.Command{Use: "verify", Short: "Build the engine at main, verify its cloud scope, and report to S3 and Gatus", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		loaded, err := reconciler.LoadConfig(config)
		if err != nil {
			return err
		}
		return reconciler.Supervisor{Config: loaded, Credentials: credentials, Log: cmd.OutOrStdout()}.Verify(cmd.Context())
	}}
	verify.Flags().StringVar(&config, "config", "/etc/infra-reconcile/verify.json", "Reconciler configuration")
	verify.Flags().StringVar(&credentials, "credentials", "", "Decrypted verify credentials as a JSON object; removed once read")
	_ = verify.MarkFlagRequired("credentials")
	run.AddCommand(verify)
	return run
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
