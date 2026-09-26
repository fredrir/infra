package reconciler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fredrir/infra/internal/objectstore"
	"github.com/fredrir/infra/internal/platformops"
	"github.com/fredrir/infra/internal/process"
	"github.com/fredrir/infra/internal/reconcile"
)

const (
	applyDeadline    = 2 * time.Hour
	leaseWait        = "11m"
	tokenExpiryAlert = 30 * 24 * time.Hour
)

var (
	applyTools = []string{"flux", "gh", "kubectl", "tofu", "helm", "actionlint", "uv", "cosign"}
	gateTools  = []string{"gh", "cosign"}
)

type Applier struct {
	Config      ApplyConfig
	Credentials string
	Identity    string
	Self        string
	Execute     func(context.Context, process.Options) (process.Result, error)
	HTTP        *http.Client
	Now         func() time.Time
	Log         io.Writer
	LockPoll    time.Duration
}

type applyRun struct {
	Run
	check int64
}

func (a Applier) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a Applier) remote() executor {
	return executor{execute: a.Execute, env: append([]string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}, hardenedGit...)}
}

func (a Applier) Pending(ctx context.Context) (Decision, error) {
	ledger, err := loadLedger(a.Config.State)
	if err != nil {
		return Decision{}, err
	}
	requests, requestErr := readRequests(a.Config.Shared, false)
	tip, tipErr := a.remote().remoteMain(ctx, a.Config.Repository)
	return decide(ledger, tip, requests, a.now()), errors.Join(requestErr, tipErr)
}

func (a Applier) Apply(ctx context.Context) error {
	log := &runLog{stream: a.Log}
	credentials, credentialErr := ConsumeCredentials(a.Credentials, ApplyCredentials)
	ctx, cancel := context.WithTimeout(ctx, applyDeadline)
	defer cancel()
	release, err := acquireHostLock(ctx, a.Config.Shared, cmp.Or(a.LockPoll, lockPoll))
	if err != nil {
		return errors.Join(credentialErr, err)
	}
	defer release()
	ledger, err := loadLedger(a.Config.State)
	if err != nil {
		return errors.Join(credentialErr, err)
	}
	requests, requestErr := readRequests(a.Config.Shared, true)
	tip, tipErr := a.remote().remoteMain(ctx, a.Config.Repository)
	for _, problem := range []error{requestErr, tipErr} {
		if problem != nil {
			fmt.Fprintln(log, problem)
		}
	}
	decision := decide(ledger, tip, requests, a.now())
	for _, kind := range decision.Consumed {
		if err := removeRequest(a.Config.Shared, kind); err != nil {
			return errors.Join(credentialErr, err)
		}
	}
	if !decision.Run {
		fmt.Fprintln(log, "Nothing to reconcile")
		return credentialErr
	}
	fmt.Fprintf(log, "Reconciling: %s\n", decision.Reason)
	if !decision.Apply {
		ledger.Checked = a.now()
		failure := errors.Join(ledger.failure(), credentialErr, a.readiness(ctx, credentials))
		return errors.Join(failure, saveLedger(a.Config.State, ledger), a.heartbeat(ctx, credentials, failure))
	}
	run := &applyRun{Run: Run{Kind: "apply", Revision: tip, Started: a.now(), Stage: "credentials", Outcome: reconcile.OutcomeFailed, Reason: decision.Reason, Full: decision.Full}}
	var ignored bool
	if credentialErr != nil {
		run.Error = credentialErr.Error()
	} else {
		ignored = a.reconcile(ctx, credentials, decision, ledger, run, log)
	}
	if ignored {
		ledger.Revision, ledger.Checked = run.Revision, a.now()
		fmt.Fprintf(log, "Only push-ignored paths changed up to %s\n", run.Revision)
		return saveLedger(a.Config.State, ledger)
	}
	return a.finish(ctx, credentials, decision, ledger, run, log)
}

