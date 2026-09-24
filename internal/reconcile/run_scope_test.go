package reconcile

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func TestIndependentChangePreservesDeployedBaseline(t *testing.T) {
	verified := time.Now().Add(-time.Hour).UTC()
	store := &memoryStore{status: Status{Desired: "old", Applied: "old", LastFullRevision: "old", LastFullVerified: verified}}
	ops := &fakeOps{selection: Affected([]string{"docs/example.md"})}
	err := (Reconciler{Store: store, Ops: ops, SkipUnchanged: true}).Apply(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops.calls) != 0 || store.status.Desired != "old" || store.status.Applied != "old" || store.status.Evaluated != "new" || store.status.Stage != "evaluated" {
		t.Fatalf("independent change affected deployment: %+v, %v", store.status, ops.calls)
	}
	if store.status.LastFullRevision != "old" || !store.status.LastFullVerified.Equal(verified) {
		t.Fatal("evaluation replaced full verification evidence")
	}
}

func TestToolingChangeVerifiesDeployedRevisionWithoutMutation(t *testing.T) {
	verified := time.Now().Add(-time.Hour).UTC()
	store := &memoryStore{status: Status{Desired: "old", Applied: "old", LastFullRevision: "old", LastFullVerified: verified}}
	ops := &fakeOps{selection: Affected([]string{".github/workflows/deploy.yml", "internal/reconcile/run.go"})}
	err := (Reconciler{Store: store, Ops: ops, Host: "logs.fredrir.com", SkipUnchanged: true}).Apply(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []string{"drift-verification"}) {
		t.Fatalf("tooling change mutated production: %v", ops.calls)
	}
	if len(ops.drift) != 1 || ops.drift[0].Revision != "old" || !sameSelection(ops.drift[0].Affected, All()) || ops.drift[0].Host != "logs.fredrir.com" {
		t.Fatalf("verification did not cover the deployed revision: %+v", ops.drift)
	}
	if store.status.Desired != "old" || store.status.Applied != "old" || store.status.Evaluated != "new" || store.status.Stage != "evaluated" {
		t.Fatalf("tooling change affected deployment: %+v", store.status)
	}
	if store.status.LastFullRevision != "old" || !store.status.LastFullVerified.Equal(verified) {
		t.Fatal("drift verification replaced full verification evidence")
	}
}

func TestToolingVerificationFailureForcesFullRecovery(t *testing.T) {
	store := &memoryStore{status: Status{Desired: "old", Applied: "old"}}
	ops := &fakeOps{selection: Affected([]string{"internal/reconcile/run.go"}), fail: "drift-verification"}
	if err := (Reconciler{Store: store, Ops: ops, SkipUnchanged: true}).Apply(context.Background(), false); err == nil {
		t.Fatal("drift verification failure lost")
	}
	if store.status.Failure == "" || store.status.Applied != "old" {
		t.Fatalf("failure not recorded: %+v", store.status)
	}
	store = &memoryStore{status: store.status}
	retry := &fakeOps{selection: Affected([]string{"internal/reconcile/run.go"})}
	if err := (Reconciler{Store: store, Ops: retry, SkipUnchanged: true}).Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(retry.calls, []string{"plan", "expand", "hosts", "publish", "kubernetes", "monitor", "verify", "retire"}) {
		t.Fatalf("recovery skipped full convergence: %v", retry.calls)
	}
}

func TestToolingChangeKeepsSelectedProjectScope(t *testing.T) {
	store := &memoryStore{status: Status{Desired: "old", Applied: "old", Stage: "complete", ArtifactsVerified: "old"}}
	ops := &checkpointOps{revision: "new", delta: Affected([]string{"internal/reconcile/run.go", "platform/projects/llunde/kustomization.yaml"})}
	if err := (Reconciler{Store: store, Ops: ops, SkipUnchanged: true, VerifyArtifacts: true}).Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []string{"plan", "publish", "kubernetes", "verify"}) {
		t.Fatalf("tooling change widened application delivery: %v", ops.calls)
	}
	if len(ops.plans) != 1 || !reflect.DeepEqual(ops.plans[0].Affected.Projects, []string{"llunde"}) {
		t.Fatalf("project scope lost: %+v", ops.plans)
	}
}

