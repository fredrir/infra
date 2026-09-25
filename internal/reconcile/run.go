package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

type Plan struct {
	Revision string    `json:"revision"`
	Base     string    `json:"base_revision"`
	Affected Selection `json:"affected"`
	Host     string    `json:"grafana_host"`
}

type Operations interface {
	Revision(context.Context) (string, error)
	Select(context.Context, string, bool) (Selection, error)
	Provenance(context.Context, ProvenanceRange) error
	Preflight(context.Context, Plan) (Plan, error)
	Plan(context.Context, Plan) error
	Expand(context.Context, Plan) error
	Hosts(context.Context, Plan) error
	ExpansionUnchanged(context.Context) (bool, error)
	Publish(context.Context, string) error
	Kubernetes(context.Context, Plan) error
	Monitor(context.Context, Plan) error
	Verify(context.Context, Plan) error
	VerifyDrift(context.Context, Plan) error
	Retire(context.Context, Plan) error
	Volatile(context.Context, Plan) error
}

const (
	applyDeadline = 90 * time.Minute
	lockPoll      = 15 * time.Second
)

type Reconciler struct {
	Store          Store
	Ops            Operations
	Host           string
	Report         func(Status) error
	LockWait       time.Duration
	Log            io.Writer
	ProvenanceBase string
}