func (a Applier) reconcile(ctx context.Context, credentials Credentials, decision Decision, ledger Ledger, run *applyRun, log *runLog) bool {
	fail := func(stage string, cause error) bool {
		run.Stage, run.Error = stage, cause.Error()
		fmt.Fprintf(log, "%s failed: %v\n", stage, cause)
		return false
	}
	run.Stage = "checkout"
	current, cleanup, err := openSession(filepath.Join(a.Config.State, "runs"), a.Execute, log, true)
	if err != nil {
		return fail("checkout", err)
	}
	defer func() {
		if removeErr := cleanup(); removeErr != nil {
			fmt.Fprintf(log, "remove run directory: %v\n", removeErr)
		}
	}()
	commands, source, work := current.commands, current.source, current.work
	revision, err := commands.checkoutMain(ctx, source, a.Config.Repository)
	if err != nil {
		return fail("checkout", err)
	}
	run.Revision = revision
	if decision.Advanced && !decision.Full && ledger.settled() && ledger.Revision != "" {
		if ignored, err := commands.onlyIgnored(ctx, source, ledger.Revision, revision); err != nil {
			fmt.Fprintf(log, "compare with %s: %v\n", ledger.Revision, err)
		} else if ignored {
			return true
		}
	}
	fmt.Fprintf(log, "Reconciling %s at %s\n", reviewedBranch, revision)
	publisher := checks{publisher: a.Config.Publisher, key: []byte(credentials[PublisherAppKey])}
	if run.check, err = publisher.start(ctx, revision, run.ID(), run.Started); err != nil {
		fmt.Fprintf(log, "check run: %v\n", err)
	}
	run.Stage = "provenance"
	base, err := a.gate(ctx, current, credentials, run)
	if err != nil {
		return fail("provenance", err)
	}
	run.Stage = "build"
	engine := filepath.Join(work, "infra")
	if err := commands.buildEngine(ctx, work, a.Config.Cache, source, engine); err != nil {
		return fail("build", err)
	}
	run.Stage = "tools"
	if err := commands.installTools(ctx, engine, work, a.Config.Cache, applyTools); err != nil {
		return fail("tools", err)
	}
	if err := current.pythonEnvironment(ctx, a.Config.Cache); err != nil {
		return fail("tools", err)
	}
	plugins, err := pluginCache(a.Config.Cache, source)
	if err != nil {
		return fail("tools", err)
	}
	run.Stage = "validate"
	for _, step := range []string{"prepare-validation", "validate"} {
		if _, err := commands.run(ctx, source, []string{"TF_PLUGIN_CACHE_DIR=" + plugins}, engine, "ci", step, "--before="+base); err != nil {
			return fail("validate", fmt.Errorf("ci %s: %w", step, err))
		}
	}
	run.Stage = "credentials"
	token, revoke, err := runnerFleetToken(ctx, a.Config.Runner, []byte(credentials[RunnerAppKey]), source, "write")
	if err != nil {
		return fail("credentials", err)
	}
	defer func() {
		if revokeErr := revoke(context.WithoutCancel(ctx)); revokeErr != nil {
			fmt.Fprintf(log, "revoke runner token: %v\n", revokeErr)
		}
	}()
	kubeconfig, err := current.kubeconfig(a.Config.Kubernetes, "infrastructure-apply", credentials[KubernetesToken])
	if err != nil {
		return fail("credentials", err)
	}
	if err := current.hostAccess(a.Identity, a.Config.KnownHosts); err != nil {
		return fail("credentials", err)
	}
	key := filepath.Join(work, "publisher.pem")
	if err := os.WriteFile(key, []byte(credentials[PublisherAppKey]+"\n"), 0o600); err != nil {
		return fail("credentials", err)
	}
	defer os.Remove(key)
	run.Stage = "apply"
	status := filepath.Join(work, "status.json")
	arguments := []string{"reconcile", "apply", "--root=" + source, "--state-bucket=" + a.Config.Bucket, "--state-prefix=" + a.Config.Prefix, "--wait=" + leaseWait, "--report=" + status}
	if decision.Full {
		arguments = append(arguments, "--full")
	}
	environment := append(credentials.engineEnvironment(a.Config.Site, kubeconfig, token), "PUBLISHER_APP_PRIVATE_KEY_FILE="+key, "PROVENANCE_TOKEN="+credentials[ProvenanceToken], "INFRA_RECONCILE_TAILNET=true", "TF_PLUGIN_CACHE_DIR="+plugins)
	result, applyErr := commands.run(ctx, source, environment, engine, arguments...)
	if data, err := os.ReadFile(status); err == nil && json.Valid(data) {
		run.Status = data
	}
	switch {
	case applyErr == nil:
		run.Outcome = OutcomeApplied
		if run.status().Stage == "evaluated" {
			run.Outcome = OutcomeEvaluated
		}
	case result.ExitCode == exitRetry:
		run.Outcome = OutcomeDeferred
		if tip, err := a.remote().remoteMain(ctx, a.Config.Repository); err == nil && tip != revision {
			run.Outcome = OutcomeSuperseded
		}
	default:
		fail("apply", cmp.Or(errorText(run.status().Failure), applyErr))
	}
	return false
}