func TestToolingChangeWithoutHostScopingConvergesEverything(t *testing.T) {
	store := &memoryStore{status: Status{Desired: "old", Applied: "old"}}
	ops := &fakeOps{selection: Affected([]string{"internal/reconcile/run.go"})}
	if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []string{"plan", "expand", "hosts", "publish", "kubernetes", "monitor", "verify", "retire"}) {
		t.Fatalf("disabled host scoping skipped full convergence: %v", ops.calls)
	}
}

func TestDriftVerificationChecksGeneratedConfigurationFirst(t *testing.T) {
	commands := &Commands{RequireMain: true, Runner: ci.Runner{Dir: t.TempDir(), Execute: func(context.Context, process.Options) (process.Result, error) {
		t.Fatal("invalid generated inputs reached external commands")
		return process.Result{}, nil
	}}}
	if err := commands.VerifyDrift(context.Background(), Plan{Affected: All()}); err == nil {
		t.Fatal("drift verification accepted missing generated configuration")
	}
}

func TestFullAndRecoveryCannotSkipIndependentChange(t *testing.T) {
	for _, scenario := range []string{"explicit", "recovery", "initial", "failed-same-revision", "interrupted-same-revision"} {
		t.Run(scenario, func(t *testing.T) {
			store := &memoryStore{status: Status{Desired: "old", Applied: "old"}}
			if scenario == "recovery" {
				store.status.Desired = "failed"
			}
			if scenario == "initial" {
				store.status = Status{}
			}
			if scenario == "failed-same-revision" {
				store.status.Failure = "scheduled verification failed"
				store.status.Stage = "complete"
			}
			if scenario == "interrupted-same-revision" {
				store.status.Stage = "verify"
			}
			ops := &fakeOps{selection: Selection{}}
			if err := (Reconciler{Store: store, Ops: ops, SkipUnchanged: true}).Apply(context.Background(), scenario == "explicit"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ops.calls, []string{"plan", "expand", "hosts", "publish", "kubernetes", "monitor", "verify", "retire"}) {
				t.Fatalf("full convergence skipped: %v", ops.calls)
			}
		})
	}
}

func TestRunnerConvergenceCannotCreateFullCheckpoint(t *testing.T) {
	store := &memoryStore{status: Status{Desired: "old", Applied: "old"}}
	ops := &fakeOps{selection: Selection{Ansible: true, HostScope: HostScopeRunners}, fail: "publish"}
	if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); err == nil {
		t.Fatal("publish failure lost")
	}
	if store.status.HostScope != HostScopeRunners || store.status.Durations["hosts"] != 0 || store.status.Durations["hosts-runners"] <= 0 {
		t.Fatalf("subset convergence resembles a full checkpoint: %+v", store.status)
	}
	if reusableHosts(store.status, time.Now()) {
		t.Fatal("runner checkpoint accepted for full recovery")
	}
}

func TestRunnerSuccessDoesNotRefreshFullVerification(t *testing.T) {
	store := &memoryStore{status: Status{Desired: "old", Applied: "old", LastFullRevision: "older"}}
	ops := &fakeOps{selection: Selection{Ansible: true, HostScope: HostScopeRunners}}
	if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ops.calls, []string{"plan", "hosts", "publish", "kubernetes", "verify"}) || store.status.LastFullRevision != "older" {
		t.Fatalf("runner scope expanded or mislabeled: %v, %+v", ops.calls, store.status)
	}
}

func TestCompletionWriteFailurePreservesVerificationBaseline(t *testing.T) {
	store := &memoryStore{status: Status{Desired: "old", Applied: "old", LastFullRevision: "older"}, fail: "complete"}
	ops := &fakeOps{selection: All()}
	if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); err == nil {
		t.Fatal("status persistence failure lost")
	}
	if store.status.Applied != "old" || store.status.LastFullRevision != "older" {
		t.Fatalf("failed persistence advanced the baseline: %+v", store.status)
	}
}

