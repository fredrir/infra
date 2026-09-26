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
	Execute     func(context.Context, process.Options) (process.Result, error)
	HTTP        *http.Client
	Now         func() time.Time
	Log         io.Writer
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
	run.Stage = "checkout"
	state := s.Config.State
	if err := os.MkdirAll(state, 0o700); err != nil {
		return fail("checkout", err)
	}
	if err := clearDirectory(state); err != nil {
		return fail("checkout", err)
	}
	work, err := os.MkdirTemp(state, "run-")
	if err != nil {
		return fail("checkout", err)
	}
	defer func() {
		if removeErr := removeTree(work); removeErr != nil {
			fmt.Fprintf(log, "remove run directory: %v\n", removeErr)
		}
	}()
	home := filepath.Join(work, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return fail("checkout", err)
	}
	environment := append([]string{"PATH=" + filepath.Join(work, "tools") + ":/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "LANG=C.UTF-8", "TF_IN_AUTOMATION=true"}, hardenedGit...)
	commands := executor{execute: s.Execute, env: environment, log: log}
	source := filepath.Join(work, "source")
	if run.Revision, err = commands.checkoutPublished(ctx, source, s.Config.Repository); err != nil {
		return fail("checkout", err)
	}
	fmt.Fprintf(log, "Verifying %s at %s\n", publishedBranch, run.Revision)
	run.Stage = "build"
	engine := filepath.Join(work, "infra")
	if err := commands.buildEngine(ctx, work, source, engine); err != nil {
		return fail("build", err)
	}
	run.Stage = "tools"
	if err := commands.installTools(ctx, engine, work); err != nil {
		return fail("tools", err)
	}
	run.Stage = "credentials"
	token, revoke, err := observerToken(ctx, s.Config.Observer, []byte(credentials[ObserverAppKey]), source)
	if err != nil {
		return fail("credentials", err)
	}
	defer func() {
		if revokeErr := revoke(context.WithoutCancel(ctx)); revokeErr != nil {
			fmt.Fprintf(log, "revoke observer token: %v\n", revokeErr)
		}
	}()
	authority, err := os.ReadFile(s.Config.Kubernetes.CertificateAuthority)
	if err != nil {
		return fail("credentials", err)
	}
	config, err := Kubeconfig(s.Config.Kubernetes.Server, authority, credentials[KubernetesToken])
	if err != nil {
		return fail("credentials", err)
	}
	kubeconfig := filepath.Join(work, "kubeconfig")
	if err := os.WriteFile(kubeconfig, config, 0o600); err != nil {
		return fail("credentials", err)
	}
	run.Stage = "verify"
	report := filepath.Join(work, "verification.json")
	result, verifyErr := commands.run(ctx, source, credentials.engineEnvironment(s.Config, kubeconfig, token), engine, "reconcile", "verify", "--scope=cloud", "--root="+source, "--state-bucket="+s.Config.Bucket, "--state-prefix="+s.Config.Prefix, "--report="+report)
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
