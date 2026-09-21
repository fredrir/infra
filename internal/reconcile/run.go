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
	Plan(context.Context, Plan) error
	Expand(context.Context, Plan) error
	Hosts(context.Context) error
	ExpansionUnchanged(context.Context) (bool, error)
	Publish(context.Context, string) error
	Kubernetes(context.Context, string) error
	Monitor(context.Context, Plan) error
	Verify(context.Context, Plan) error
	Retire(context.Context, Plan) error
}

type Reconciler struct {
	Store  Store
	Ops    Operations
	Host   string
	Report func(Status) error
}

func (r Reconciler) Apply(ctx context.Context, full bool) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Minute)
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
	previousDesired := status.Desired
	if !full && reusableHosts(status, time.Now()) {
		changed, err := r.Ops.Select(ctx, status.Desired, false)
		if err != nil {
			return err
		}
		reuseHosts = !changed.Tofu && !changed.Ansible
	}
	selected, err := r.Ops.Select(ctx, status.Applied, full || status.Desired != status.Applied)
	if err != nil {
		return err
	}
	plan := Plan{Revision: revision, Base: status.Applied, Affected: selected, Host: r.Host}
	status.Desired, status.Failure, status.HostsReusedFrom = revision, "", ""
	status.Durations = map[string]float64{}
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
	if err = stage("plan", func() error { return r.Ops.Plan(ctx, plan) }); err != nil {
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
		}
	} else {
		reuseHosts = false
	}
	if selected.Ansible && !selected.MonitorOnly && !reuseHosts {
		if err = stage("hosts", func() error { return r.Ops.Hosts(ctx) }); err != nil {
			return err
		}
	}
	if err = stage("publish", func() error { return r.Ops.Publish(ctx, revision) }); err != nil {
		return err
	}
	if err = stage("kubernetes", func() error { return r.Ops.Kubernetes(ctx, revision) }); err != nil {
		return err
	}
	if selected.Ansible {
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
	previous := status.Applied
	status.Applied, status.Stage = revision, "complete"
	if err = save(ctx); err != nil {
		status.Applied = previous
		return err
	}
	return nil
}
