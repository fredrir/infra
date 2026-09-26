package reconcile

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func TestVolatileConvergenceNeverFailsTheFleet(t *testing.T) {
	for _, test := range []struct {
		name      string
		selection Selection
		failure   error
		runs      int
	}{
		{name: "converged", selection: All(), runs: 1},
		{name: "failed", selection: All(), failure: errors.New("volatile.yml unreachable hosts: fredrir-10"), runs: 1},
		{name: "runners only", selection: Selection{Ansible: true, HostScope: HostScopeRunners}},
		{name: "monitor only", selection: Selection{Ansible: true, HostScope: HostScopeMonitor, MonitorOnly: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryStore{status: Status{Desired: "old", Applied: "old", VolatileFailure: "stale"}}
			ops := &fakeOps{selection: test.selection, volatileFailure: test.failure}
			if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); err != nil {
				t.Fatalf("fleet failed with volatile outcome %v: %v", test.failure, err)
			}
			want := ""
			if test.failure != nil {
				want = test.failure.Error()
			}
			if ops.volatile != test.runs || store.status.VolatileFailure != want || store.status.Stage != "complete" || store.status.Applied != "new" {
				t.Fatalf("volatile ran %d times and recorded %+v", ops.volatile, store.status)
			}
			if store.status.NeedsRecovery() {
				t.Fatal("a volatile outcome requests fleet recovery")
			}
		})
	}
}

type observedVolatile struct {
	fakeOps
	store    *memoryStore
	reported []Status
	observed []Status
	released []bool
	during   func()
}

func (o *observedVolatile) Volatile(ctx context.Context, plan Plan) error {
	o.observed, o.released = append(o.observed, o.store.status), append(o.released, o.store.released)
	if o.during != nil {
		o.during()
	}
	return o.fakeOps.Volatile(ctx, plan)
}

func TestVolatileConvergesAfterTheFleetIsRecordedComplete(t *testing.T) {
	for _, failure := range []error{nil, errors.New("volatile.yml unreachable hosts: fredrir-10")} {
		store := &memoryStore{status: Status{Desired: "old", Applied: "old", Stage: "complete"}}
		ops := &observedVolatile{fakeOps: fakeOps{selection: All(), volatileFailure: failure}, store: store}
		reconciler := Reconciler{Store: store, Ops: ops, Report: func(status Status) error {
			ops.reported = append(ops.reported, status)
			return nil
		}}
		if err := reconciler.Apply(context.Background(), false); err != nil {
			t.Fatalf("volatile outcome %v failed the apply: %v", failure, err)
		}
		if len(ops.observed) != 1 || ops.observed[0].Stage != "complete" || ops.observed[0].Applied != "new" || ops.released[0] {
			t.Fatalf("volatile converged before the fleet was durably complete under the lease: %+v, released %v", ops.observed, ops.released)
		}
		if last := ops.reported[len(ops.reported)-2]; last.Stage != "complete" || last.Applied != "new" || last.VolatileFailure != "" {
			t.Fatalf("completion not published before volatile convergence: %+v", last)
		}
		want := ""
		if failure != nil {
			want = failure.Error()
		}
		if store.status.Stage != "complete" || store.status.VolatileFailure != want || store.status.Durations["volatile"] <= 0 || store.status.NeedsRecovery() || !store.released {
			t.Fatalf("volatile outcome recorded as %+v, released %v", store.status, store.released)
		}
	}
}

type cancelableStore struct {
	memoryStore
	cancel context.CancelCauseFunc
}

func (s *cancelableStore) Lock(ctx context.Context) (context.Context, func() error, error) {
	held, unlock, err := s.memoryStore.Lock(ctx)
	if err != nil {
		return nil, nil, err
	}
	held, s.cancel = context.WithCancelCause(held)
	return held, unlock, nil
}

func TestLostLeaseDuringVolatileConvergenceKeepsTheCompletedFleet(t *testing.T) {
	store := &cancelableStore{memoryStore: memoryStore{status: Status{Desired: "old", Applied: "old", Stage: "complete"}}}
	ops := &observedVolatile{fakeOps: fakeOps{selection: All()}, store: &store.memoryStore}
	ops.during = func() {
		store.cancel(errLeaseLost)
		ops.volatileFailure = errors.New("volatile.yml: context canceled")
	}
	if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); !errors.Is(err, errLeaseLost) {
		t.Fatalf("lost lease during volatile convergence returned %v", err)
	}
	if store.status.Stage != "complete" || store.status.Applied != "new" || store.status.Failure != "" || store.status.VolatileFailure != "" || store.status.NeedsRecovery() {
		t.Fatalf("status written without the lease or fleet recovery requested: %+v", store.status)
	}
}

