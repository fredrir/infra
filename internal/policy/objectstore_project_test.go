package policy

import "testing"

func objectStoreEgress() object {
	return object{"metadata": object{"name": "object-store-egress"}, "spec": object{"egress": []any{object{
		"to":    []any{object{"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": "object-store"}}, "podSelector": object{"matchLabels": object{"app.kubernetes.io/name": "seaweedfs", "app.kubernetes.io/instance": "nl"}}}},
		"ports": []any{object{"port": 8333, "protocol": "TCP"}},
	}}}}
}

func TestProjectEgressReachesOnlyItsObjectStoreCell(t *testing.T) {
	e := newEvaluator(t)
	boundary := []string{"project-network-boundary"}
	if !e.admitted(boundary, objectStoreEgress(), "llunde-pyparser", reconciler, "CREATE", nil) {
		t.Fatal("platform object store egress rejected")
	}
	if e.admitted(boundary, objectStoreEgress(), "llunde-pyparser", projectRunner, "CREATE", nil) {
		t.Fatal("project runner granted object store egress")
	}
	for _, namespace := range []string{"y", "portfolio", "llunde"} {
		if e.admitted(boundary, objectStoreEgress(), namespace, reconciler, "CREATE", nil) {
			t.Errorf("object store egress admitted in %s", namespace)
		}
	}
	for name, mutate := range map[string]func(object){
		"other name": func(p object) { set(p, "egress", "metadata", "name") },
		"other cell": func(p object) {
			set(p, "hel1", "spec", "egress", 0, "to", 0, "podSelector", "matchLabels", "app.kubernetes.io/instance")
		},
		"any object store pod": func(p object) { set(p, object{}, "spec", "egress", 0, "to", 0, "podSelector") },
		"any namespace":        func(p object) { set(p, object{}, "spec", "egress", 0, "to", 0, "namespaceSelector") },
		"gRPC port":            func(p object) { set(p, 18333, "spec", "egress", 0, "ports", 0, "port") },
		"port range":           func(p object) { set(p, 18333, "spec", "egress", 0, "ports", 0, "endPort") },
		"UDP":                  func(p object) { set(p, "UDP", "spec", "egress", 0, "ports", 0, "protocol") },
		"extra port":           func(p object) { appendAt(p, object{"port": 9327}, "spec", "egress", 0, "ports") },
		"no ports":             func(p object) { delete(at(p, "spec", "egress", 0).(object), "ports") },
	} {
		p := objectStoreEgress()
		mutate(p)
		if e.admitted(boundary, p, "llunde-pyparser", reconciler, "CREATE", nil) {
			t.Errorf("%s admitted", name)
		}
	}
}

func TestProjectNetworkPoliciesPassTheBoundary(t *testing.T) {
	e := newEvaluator(t)
	for _, project := range []string{"llunde", "portfolio", "y", "llunde-pyparser"} {
		for _, resource := range renderedTree(t, "platform", "platform/projects/"+project) {
			if resource["kind"] == "NetworkPolicy" && !e.admitted([]string{"project-network-boundary"}, resource, project, reconciler, "CREATE", nil) {
				t.Errorf("%s/%s violates the project network boundary", project, at(resource, "metadata", "name"))
			}
		}
	}
}
