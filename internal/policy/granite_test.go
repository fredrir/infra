package policy

import "testing"

func TestGraniteServesTheParserFromThePinnedNode(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	var served bool
	for _, resource := range renderedTree(t, "platform", "platform/projects/llunde-pyparser") {
		if resource["kind"] != "Deployment" || at(resource, "metadata", "name") != "granite" {
			continue
		}
		served = true
		pod := clone(at(resource, "spec", "template")).(object)
		if !e.admitted([]string{"workload-isolation"}, pod, "llunde-pyparser", reconciler, "CREATE", nil) {
			t.Fatal("granite workload violates the isolation policy")
		}
		spec := at(pod, "spec").(object)
		if at(spec, "nodeSelector", "kubernetes.io/hostname") != "fredrir-09" {
			t.Fatal("granite must share the parser's node and model cache")
		}
		container := at(spec, "containers", 0).(object)
		if at(container, "securityContext", "readOnlyRootFilesystem") != true || at(spec, "securityContext", "runAsNonRoot") != true {
			t.Fatal("granite runs with a writable root or as root")
		}
	}
	if !served {
		t.Fatal("granite deployment missing")
	}
	for _, resource := range renderedTree(t, "platform", "platform/projects/llunde-pyparser/application") {
		if resource["kind"] != "Deployment" {
			continue
		}
		environment := map[string]string{}
		for _, env := range at(resource, "spec", "template", "spec", "containers", 0, "env").([]any) {
			environment[at(env, "name").(string)], _ = lookup(env, "value").(string)
		}
		if environment["PYPARSER_GRANITE_ENDPOINT"] != "http://granite:8080" || environment["PYPARSER_GRANITE_API"] != "chat-completions" {
			t.Errorf("%s reaches granite with %v", at(resource, "metadata", "name"), environment)
		}
	}
}
