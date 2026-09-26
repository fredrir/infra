package reconciler

import (
	"bytes"
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
	"sync"
	"time"

	"github.com/fredrir/infra/internal/objectstore"
	"github.com/fredrir/infra/internal/platformops"
	"github.com/fredrir/infra/internal/process"
	"github.com/fredrir/infra/internal/reconcile"
)

const (
	runDeadline    = 90 * time.Minute
	finishTimeout  = 2 * time.Minute
	exitRetry      = 75
	logBufferLimit = 32 << 20
)

type Supervisor struct {
	Config      Config
	Credentials string
	Identity    string
	Execute     func(context.Context, process.Options) (process.Result, error)
	HTTP        *http.Client
	Now         func() time.Time
	Log         io.Writer
	LockPoll    time.Duration
}

type runLog struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	stream    io.Writer
	truncated bool
}

func (l *runLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if remaining := logBufferLimit - l.buffer.Len(); remaining < len(data) {
		l.buffer.Write(data[:max(remaining, 0)])
		l.truncated = true
	} else {
		l.buffer.Write(data)
	}
	if l.stream != nil {
		_, _ = l.stream.Write(data)
	}
	return len(data), nil
}

func (l *runLog) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.truncated {
		return append(bytes.Clone(l.buffer.Bytes()), "\n[log truncated]\n"...)
	}
	return bytes.Clone(l.buffer.Bytes())
}

func (s Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s Supervisor) Verify(ctx context.Context) (err error) {
	log := &runLog{stream: s.Log}
	run := Run{Kind: "verify", Started: s.now(), Stage: "credentials", Outcome: reconcile.OutcomeFailed}
	credentials, credentialErr := ConsumeCredentials(s.Credentials, VerifyCredentials)
	defer func() { err = s.finish(ctx, run, credentials, log) }()
	fail := func(stage string, cause error) error {
		run.Stage, run.Error = stage, cause.Error()
		fmt.Fprintf(log, "%s failed: %v\n", stage, cause)
		return cause
	}
	if credentialErr != nil {
		return fail("credentials", credentialErr)
	}
	ctx, cancel := context.WithTimeout(ctx, runDeadline)
	defer cancel()
	run.Stage = "lock"
	release, err := acquireHostLock(ctx, s.Config.Shared, cmp.Or(s.LockPoll, lockPoll))
	if err != nil {
		return fail("lock", err)
	}
	defer release()
	run.Started = s.now()
	run.Stage = "checkout"
	hosts := s.Config.Scope == reconcile.ScopeFull
	current, cleanup, err := openSession(s.Config.State, s.Credentials, s.Execute, log, hosts)
	if err != nil {
		return fail("checkout", err)
	}
	defer func() {
		if removeErr := cleanup(); removeErr != nil {
			fmt.Fprintf(log, "remove run directory: %v\n", removeErr)
		}
	}()
	commands, source, work := current.commands, current.source, current.work
	published, err := commands.checkoutPublished(ctx, source, s.Config.Repository)
	if err != nil {
		return fail("checkout", err)
	}
	run.Revision = published.Revision
	defer func() { s.requestRepair(run, published.Main, log) }()
	if !published.OnMain {
		run.Verification = offMain(published.Revision)
		run.Outcome = run.Verification.Outcome
		fmt.Fprintf(log, "Refusing %s at %s: not on %s\n", publishedBranch, published.Revision, reviewedBranch)
		return nil
	}
	fmt.Fprintf(log, "Verifying %s at %s\n", publishedBranch, run.Revision)
	run.Stage = "build"
	engine := filepath.Join(work, "infra")
	if err := commands.buildEngine(ctx, work, s.Config.Cache, source, engine); err != nil {
		return fail("build", err)
	}
	run.Stage = "tools"
	tools := cloudTools
	if hosts {
		tools = append(slices.Clone(cloudTools), "uv")
	}
	if err := commands.installTools(ctx, engine, work, s.Config.Cache, tools); err != nil {
		return fail("tools", err)
	}
	plugins, err := pluginCache(s.Config.Cache, source)
	if err != nil {
		return fail("tools", err)
	}
	environment := append(current.secretEnvironment(), "TF_PLUGIN_CACHE_DIR="+plugins)
	if hosts {
		if err := current.pythonEnvironment(ctx, s.Config.Cache); err != nil {
			return fail("tools", err)
		}
		access, err := current.hostAccess(s.Identity, s.Config.KnownHosts)
		if err != nil {
			return fail("credentials", err)
		}
		environment = append(append(environment, access...), "INFRA_RECONCILE_TAILNET=true")
	}
	run.Stage = "credentials"
	token, revoke, err := runnerFleetToken(ctx, s.Config.Observer, []byte(credentials[ObserverAppKey]), source, "read")
	if err != nil {
		return fail("credentials", err)
	}
	defer func() {
		if revokeErr := revoke(context.WithoutCancel(ctx)); revokeErr != nil {
			fmt.Fprintf(log, "revoke observer token: %v\n", revokeErr)
		}
	}()
	kubeconfig, err := current.kubeconfig(s.Config.Kubernetes, "infrastructure-verify", credentials[KubernetesToken])
	if err != nil {
		return fail("credentials", err)
	}
	run.Stage = "verify"
	report := filepath.Join(work, "verification.json")
	result, verifyErr := commands.run(ctx, source, append(credentials.engineEnvironment(s.Config.Site, kubeconfig, token), environment...), engine, "reconcile", "verify", "--scope="+string(s.Config.Scope), "--root="+source, "--state-bucket="+s.Config.Bucket, "--state-prefix="+s.Config.Prefix, "--report="+report)
	data, readErr := os.ReadFile(report)
	if readErr != nil {
		return fail("verify", errors.Join(verifyErr, readErr))
	}
	var verification reconcile.Verification
	if err := json.Unmarshal(data, &verification); err != nil {
		return fail("verify", err)
	}
	run.Verification, run.Outcome = &verification, verification.Outcome
	if result.ExitCode == exitRetry {
		run.Outcome = OutcomeSkipped
	}
	return nil
}