func (a Applier) gate(ctx context.Context, current session, credentials Credentials, run *applyRun) (string, error) {
	self := a.Self
	if self == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		self = executable
	}
	tools := filepath.Join(current.work, "gate", "tools")
	gate := executor{execute: a.Execute, env: append([]string{"PATH=" + tools + ":/usr/local/bin:/usr/bin:/bin", "HOME=" + current.home, "LANG=C.UTF-8"}, hardenedGit...), log: current.commands.log}
	if _, err := gate.run(ctx, current.work, []string{"INFRA_TOOL_CACHE=" + tools, "INFRA_TOOL_DOWNLOADS=" + filepath.Join(a.Config.Cache, "tools")}, self, append([]string{"ci", "install-tools", "--temporary", current.work}, gateTools...)...); err != nil {
		return "", fmt.Errorf("install gate tools: %w", err)
	}
	report := filepath.Join(current.work, "provenance.json")
	_, gateErr := gate.run(ctx, current.source, append(credentials.stateEnvironment(a.Config.Site), "PROVENANCE_TOKEN="+credentials[ProvenanceToken]), self, "reconcile", "provenance", "--root="+current.source, "--state-bucket="+a.Config.Bucket, "--state-prefix="+a.Config.Prefix, "--report="+report)
	data, readErr := os.ReadFile(report)
	var checked struct {
		reconcile.ProvenanceRange
		Error string `json:"error"`
	}
	if readErr == nil {
		readErr = json.Unmarshal(data, &checked)
		run.Provenance = data
	}
	switch {
	case gateErr != nil || readErr != nil:
		return "", cmp.Or(errorText(checked.Error), errors.Join(gateErr, readErr))
	case checked.Revision != run.Revision || !revisionPattern.MatchString(checked.Base):
		return "", fmt.Errorf("provenance report covers %s..%s, not %s", checked.Base, checked.Revision, run.Revision)
	}
	return checked.Base, nil
}

func (a Applier) readiness(ctx context.Context, credentials Credentials) error {
	client := objectstore.Client{Endpoint: cmp.Or(a.Config.Endpoint, "https://s3."+a.Config.Region+".amazonaws.com"), Region: a.Config.Region, AccessKey: credentials[AWSAccessKeyID], SecretKey: credentials[AWSSecretAccessKey], HTTP: a.HTTP}
	var problems []error
	if _, err := (reconcile.S3Store{Client: &client, Bucket: a.Config.Bucket, Prefix: a.Config.Prefix}).Read(ctx); err != nil {
		problems = append(problems, fmt.Errorf("state bucket: %w", err))
	}
	if err := a.provenanceTokenLifetime(ctx, credentials[ProvenanceToken]); err != nil {
		problems = append(problems, fmt.Errorf("provenance token: %w", err))
	}
	return errors.Join(problems...)
}

func (a Applier) provenanceTokenLifetime(ctx context.Context, token string) error {
	client, err := reconcile.GitHubClient(a.Config.Publisher.API, token)
	if err != nil {
		return err
	}
	_, response, err := client.RateLimit.Get(ctx)
	if err != nil {
		return err
	}
	expires := response.TokenExpiration.Time
	switch {
	case expires.IsZero():
		return errors.New("has no expiry")
	case expires.Sub(a.now()) < tokenExpiryAlert:
		return fmt.Errorf("expires %s", expires.UTC().Format(time.DateOnly))
	}
	return nil
}