type rejectedPreflight struct{ fakeOps }

func (o *rejectedPreflight) Preflight(context.Context, Plan) (Plan, error) {
	return Plan{}, errors.New("missing artifact read permission")
}

func TestPreflightFailurePreventsMutations(t *testing.T) {
	store := &memoryStore{status: Status{Desired: "old", Applied: "old"}}
	ops := &rejectedPreflight{fakeOps{selection: All()}}
	if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), false); err == nil {
		t.Fatal("preflight failure lost")
	}
	if len(ops.calls) != 0 || store.status.Applied != "old" || store.status.Failure == "" {
		t.Fatalf("preflight failure allowed mutations: %v, %+v", ops.calls, store.status)
	}
}

func TestScopeControlsApplyToSelection(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, path := range []string{"build/cli-release.json", "platform/projects/portfolio/kustomization.yaml"} {
			commands := &Commands{ScopeHosts: enabled, ScopeProjects: enabled, VerifyArtifacts: enabled, Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				if options.Args[0] == "diff" {
					return process.Result{Stdout: []byte(path + "\n")}, nil
				}
				return process.Result{}, nil
			}}}
			selected, err := commands.Select(context.Background(), strings.Repeat("a", 40), false)
			if err != nil {
				t.Fatal(err)
			}
			if selected.Ansible && (effectiveHostScope(selected) == HostScopeRunners) != enabled {
				t.Fatalf("host control ignored: %+v", selected)
			}
			if selected.Kubernetes && (len(selected.Projects) == 1) != enabled {
				t.Fatalf("project control ignored: %+v", selected)
			}
		}
	}
}

func TestProjectScopeRequiresVerifiedArtifactBaseline(t *testing.T) {
	for _, scenario := range []struct {
		name, proof     string
		enabled, scoped bool
	}{
		{"legacy state", "", true, false},
		{"outdated proof", "older", true, false},
		{"current proof", "old", true, true},
		{"verification disabled", "old", false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store := &memoryStore{status: Status{Desired: "old", Applied: "old", Stage: "complete", ArtifactsVerified: scenario.proof}}
			ops := &checkpointOps{revision: "new", delta: Selection{Kubernetes: true, Projects: []string{"portfolio"}}}
			if err := (Reconciler{Store: store, Ops: ops, VerifyArtifacts: scenario.enabled}).Apply(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			if len(ops.plans) != 1 || (len(ops.plans[0].Affected.Projects) == 1) != scenario.scoped {
				t.Fatalf("artifact baseline gate failed: %+v", ops.plans)
			}
			want := ""
			if scenario.enabled {
				want = "new"
			}
			if store.status.ArtifactsVerified != want {
				t.Fatalf("incorrect proof: %+v", store.status)
			}
		})
	}
}

func TestArtifactVerificationFailurePreservesProof(t *testing.T) {
	store := &memoryStore{status: Status{Desired: "old", Applied: "old", ArtifactsVerified: "old"}}
	ops := &fakeOps{selection: All(), fail: "verify"}
	if err := (Reconciler{Store: store, Ops: ops, VerifyArtifacts: true}).Apply(context.Background(), false); err == nil {
		t.Fatal("verification failure lost")
	}
	if store.status.ArtifactsVerified != "old" || store.status.Applied != "old" {
		t.Fatalf("failed verification advanced proof: %+v", store.status)
	}
}

func TestApplyChecksGeneratedConfigurationWithoutKubernetesSelection(t *testing.T) {
	commands := &Commands{RequireMain: true, Runner: ci.Runner{Dir: t.TempDir(), Execute: func(context.Context, process.Options) (process.Result, error) {
		t.Fatal("invalid generated inputs reached external commands")
		return process.Result{}, nil
	}}}
	if err := commands.Plan(context.Background(), Plan{Affected: Selection{Ansible: true, HostScope: HostScopeRunners}}); err == nil {
		t.Fatal("apply accepted missing generated configuration")
	}
}