func (s Supervisor) requestRepair(run Run, main string, log io.Writer) {
	repairable := run.Outcome == reconcile.OutcomeDiffers && slices.ContainsFunc(run.Verification.Differences, func(difference reconcile.Difference) bool { return difference.System != "rulesets" })
	switch {
	case repairable:
		request := Request{Kind: RequestRepair, Revision: main, Full: true, Reason: truncate(run.failure().Error()), Requested: s.now()}
		if err := WriteRequest(s.Config.Shared, request); err != nil {
			fmt.Fprintf(log, "request repair: %v\n", err)
			return
		}
		fmt.Fprintf(log, "Requested a full reconciliation of %s at %s\n", reviewedBranch, main)
	case run.Outcome == reconcile.OutcomeMatches || run.Outcome == reconcile.OutcomeDiffers:
		if err := os.Remove(requestPath(s.Config.Shared, RequestRepair)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(log, "withdraw repair: %v\n", err)
		}
	}
}

func (s Supervisor) finish(ctx context.Context, run Run, credentials Credentials, log *runLog) error {
	run.Finished = s.now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	fmt.Fprintf(log, "Verification %s at stage %s\n", run.Outcome, run.Stage)
	failure := run.failure()
	var uploadErr, heartbeatErr error
	if credentials[AWSAccessKeyID] == "" || credentials[AWSSecretAccessKey] == "" {
		uploadErr = errors.New("report not uploaded: AWS credentials unavailable")
	} else {
		client := objectstore.Client{Endpoint: cmp.Or(s.Config.Endpoint, "https://s3."+s.Config.Region+".amazonaws.com"), Region: s.Config.Region, AccessKey: credentials[AWSAccessKeyID], SecretKey: credentials[AWSSecretAccessKey], HTTP: s.HTTP}
		uploadErr = Store{Client: client, Bucket: s.Config.Bucket, Prefix: s.Config.Prefix}.upload(ctx, run, log.bytes())
	}
	if run.Outcome != OutcomeSkipped {
		if token := credentials[GatusToken]; token == "" {
			heartbeatErr = errors.New("heartbeat not reported: Gatus token unavailable")
		} else {
			heartbeatErr = platformops.ReportHeartbeat(ctx, s.Config.Gatus, s.Config.Heartbeat, token, errors.Join(failure, uploadErr))
		}
	}
	if s.Log != nil {
		for _, problem := range []error{uploadErr, heartbeatErr} {
			if problem != nil {
				fmt.Fprintln(s.Log, strings.TrimSpace(problem.Error()))
			}
		}
	}
	return errors.Join(failure, uploadErr, heartbeatErr)
}
