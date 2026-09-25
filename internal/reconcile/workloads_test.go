package reconcile

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestWorkloadRejectsWrongImageAndIncompleteRollout(t *testing.T) {
	expected := artifactFixture(t, `{"kind":"Deployment","spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"api","image":"example@sha256:desired"}]}}}}`)
	actual := artifactFixture(t, `{"kind":"Deployment","metadata":{"name":"api","namespace":"y","generation":3},"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"api","image":"example@sha256:desired"}]}}},"status":{"observedGeneration":3,"replicas":2,"updatedReplicas":2,"availableReplicas":2}}`)
	if err := workloadReady(expected, actual); err != nil {
		t.Fatal(err)
	}
	actual.Spec.Template.Spec.Containers[0].Image = "example@sha256:old"
	if err := workloadReady(expected, actual); err == nil {
		t.Fatal("wrong image accepted")
	}
	actual.Spec.Template.Spec.Containers[0].Image = "example@sha256:desired"
	actual.Status.Replicas = 3
	if err := workloadReady(expected, actual); err == nil {
		t.Fatal("unfinished old replica accepted")
	}
	actual.Status.Replicas = 2
	actual.Status.ObservedGeneration = 2
	if err := workloadReady(expected, actual); err == nil {
		t.Fatal("stale rollout accepted")
	}
}

func TestReplicaCountDiffersOnlyWhenDeclared(t *testing.T) {
	actual := artifactFixture(t, `{"kind":"Deployment","metadata":{"name":"api","namespace":"y","generation":2},"spec":{"replicas":3,"template":{"spec":{"containers":[{"name":"api","image":"example@sha256:desired"}]}}},"status":{"observedGeneration":2,"replicas":3,"updatedReplicas":3,"availableReplicas":3}}`)
	for _, test := range []struct {
		name, expected string
		want           Verification
	}{
		{name: "declared", expected: `{"kind":"Deployment","spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"api","image":"example@sha256:desired"}]}}}}`, want: Verification{Outcome: OutcomeDiffers, Differences: []Difference{{System: "kubernetes", Item: "Deployment y/api runs 3 replicas, want 1"}}, Errors: []string{}}},
		{name: "undeclared", expected: `{"kind":"Deployment","spec":{"template":{"spec":{"containers":[{"name":"api","image":"example@sha256:desired"}]}}}}`, want: Verification{Outcome: OutcomeFailed, Differences: []Difference{}, Errors: []string{"Deployment y/api runs 3 replicas, want the default 1"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := VerificationOutcome("", false, workloadReady(artifactFixture(t, test.expected), actual)); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("scaled workload reported %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestMigrationRequiresCompletion(t *testing.T) {
	expected := artifactFixture(t, `{"kind":"Job","spec":{"template":{"spec":{"containers":[{"name":"migrate","image":"example@sha256:new"}]}}}}`)
	actual := artifactFixture(t, `{"kind":"Job","metadata":{"name":"migrate","namespace":"llunde-pyparser"},"spec":{"template":{"spec":{"containers":[{"name":"migrate","image":"example@sha256:new"}]}}},"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}`)
	if err := workloadReady(expected, actual); err != nil {
		t.Fatal(err)
	}
	actual.Status.Conditions = nil
	if err := workloadReady(expected, actual); err == nil {
		t.Fatal("incomplete migration accepted")
	}
}

func TestPinnedControllerWorkloadEvidence(t *testing.T) {
	path := os.Getenv("INFRA_TEST_FLUX_EVIDENCE")
	if path == "" {
		t.Skip("requires isolated pinned-controller evidence")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Passed    bool
		Snapshots map[string][]json.RawMessage
	}
	if err = json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	if !evidence.Passed {
		t.Fatal("controller qualification did not pass")
	}
	decode := func(name string, index int) resource {
		t.Helper()
		items := evidence.Snapshots[name]
		if len(items) <= index {
			t.Fatalf("missing %s snapshot %d", name, index)
		}
		return artifactFixture(t, string(items[index]))
	}
	owner := decode("ready-owner-unavailable-workload", 0)
	unavailable := decode("ready-owner-unavailable-workload", 1)
	if err = ready(owner); err != nil {
		t.Fatalf("fixture owner is not Ready: %v", err)
	}
	if err = workloadReady(unavailable, unavailable); err == nil {
		t.Fatal("Ready owner masked unavailable workload")
	}
	recovered := decode("selected-recovery-with-unrelated-failure", 1)
	unrelated := decode("selected-recovery-with-unrelated-failure", 2)
	if err = workloadReady(recovered, recovered); err != nil {
		t.Fatal(err)
	}
	if err = ready(unrelated); err == nil {
		t.Fatal("fixture unrelated failure is absent")
	}
	blocked := decode("parser-application-blocked-by-migration", 0)
	if err = ready(blocked); err == nil {
		t.Fatal("application accepted before migration completion")
	}
	migration := decode("parser-migration-before-application", 0)
	application := decode("parser-migration-before-application", 1)
	job := decode("parser-migration-before-application", 2)
	artifact := decode("parser-migration-before-application", 3)
	for _, owner := range []resource{migration, application} {
		if err = ready(owner); err != nil {
			t.Fatal(err)
		}
		if owner.Status.LastAppliedRevision != artifact.Status.Artifact.Revision {
			t.Fatal("parser chain consumed different artifact")
		}
	}
	if err = workloadReady(job, job); err != nil {
		t.Fatal(err)
	}
}
