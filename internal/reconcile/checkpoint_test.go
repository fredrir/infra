package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

type checkpointOps struct {
	fakeOps
	revision   string
	delta      Selection
	unchanged  bool
	proofError error
	proof      func()
	selections []struct {
		base string
		full bool
	}
	plans []Plan
}

func (o *checkpointOps) Revision(context.Context) (string, error) { return o.revision, nil }
func (o *checkpointOps) Select(_ context.Context, base string, full bool) (Selection, error) {
	o.selections = append(o.selections, struct {
		base string
		full bool
	}{base, full})
	if full || base == "" {
		return All(), nil
	}
	return o.delta, nil
}
func (o *checkpointOps) Plan(_ context.Context, plan Plan) error {
	o.plans = append(o.plans, plan)
	return o.call("plan")
}
func (o *checkpointOps) ExpansionUnchanged(context.Context) (bool, error) {
	o.calls = append(o.calls, "host-check")
	if o.proof != nil {
		o.proof()
	}
	return o.unchanged, o.proofError
}

type checkpointStore struct{ memoryStore }

func (s *checkpointStore) Write(ctx context.Context, status Status) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return err
	}
	return s.memoryStore.Write(ctx, status)
}
func (s *checkpointStore) Read(context.Context) (Status, error) {
	data, err := json.Marshal(s.status)
	if err != nil {
		return Status{}, err
	}
	var status Status
	err = json.Unmarshal(data, &status)
	return status, err
}
func (s *checkpointStore) Lock(ctx context.Context) (func() error, error) {
	unlock, err := s.memoryStore.Lock(ctx)
	if err != nil {
		return nil, err
	}
	return func() error { s.locked = false; return unlock() }, nil
}

func hostCheckpoint() Status {
	return Status{Applied: strings.Repeat("a", 40), Desired: strings.Repeat("b", 40), Stage: "publish", Updated: time.Now().Add(-time.Minute), Durations: map[string]float64{"plan": 1, "expand": 2, "hosts": 600}}
}

