package reconcile

import "testing"

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
