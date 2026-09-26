package reconciler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	applyDeadline     = 2 * time.Hour
	leaseWait         = "11m"
	tokenExpiryAlert  = 30 * 24 * time.Hour
	gateOutageRetries = 3
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

type stageFailure struct {
	stage    string
	err      error
	terminal bool
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
	requests, invalid := readRequests(a.Config.Shared, a.now())
	tip, err := a.remote().remoteMain(ctx, a.Config.Repository)
	return decide(ledger, tip, requests, invalid, a.now()), err
}

func (a Applier) Apply(ctx context.Context) error {
	log := &runLog{stream: a.Log}
	credentials, credentialErr := ConsumeCredentials(a.Credentials, ApplyCredentials)
	release, err := acquireHostLock(ctx, a.Config.Shared, cmp.Or(a.LockPoll, lockPoll))
	if err != nil {
		return errors.Join(credentialErr, err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, applyDeadline)
	defer cancel()
	ledger, err := loadLedger(a.Config.State)
	if err != nil {
		return errors.Join(credentialErr, err)
	}
	requests, invalid := readRequests(a.Config.Shared, a.now())
	tip, tipErr := a.remote().remoteMain(ctx, a.Config.Repository)
	if tipErr != nil {
		fmt.Fprintln(log, tipErr)
	}
	decision := decide(ledger, tip, requests, invalid, a.now())
	if !decision.Run {
		return credentialErr
	}
	var quarantined error
	if len(decision.Quarantine) > 0 {
		ledger = ledger.quarantine(decision.Quarantine)
		for _, kind := range slices.Sorted(maps.Keys(decision.Quarantine)) {
			quarantined = errors.Join(quarantined, fmt.Errorf("quarantined %s: %s", decision.Quarantine[kind].Fingerprint[:12], decision.Quarantine[kind].Reason))
		}
		if a.Log != nil {
			fmt.Fprintln(a.Log, quarantined)
		}
		if err := saveLedger(a.Config.State, ledger); err != nil {
			return errors.Join(credentialErr, quarantined, err)
		}
		if err := a.heartbeat(ctx, credentials, quarantined); err != nil && a.Log != nil {
			fmt.Fprintln(a.Log, err)
		}
	}
	fmt.Fprintf(log, "Reconciling: %s\n", decision.Reason)
	if !decision.Apply {
		if quarantined != nil && a.now().Sub(ledger.Checked) < readinessInterval {
			return errors.Join(credentialErr, quarantined)
		}
		ledger.Checked, ledger.Consumed = a.now(), decision.Consumed
		failure := errors.Join(ledger.failure(), credentialErr, a.readiness(ctx, credentials))
		return errors.Join(quarantined, failure, saveLedger(a.Config.State, ledger), a.heartbeat(ctx, credentials, failure))
	}
	started := ledger.begin(decision, tip, a.now())
	if err := saveLedger(a.Config.State, started); err != nil {
		return errors.Join(credentialErr, err)
	}
	run := &applyRun{Run: Run{Kind: "apply", Revision: tip, Started: started.Started, Stage: "credentials", Outcome: reconcile.OutcomeFailed, Reason: decision.Reason, Full: decision.Full}}
	var failure *stageFailure
	ignored := false
	if credentialErr != nil {
		failure = &stageFailure{stage: "credentials", err: credentialErr}
	} else {
		ignored, failure = a.reconcile(ctx, credentials, decision, ledger, started.Attempts, run, log)
	}
	if ignored {
		ledger.Revision, ledger.Checked, ledger.Consumed = run.Revision, a.now(), decision.Consumed
		fmt.Fprintf(log, "Only push-ignored paths changed up to %s\n", run.Revision)
		return errors.Join(quarantined, saveLedger(a.Config.State, ledger))
	}
	if failure != nil {
		run.Stage, run.Error, run.Outcome = failure.stage, failure.err.Error(), OutcomeRetry
		if failure.terminal {
			run.Outcome = reconcile.OutcomeFailed
		}
		fmt.Fprintf(log, "%s failed: %v\n", failure.stage, failure.err)
	}
	return errors.Join(quarantined, a.finish(ctx, credentials, started, run, log))
}

func (a Applier) reconcile(ctx context.Context, credentials Credentials, decision Decision, ledger Ledger, attempt int, run *applyRun, log *runLog) (bool, *stageFailure) {
	transient := func(stage string, err error) (bool, *stageFailure) {
		return false, &stageFailure{stage: stage, err: err}
	}
	terminal := func(stage string, err error) (bool, *stageFailure) {
		return false, &stageFailure{stage: stage, err: err, terminal: true}
	}
	current, cleanup, err := openSession(filepath.Join(a.Config.State, "runs"), a.Credentials, a.Execute, log, true)
	if err != nil {
		return transient("checkout", err)
	}
	defer func() {
		if removeErr := cleanup(); removeErr != nil {
			fmt.Fprintf(log, "remove run directory: %v\n", removeErr)
		}
	}()
	commands, source, work := current.commands, current.source, current.work
	revision, err := commands.checkoutMain(ctx, source, a.Config.Repository)
	if err != nil {
		return transient("checkout", err)
	}
	run.Revision = revision
	if decision.Advanced && !decision.Full && ledger.settled() && ledger.Revision != "" {
		if ignored, err := commands.onlyIgnored(ctx, source, ledger.Revision, revision); err != nil {
			fmt.Fprintf(log, "compare with %s: %v\n", ledger.Revision, err)
		} else if ignored {
			return true, nil
		}
	}
	fmt.Fprintf(log, "Reconciling %s at %s\n", reviewedBranch, revision)
	publisher := checks{publisher: a.Config.Publisher, key: []byte(credentials[PublisherAppKey])}
	if run.check, err = publisher.start(ctx, revision, run.ID(), run.Started); err != nil {
		fmt.Fprintf(log, "check run: %v\n", err)
	}
	base, err := a.gate(ctx, current, credentials, run)
	if err != nil {
		var rejected rejection
		switch {
		case !errors.As(err, &rejected):
			return transient("provenance", err)
		case rejected.unavailable && attempt <= gateOutageRetries:
			return transient("provenance", err)
		}
		return terminal("provenance", err)
	}
	engine := filepath.Join(work, "infra")
	if err := commands.buildEngine(ctx, work, a.Config.Cache, source, engine); err != nil {
		return transient("build", err)
	}
	if err := commands.installTools(ctx, engine, work, a.Config.Cache, applyTools); err != nil {
		return transient("tools", err)
	}
	if err := current.pythonEnvironment(ctx, a.Config.Cache); err != nil {
		return transient("tools", err)
	}
	plugins, err := pluginCache(a.Config.Cache, source)
	if err != nil {
		return transient("tools", err)
	}
	if _, err := commands.run(ctx, source, []string{"TF_PLUGIN_CACHE_DIR=" + plugins}, engine, "ci", "prepare-validation", "--before="+base); err != nil {
		return transient("validate", fmt.Errorf("ci prepare-validation: %w", err))
	}
	if result, err := commands.run(ctx, source, []string{"TF_PLUGIN_CACHE_DIR=" + plugins}, engine, "ci", "validate", "--before="+base); err != nil {
		if result.ExitCode > 0 {
			return terminal("validate", fmt.Errorf("ci validate: %w", err))
		}
		return transient("validate", fmt.Errorf("ci validate: %w", err))
	}
	hosts, err := a.hostsSelected(ctx, commands, engine, source, credentials, decision.Full)
	if err != nil {
		return transient("requirements", err)
	}
	environment, err := current.hostAccess(a.Identity, a.Config.KnownHosts)
	if err != nil {
		return transient("credentials", err)
	}
	var token string
	if hosts {
		minted, revoke, err := runnerFleetToken(ctx, a.Config.Runner, []byte(credentials[RunnerAppKey]), source, "write")
		if err != nil {
			return transient("credentials", err)
		}
		defer func() {
			if revokeErr := revoke(context.WithoutCancel(ctx)); revokeErr != nil {
				fmt.Fprintf(log, "revoke runner token: %v\n", revokeErr)
			}
		}()
		token = minted
	}
	kubeconfig, err := current.kubeconfig(a.Config.Kubernetes, "infrastructure-apply", credentials[KubernetesToken])
	if err != nil {
		return transient("credentials", err)
	}
	key := filepath.Join(current.secrets, "publisher.pem")
	if err := os.WriteFile(key, []byte(credentials[PublisherAppKey]+"\n"), 0o600); err != nil {
		return transient("credentials", err)
	}
	status := filepath.Join(work, "status.json")
	arguments := []string{"reconcile", "apply", "--root=" + source, "--state-bucket=" + a.Config.Bucket, "--state-prefix=" + a.Config.Prefix, "--wait=" + leaseWait, "--report=" + status}
	if decision.Full {
		arguments = append(arguments, "--full")
	}
	environment = append(append(append(credentials.engineEnvironment(a.Config.Site, kubeconfig, token), environment...), current.secretEnvironment()...), "PUBLISHER_APP_PRIVATE_KEY_FILE="+key, "PROVENANCE_TOKEN="+credentials[ProvenanceToken], "INFRA_RECONCILE_TAILNET=true", "TF_PLUGIN_CACHE_DIR="+plugins)
	result, applyErr := commands.run(ctx, source, environment, engine, arguments...)
	if data, err := os.ReadFile(status); err == nil && json.Valid(data) {
		run.Status = data
	}
	switch {
	case applyErr == nil:
		run.Stage, run.Outcome = "apply", OutcomeApplied
		if run.status().Stage == "evaluated" {
			run.Outcome = OutcomeEvaluated
		}
	case result.ExitCode == exitRetry:
		run.Stage, run.Outcome = "apply", OutcomeDeferred
		if tip, err := a.remote().remoteMain(ctx, a.Config.Repository); err == nil && tip != revision {
			run.Outcome = OutcomeSuperseded
		}
	case result.ExitCode > 0:
		return terminal("apply", cmp.Or(errorText(run.status().Failure), applyErr))
	default:
		return transient("apply", applyErr)
	}
	return false, nil
}

func (a Applier) hostsSelected(ctx context.Context, commands executor, engine, source string, credentials Credentials, full bool) (bool, error) {
	quiet := commands
	quiet.log = nil
	arguments := []string{"reconcile", "requirements", "--root=" + source, "--state-bucket=" + a.Config.Bucket, "--state-prefix=" + a.Config.Prefix}
	if full {
		arguments = append(arguments, "--full")
	}
	result, err := quiet.run(ctx, source, credentials.stateEnvironment(a.Config.Site), engine, arguments...)
	if err != nil {
		return false, fmt.Errorf("reconcile requirements: %w: %s", err, strings.TrimSpace(string(result.Stderr)))
	}
	var selection reconcile.Selection
	if err := json.Unmarshal(result.Stdout, &selection); err != nil {
		return false, fmt.Errorf("reconcile requirements: %w", err)
	}
	return selection.Ansible || selection.Tooling, nil
}

type rejection struct {
	error
	unavailable bool
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
	if _, err := gate.run(ctx, current.work, []string{"INFRA_TOOL_CACHE=" + tools, "INFRA_TOOL_DOWNLOADS=" + filepath.Join(a.Config.Cache, "gate-tools")}, self, append([]string{"ci", "install-tools", "--temporary", current.work}, gateTools...)...); err != nil {
		return "", fmt.Errorf("install gate tools: %w", err)
	}
	report := filepath.Join(current.work, "provenance.json")
	environment := append(append(credentials.stateEnvironment(a.Config.Site), current.secretEnvironment()...), "PROVENANCE_TOKEN="+credentials[ProvenanceToken])
	_, gateErr := gate.run(ctx, current.source, environment, self, "reconcile", "provenance", "--root="+current.source, "--state-bucket="+a.Config.Bucket, "--state-prefix="+a.Config.Prefix, "--report="+report)
	data, readErr := os.ReadFile(report)
	var checked reconcile.ProvenanceOutcome
	if readErr == nil {
		readErr = json.Unmarshal(data, &checked)
		run.Provenance = data
	}
	switch {
	case readErr != nil:
		return "", errors.Join(gateErr, readErr)
	case checked.Revision == "" && checked.Base == "":
		return "", cmp.Or(errorText(checked.Error), gateErr, errors.New("provenance report names no range"))
	case checked.Revision != run.Revision:
		return "", rejection{error: fmt.Errorf("provenance report covers %s..%s, not %s", checked.Base, checked.Revision, run.Revision)}
	case !revisionPattern.MatchString(checked.Base):
		return "", rejection{error: fmt.Errorf("provenance report has an invalid base %q", checked.Base)}
	case gateErr != nil || checked.Error != "":
		return "", rejection{error: cmp.Or(errorText(checked.Error), gateErr), unavailable: checked.Unavailable}
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

func (a Applier) finish(ctx context.Context, credentials Credentials, started Ledger, run *applyRun, log *runLog) error {
	run.Finished = a.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	readiness := a.readiness(ctx, credentials)
	fmt.Fprintf(log, "Reconciliation %s at stage %s\n", run.Outcome, run.Stage)
	ledger := started.end(*run, run.Finished)
	client := objectstore.Client{Endpoint: cmp.Or(a.Config.Endpoint, "https://s3."+a.Config.Region+".amazonaws.com"), Region: a.Config.Region, AccessKey: credentials[AWSAccessKeyID], SecretKey: credentials[AWSSecretAccessKey], HTTP: a.HTTP}
	uploadErr := Store{Client: client, Bucket: a.Config.Bucket, Prefix: a.Config.Prefix}.upload(ctx, run.Run, log.bytes())
	if run.check != 0 {
		title, conclusion := run.conclusion()
		if err := (checks{publisher: a.Config.Publisher, key: []byte(credentials[PublisherAppKey])}).complete(ctx, run.check, conclusion, title, run.summary(a.Config.Site), run.Finished); err != nil {
			fmt.Fprintf(log, "check run: %v\n", err)
		}
	}
	var heartbeatErr error
	if slices.Contains([]string{OutcomeApplied, OutcomeEvaluated, reconcile.OutcomeFailed, OutcomeRetry}, run.Outcome) || readiness != nil {
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
	case OutcomeRetry:
		return "Retrying after " + r.Stage + " failed", "neutral"
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