func TestReconciliationResumesOnlyDurablyCompletedHosts(t *testing.T) {
	applied, prior, current := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	store := &checkpointStore{memoryStore{status: Status{Applied: applied, Desired: applied}}}
	first := &checkpointOps{fakeOps: fakeOps{fail: "publish"}, revision: prior, delta: All()}
	if err := (Reconciler{Store: store, Ops: first}).Apply(context.Background(), false); err == nil {
		t.Fatal("publish failure lost")
	}
	if !reusableHosts(store.status, time.Now()) {
		t.Fatalf("completed prerequisites were not durable: %+v", store.status)
	}
	second := &checkpointOps{revision: current, delta: Selection{Kubernetes: true}, unchanged: true}
	if err := (Reconciler{Store: store, Ops: second}).Apply(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	want := []string{"plan", "expand", "host-check", "publish", "kubernetes", "monitor", "verify", "retire"}
	if !reflect.DeepEqual(second.calls, want) {
		t.Fatalf("pending transition stages lost: %v", second.calls)
	}
	if len(second.selections) != 2 || second.selections[0].base != prior || second.selections[0].full || second.selections[1].base != applied || !second.selections[1].full {
		t.Fatalf("checkpoint comparison lost: %+v", second.selections)
	}
	if second.plans[0].Affected != All() || second.plans[0].Base != applied || store.status.Applied != current || store.status.HostsReusedFrom != prior {
		t.Fatalf("original transition or reuse receipt lost: %+v, %+v", second.plans, store.status)
	}
}

func TestHostReuseFallsBackForUncertainCheckpointOrChangedInputs(t *testing.T) {
	for _, scenario := range []string{"full", "earlier-stage", "later-stage", "missing-duration", "zero-duration", "missing-state", "invalid-desired", "invalid-applied", "already-applied", "old", "future", "ansible", "tofu", "unknown-revision", "drift", "proof-failure"} {
		t.Run(scenario, func(t *testing.T) {
			status := hostCheckpoint()
			ops := &checkpointOps{revision: strings.Repeat("c", 40), unchanged: true}
			full := scenario == "full"
			switch scenario {
			case "earlier-stage":
				status.Stage = "hosts"
			case "later-stage":
				status.Stage = "verify"
			case "missing-duration":
				delete(status.Durations, "hosts")
			case "zero-duration":
				status.Durations["hosts"] = 0
			case "missing-state":
				status = Status{}
			case "invalid-desired":
				status.Desired = "main"
			case "invalid-applied":
				status.Applied = ""
			case "already-applied":
				status.Desired = status.Applied
				ops.delta = All()
			case "old":
				status.Updated = time.Now().Add(-91 * time.Minute)
			case "future":
				status.Updated = time.Now().Add(time.Minute)
			case "ansible":
				ops.delta.Ansible = true
			case "tofu":
				ops.delta.Tofu = true
			case "unknown-revision":
				ops.delta = All()
			case "drift":
				ops.unchanged = false
			case "proof-failure":
				ops.proofError = errors.New("plan unavailable")
			}
			store := &checkpointStore{memoryStore{status: status}}
			if err := (Reconciler{Store: store, Ops: ops}).Apply(context.Background(), full); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, call := range ops.calls {
				found = found || call == "hosts"
			}
			if !found || store.status.HostsReusedFrom != "" {
				t.Fatalf("uncertain checkpoint skipped hosts: %v, %+v", ops.calls, store.status)
			}
		})
	}
	for _, duration := range []float64{math.NaN(), math.Inf(1), -1} {
		status := hostCheckpoint()
		status.Durations["hosts"] = duration
		if reusableHosts(status, time.Now()) {
			t.Fatal("invalid duration accepted")
		}
	}
}

func TestHostReusePreservesFailuresAndCancellation(t *testing.T) {
	for _, failure := range []string{"plan", "expand", "publish", "kubernetes", "monitor", "verify", "retire", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &checkpointStore{memoryStore{status: hostCheckpoint()}}
			ops := &checkpointOps{fakeOps: fakeOps{fail: failure}, revision: strings.Repeat("c", 40), unchanged: true}
			if failure == "cancellation" {
				ops.proof = cancel
			}
			err := (Reconciler{Store: store, Ops: ops}).Apply(ctx, false)
			if err == nil || store.status.Applied != strings.Repeat("a", 40) || store.status.Failure == "" || !store.released {
				t.Fatalf("failed transition was accepted: %v, %+v", err, store.status)
			}
			if failure == "cancellation" {
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(ops.calls, []string{"plan", "expand", "host-check"}) {
					t.Fatalf("canceled proof continued: %v, %v", ops.calls, err)
				}
			} else if ops.calls[len(ops.calls)-1] != failure {
				t.Fatalf("continued after failure: %v", ops.calls)
			}
			if failure == "publish" {
				if reusableHosts(store.status, time.Now()) {
					t.Fatal("reused work forged a new completed host checkpoint")
				}
				retry := &checkpointOps{revision: strings.Repeat("d", 40), unchanged: true}
				if err := (Reconciler{Store: store, Ops: retry}).Apply(context.Background(), false); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(retry.calls, []string{"plan", "expand", "hosts", "publish", "kubernetes", "monitor", "verify", "retire"}) {
					t.Fatalf("interrupted reuse did not recover fully: %v", retry.calls)
				}
			}
		})
	}
}

func noDriftPlan() map[string]any {
	return map[string]any{"format_version": "1.2", "prior_state": map[string]any{}, "planned_values": map[string]any{}, "resource_changes": []any{map[string]any{"address": "hcloud_server.worker", "change": map[string]any{"actions": []string{"no-op"}}}}}
}

