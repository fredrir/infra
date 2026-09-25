package reconcile

import (
	"context"
	"errors"
	"fmt"
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
}

const applyDeadline = 90 * time.Minute

type Reconciler struct {
	Store  Store
	Ops    Operations
	Host   string
	Report func(Status) error
}

func (r Reconciler) Apply(ctx context.Context, full bool) (err error) {
	ctx, cancel := context.WithTimeout(ctx, applyDeadline)
	defer cancel()
	unlock, err := r.Store.Lock(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
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
	status.Failure, status.HostsReusedFrom, status.HostScope = "", "", ""
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
		if err != nil {
			status.Failure = err.Error()
			failureContext, failureCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer failureCancel()
			err = errors.Join(err, save(failureContext))
		}
	}()
	stage := func(name string, action func() error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		status.Stage = name
		if err := save(ctx); err != nil {
			return err
		}
		started := time.Now()
		err := action()
		status.Durations[name] = time.Since(started).Seconds()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
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
	if selected.Ansible && !selected.MonitorOnly && !reuseHosts {
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
	if selected.Ansible && effectiveHostScope(selected) != HostScopeRunners {
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