func TestVolatileOutcomesAreDegradedNotFailed(t *testing.T) {
	degraded := Degraded{errors.Join(Differences{{System: "volatile", Host: "fredrir-10", Item: "Mount the data volume"}}, errors.New("volatile.yml comparison incomplete: unreachable hosts: fredrir-10"))}
	verification := VerificationOutcome("r", ScopeFull, errors.Join(errors.New("flux-system is not ready"), degraded))
	if verification.Outcome != OutcomeFailed || !slices.Equal(verification.Errors, []string{"flux-system is not ready"}) || len(verification.Differences) != 0 {
		t.Fatalf("degraded outcome leaked into the fleet: %+v", verification)
	}
	if want := []string{"production differs from its declaration: volatile: fredrir-10: Mount the data volume", "volatile.yml comparison incomplete: unreachable hosts: fredrir-10"}; !slices.Equal(verification.Degraded, want) {
		t.Fatalf("degraded %q, want %q", verification.Degraded, want)
	}
	if only := VerificationOutcome("r", ScopeFull, errors.Join(nil, degraded)); only.Outcome != OutcomeMatches || len(only.Degraded) != 2 {
		t.Fatalf("degraded-only verification %+v", only)
	}
	if err := WithoutDegraded(errors.Join(degraded, errors.Join(degraded))); err != nil {
		t.Fatalf("degraded outcome fails the command: %v", err)
	}
	if err := WithoutDegraded(errors.Join(degraded, errors.New("kept"))); err == nil || err.Error() != "kept" {
		t.Fatalf("fleet errors lost: %v", err)
	}
}

func TestVolatilePlaybookResultsSummarizeTheWorker(t *testing.T) {
	broken := junitCase("[fredrir-10] Configure volatile workers: data_volume : Require the attached data volume", "roles/data_volume/tasks/main.yml:25", `<failure message="failed"/>`)
	changed := junitCase("[fredrir-10] Configure volatile workers: data_volume : Mount the data volume", "roles/data_volume/tasks/main.yml:90", junitResult(true))
	for _, test := range []struct {
		name     string
		playbook fakePlaybooks
		apply    string
		compare  []string
	}{
		{name: "matching", playbook: fakePlaybooks{reports: []string{junitReport("volatile", junitCase("[fredrir-10] Configure volatile workers: Gather volatile worker facts", "volatile.yml:13", junitResult(false)))}}},
		{name: "unenrolled", playbook: fakePlaybooks{}},
		{name: "failed", playbook: fakePlaybooks{reports: []string{junitReport("volatile", broken)}, exit: 2}, apply: "volatile.yml host tasks failed: [fredrir-10] Configure volatile workers: data_volume : Require the attached data volume", compare: []string{"volatile.yml comparison incomplete: host tasks failed: [fredrir-10] Configure volatile workers: data_volume : Require the attached data volume"}},
		{name: "drifted", playbook: fakePlaybooks{reports: []string{junitReport("volatile", changed)}}, compare: []string{"production differs from its declaration: volatile: fredrir-10: Configure volatile workers: data_volume : Mount the data volume"}},
		{name: "unreachable", playbook: fakePlaybooks{reports: []string{junitReport("volatile")}, recap: "fredrir-10                 : ok=0    changed=0    unreachable=1    failed=0    skipped=0\n", exit: 4}, apply: "volatile.yml unreachable hosts: fredrir-10", compare: []string{"volatile.yml comparison incomplete: unreachable hosts: fredrir-10"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls [][]string
			commands := Commands{Work: t.TempDir(), Runner: ci.Runner{Dir: "/source", Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				calls = append(calls, options.Args)
				return test.playbook.execute(t, options)
			}}}
			applied := commands.Volatile(context.Background(), Plan{Affected: All()})
			if (applied == nil) != (test.apply == "") || (applied != nil && applied.Error() != test.apply) {
				t.Fatalf("convergence reported %v, want %q", applied, test.apply)
			}
			compared := VerificationOutcome("", ScopeFull, commands.compareVolatile(context.Background()))
			if compared.Outcome != OutcomeMatches || !slices.Equal(compared.Degraded, test.compare) {
				t.Fatalf("comparison reported %+v, want degraded %q", compared, test.compare)
			}
			if want := [][]string{{"-i", "inventory/production.yml", "volatile.yml"}, {"-i", "inventory/production.yml", "volatile.yml", "--check"}}; !reflect.DeepEqual(calls, want) {
				t.Fatalf("ran %q", calls)
			}
		})
	}
	if err := (&Commands{}).Volatile(context.Background(), Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners}}); err != nil {
		t.Fatalf("runner-only scope converged volatile hosts: %v", err)
	}
}