func (r Reconciler) Apply(ctx context.Context, full bool) (err error) {
	held, unlock, err := r.lock(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	ctx, cancel := context.WithTimeout(held, applyDeadline)
	defer cancel()
	status, err := r.Store.Read(ctx)
	if err != nil {
		return err
	}
	revision, err := r.Ops.Revision(ctx)
	if err != nil {
		return err
	}
	reuseHosts := false
	recovery := status.NeedsRecovery()
	previousDesired := status.Desired
	if !full && reusableHosts(status, time.Now()) {
		changed, err := r.Ops.Select(ctx, status.Desired, false)
		if err != nil {
			return err
		}
		reuseHosts = !changed.Tofu && !changed.Ansible
	}
	selected, err := r.Ops.Select(ctx, status.Applied, full || recovery)
	if err != nil {
		return err
	}
	if full || status.Applied == "" || recovery {
		selected = All()
	}
	plan := Plan{Revision: revision, Base: status.Applied, Affected: selected, Host: r.Host}
	skip := !full && !recovery && status.Applied != "" && !selected.Tofu && !selected.Ansible && !selected.Kubernetes
	status.Evaluated, status.Selection = revision, selected
	status.Failure, status.HostsReusedFrom, status.HostScope, status.Provenance, status.VolatileFailure = "", "", "", nil, ""
	status.Durations = map[string]float64{}
	if !skip {
		status.Desired = revision
	}
	save := func(ctx context.Context) error {
		status.Updated = time.Now().UTC()
		if err := r.Store.Write(ctx, status); err != nil {
			return err
		}
		if r.Report != nil {
			return r.Report(status)
		}
		return nil
	}
	defer func() {
		if err != nil && !errors.Is(context.Cause(held), errLeaseLost) {
			status.Failure = err.Error()
			failureContext, failureCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer failureCancel()
			err = errors.Join(err, save(failureContext))
		}
	}()
	stage := func(name string, action func() error) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		status.Stage = name
		if err := save(ctx); err != nil {
			return err
		}
		started := time.Now()
		err := action()
		status.Durations[name] = time.Since(started).Seconds()
		if err != nil && ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
	if err = stage("provenance", func() error {
		checked, err := NewProvenanceRange(status.Applied, r.ProvenanceBase, revision)
		if err != nil {
			return err
		}
		status.Provenance = &checked
		return r.Ops.Provenance(ctx, checked)
	}); err != nil {
		return err
	}
	if skip {
		if selected.Tooling {
			deployed := Plan{Revision: status.Applied, Base: status.Applied, Affected: All(), Host: r.Host}
			if err = stage("drift-verification", func() error { return r.Ops.VerifyDrift(ctx, deployed) }); err != nil {
				return err
			}
		}
		status.Stage = "evaluated"
		return save(ctx)
	}
	if err = stage("plan", func() error {
		var err error
		plan, err = r.Ops.Preflight(ctx, plan)
		if err != nil {
			return err
		}
		selected, status.Selection = plan.Affected, plan.Affected
		return r.Ops.Plan(ctx, plan)
	}); err != nil {
		return err
	}
	if selected.Tofu {
		if err = stage("expand", func() error { return r.Ops.Expand(ctx, plan) }); err != nil {
			return err
		}
	}
	if reuseHosts && selected.Tofu && selected.Ansible && !selected.MonitorOnly {
		unchanged, proofErr := r.Ops.ExpansionUnchanged(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		reuseHosts = proofErr == nil && unchanged
		if reuseHosts {
			status.HostsReusedFrom = previousDesired
			status.HostScope = HostScopeFull
		}
	} else {
		reuseHosts = false
	}
	convergedHosts := selected.Ansible && !selected.MonitorOnly && !reuseHosts
	if convergedHosts {
		name := "hosts"
		if effectiveHostScope(selected) == HostScopeRunners {
			name = "hosts-runners"
		}
		if err = stage(name, func() error { return r.Ops.Hosts(ctx, plan) }); err != nil {
			return err
		}
	}
	if selected.Ansible && !selected.MonitorOnly {
		status.HostScope = effectiveHostScope(selected)
	}
	if err = stage("publish", func() error { return r.Ops.Publish(ctx, revision) }); err != nil {
		return err
	}
	if err = stage("kubernetes", func() error { return r.Ops.Kubernetes(ctx, plan) }); err != nil {
		return err
	}
	if selected.Ansible && (effectiveHostScope(selected) != HostScopeRunners || monitorCLIChanged(selected)) {
		if err = stage("monitor", func() error { return r.Ops.Monitor(ctx, plan) }); err != nil {
			return err
		}
	}
	if err = stage("verify", func() error { return r.Ops.Verify(ctx, plan) }); err != nil {
		return err
	}
	if selected.Tofu {
		if err = stage("retire", func() error { return r.Ops.Retire(ctx, plan) }); err != nil {
			return err
		}
	}
	if convergedHosts && effectiveHostScope(selected) == HostScopeFull {
		status.Stage = "volatile"
		if err = save(ctx); err != nil {
			return err
		}
		started := time.Now()
		if failure := r.Ops.Volatile(ctx, plan); failure != nil {
			if err = ctx.Err(); err != nil {
				return err
			}
			status.VolatileFailure = failure.Error()
		}
		status.Durations["volatile"] = time.Since(started).Seconds()
	}
	previous, previousFull, previousFullTime := status.Applied, status.LastFullRevision, status.LastFullVerified
	if selected.Tofu && selected.Kubernetes && effectiveHostScope(selected) == HostScopeFull && len(selected.Projects) == 0 {
		status.LastFullRevision, status.LastFullVerified = revision, time.Now().UTC()
	}
	status.Applied, status.Stage = revision, "complete"
	if err = save(ctx); err != nil {
		status.Applied, status.LastFullRevision, status.LastFullVerified = previous, previousFull, previousFullTime
		return err
	}
	return nil
}

func (r Reconciler) lock(ctx context.Context) (context.Context, func() error, error) {
	deadline := time.Now().Add(r.LockWait)
	var holder string
	for {
		held, unlock, err := r.Store.Lock(ctx)
		var locked ErrLocked
		if !errors.As(err, &locked) || !time.Now().Before(deadline) {
			return held, unlock, err
		}
		if r.Log != nil && locked.Owner != holder {
			fmt.Fprintf(r.Log, "Waiting until %s: %s\n", deadline.Format(time.RFC3339), locked)
		}
		holder = locked.Owner
		wait := min(lockPoll, time.Until(deadline))
		if expiry := time.Until(locked.Expires); expiry > 0 {
			wait = min(wait, expiry)
		}
		select {
		case <-ctx.Done():
			return nil, nil, context.Cause(ctx)
		case <-time.After(wait):
		}
	}
}

func Retryable(err error) bool {
	switch err := err.(type) {
	case nil:
		return false
	case ErrLocked:
		return true
	case interface{ Unwrap() []error }:
		return !slices.ContainsFunc(err.Unwrap(), func(inner error) bool { return !Retryable(inner) })
	case interface{ Unwrap() error }:
		return Retryable(err.Unwrap())
	default:
		return err == ErrSuperseded
	}
}