func (a Applier) finish(ctx context.Context, credentials Credentials, decision Decision, ledger Ledger, run *applyRun, log *runLog) error {
	run.Finished = a.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	readiness := a.readiness(ctx, credentials)
	fmt.Fprintf(log, "Reconciliation %s at stage %s\n", run.Outcome, run.Stage)
	ledger.Revision, ledger.Outcome, ledger.Failure, ledger.Finished, ledger.Checked = run.Revision, run.Outcome, run.Error, run.Finished, run.Finished
	if decision.Repair {
		ledger.Repair = &Attempt{Revision: run.Revision, Started: run.Started, Outcome: run.Outcome}
	}
	client := objectstore.Client{Endpoint: cmp.Or(a.Config.Endpoint, "https://s3."+a.Config.Region+".amazonaws.com"), Region: a.Config.Region, AccessKey: credentials[AWSAccessKeyID], SecretKey: credentials[AWSSecretAccessKey], HTTP: a.HTTP}
	uploadErr := Store{Client: client, Bucket: a.Config.Bucket, Prefix: a.Config.Prefix}.upload(ctx, run.Run, log.bytes())
	if run.check != 0 {
		title, conclusion := run.conclusion()
		if err := (checks{publisher: a.Config.Publisher, key: []byte(credentials[PublisherAppKey])}).complete(ctx, run.check, conclusion, title, run.summary(a.Config.Site), run.Finished); err != nil {
			fmt.Fprintf(log, "check run: %v\n", err)
		}
	}
	var heartbeatErr error
	if slices.Contains([]string{OutcomeApplied, OutcomeEvaluated, reconcile.OutcomeFailed}, run.Outcome) || readiness != nil {
		heartbeatErr = a.heartbeat(ctx, credentials, errors.Join(ledger.failure(), readiness, uploadErr))
	}
	for _, problem := range []error{readiness, uploadErr, heartbeatErr} {
		if problem != nil && a.Log != nil {
			fmt.Fprintln(a.Log, strings.TrimSpace(problem.Error()))
		}
	}
	return errors.Join(ledger.failure(), saveLedger(a.Config.State, ledger), uploadErr, heartbeatErr)
}

func (a Applier) heartbeat(ctx context.Context, credentials Credentials, failure error) error {
	token := credentials[GatusToken]
	if token == "" {
		return errors.New("heartbeat not reported: Gatus token unavailable")
	}
	if failure != nil {
		failure = errors.New(truncate(failure.Error()))
	}
	return platformops.ReportHeartbeat(ctx, a.Config.Gatus, a.Config.Heartbeat, token, failure)
}

func (l Ledger) failure() error {
	if l.Outcome != reconcile.OutcomeFailed {
		return nil
	}
	return fmt.Errorf("reconciliation of %s failed: %s", l.Revision, l.Failure)
}

func (r applyRun) status() reconcile.Status {
	var status reconcile.Status
	_ = json.Unmarshal(r.Status, &status)
	return status
}

func (r applyRun) conclusion() (string, string) {
	revision := r.Revision
	if len(revision) > 12 {
		revision = revision[:12]
	}
	switch r.Outcome {
	case OutcomeApplied:
		return "Applied " + revision, "success"
	case OutcomeEvaluated:
		return "Evaluated " + revision + "; nothing to apply", "success"
	case OutcomeSuperseded:
		return "Superseded by a newer main", "skipped"
	case OutcomeDeferred:
		return "Deferred: another reconciliation holds the lease", "skipped"
	}
	return "Failed at " + r.Stage, "failure"
}

func (r applyRun) summary(site Site) string {
	var summary strings.Builder
	fmt.Fprintf(&summary, "| Name | Value |\n| --- | --- |\n| Revision | `%s` |\n| Reason | %s |\n| Full | %t |\n| Outcome | %s |\n| Stage | %s |\n| Report | `s3://%s/%s/runs/%s/` |\n", r.Revision, r.Reason, r.Full, r.Outcome, r.Stage, site.Bucket, site.Prefix, r.ID())
	if r.Error != "" {
		fmt.Fprintf(&summary, "\n```text\n%s\n```\n", truncate(r.Error))
	}
	for _, report := range [][]byte{r.Provenance, r.Status} {
		if len(report) > 0 {
			fmt.Fprintf(&summary, "\n```json\n%s\n```\n", strings.TrimSpace(string(report)))
		}
	}
	return summary.String()
}

func errorText(text string) error {
	if text == "" {
		return nil
	}
	return errors.New(text)
}

func (e executor) onlyIgnored(ctx context.Context, source, base, revision string) (bool, error) {
	if contained, err := e.ancestor(ctx, source, base, revision); err != nil || !contained {
		return false, err
	}
	changed, err := e.output(ctx, "", "git", "-C", source, "diff", "--name-only", "--no-renames", "-z", base, revision)
	if err != nil {
		return false, err
	}
	for path := range strings.SplitSeq(changed, "\x00") {
		if path != "" && !reconcile.PushIgnored(path) {
			return false, nil
		}
	}
	return true, nil
}