func TestHostReuseRequiresNoPlannedChangesOrObservedDrift(t *testing.T) {
	for _, scenario := range []string{"unchanged", "create", "update", "delete", "read", "replace", "unknown-action", "missing-action", "resource-drift", "deferred", "moved", "deposed", "outputs", "unknown-check", "failed-check", "errored", "unsupported-version", "missing-version", "missing-prior", "missing-planned", "missing-resources", "malformed-prior", "import", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			plan := noDriftPlan()
			resource := plan["resource_changes"].([]any)[0].(map[string]any)
			switch scenario {
			case "create", "update", "delete", "read", "unknown-action":
				resource["change"] = map[string]any{"actions": []string{scenario}}
			case "replace":
				resource["change"] = map[string]any{"actions": []string{"delete", "create"}}
			case "missing-action":
				delete(resource, "change")
			case "resource-drift":
				plan["resource_drift"] = []any{map[string]any{}}
			case "deferred":
				plan["deferred_changes"] = []any{map[string]any{}}
			case "moved":
				resource["previous_address"] = "hcloud_server.old"
			case "deposed":
				resource["deposed"] = "old"
			case "outputs":
				plan["output_changes"] = map[string]any{"inventory": map[string]any{"actions": []string{"update"}}}
			case "unknown-check":
				plan["checks"] = []any{map[string]any{"status": "unknown"}}
			case "failed-check":
				plan["checks"] = []any{map[string]any{"status": "fail"}}
			case "errored":
				plan["errored"] = true
			case "unsupported-version":
				plan["format_version"] = "2.0"
			case "missing-version":
				delete(plan, "format_version")
			case "missing-prior":
				plan["prior_state"] = nil
			case "missing-planned":
				delete(plan, "planned_values")
			case "missing-resources":
				delete(plan, "resource_changes")
			case "malformed-prior":
				plan["prior_state"] = "invalid"
			case "import":
				resource["change"].(map[string]any)["importing"] = map[string]any{"id": "replacement"}
			case "incomplete":
				plan["complete"] = false
			}
			data, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if unchangedExpansion(data) != (scenario == "unchanged") {
				t.Fatalf("unsafe expansion proof for %s", scenario)
			}
		})
	}
	for _, data := range []string{"", "{}", "null", "not JSON"} {
		if unchangedExpansion([]byte(data)) {
			t.Fatalf("invalid plan accepted: %q", data)
		}
	}
}

func TestHostReuseInspectsTheSavedExpansionPlan(t *testing.T) {
	work := t.TempDir()
	expected := filepath.Join(work, "expand.tfplan")
	commands := Commands{Work: work, Runner: ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		if options.Name != "tofu" || !reflect.DeepEqual(options.Args, []string{"-chdir=tofu", "show", "-json", expected}) {
			return process.Result{}, errors.New("wrong plan inspected")
		}
		data, err := json.Marshal(noDriftPlan())
		return process.Result{Stdout: data}, err
	}}}
	unchanged, err := commands.ExpansionUnchanged(context.Background())
	if err != nil || !unchanged {
		t.Fatalf("saved expansion plan not verified: %t, %v", unchanged, err)
	}
}

func TestSavedExpansionProofWithLocalTofu(t *testing.T) {
	if os.Getenv("INFRA_RECONCILE_PLAN_QUALIFY") != "1" {
		t.Skip("requires OpenTofu")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	work := filepath.Join(root, "tofu")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(work, "main.tf")
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(config, []byte(fmt.Sprintf("resource \"terraform_data\" \"fixture\" { input = %q }\n", value)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		t.Helper()
		command := exec.CommandContext(ctx, "tofu", args...)
		command.Dir = work
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("OpenTofu fixture: %v\n%s", err, output)
		}
	}
	write("original")
	run("init", "-backend=false", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color")
	commands := Commands{Work: root, Runner: ci.Runner{Dir: root}}
	for _, changed := range []bool{false, true} {
		if changed {
			write("changed")
		}
		run("plan", "-input=false", "-no-color", "-out="+filepath.Join(root, "expand.tfplan"))
		run("apply", "-input=false", "-no-color", filepath.Join(root, "expand.tfplan"))
		unchanged, err := commands.ExpansionUnchanged(ctx)
		if err != nil || unchanged == changed {
			t.Fatalf("changed=%t: reusable=%t, error=%v", changed, unchanged, err)
		}
	}
}
