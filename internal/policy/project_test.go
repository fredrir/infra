package policy

import "testing"

func projectPod() object {
	return object{"metadata": object{"name": "job"}, "spec": object{"runtimeClassName": "gvisor", "priorityClassName": "ci", "serviceAccountName": "ci-job", "automountServiceAccountToken": false, "nodeSelector": object{"node-restriction.kubernetes.io/ci": "true"}, "containers": []any{object{"name": "job", "image": "ghcr.io/fredrir/infra-ci@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "securityContext": object{"allowPrivilegeEscalation": false, "capabilities": object{"drop": []any{"ALL"}}}, "resources": object{"requests": object{"cpu": "100m", "memory": "128Mi"}, "limits": object{"cpu": "1", "memory": "512Mi"}}}}}}
}
func publicPeer() object {
	return object{"ipBlock": object{"cidr": "0.0.0.0/0", "except": []any{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8"}}}
}
func egress(peer object, port object) object {
	return object{"spec": object{"egress": []any{object{"to": []any{peer}, "ports": []any{port}}}}}
}

func TestProjectWorkloadIsolation(t *testing.T) {
	e := newEvaluator(t)
	p := projectPod()
	if !e.project("workload-isolation", p) {
		t.Fatal("isolated workload rejected")
	}
	set(p, []any{clone(at(p, "spec", "containers", 0))}, "spec", "initContainers")
	if !e.project("workload-isolation", p) {
		t.Fatal("bounded init container rejected")
	}
	for _, mutate := range []func(object){func(p object) { set(p, object{}, "spec", "initContainers", 0, "resources") }, func(p object) { set(p, true, "spec", "initContainers", 0, "securityContext", "privileged") }} {
		candidate := clone(p).(object)
		mutate(candidate)
		if e.project("workload-isolation", candidate) {
			t.Fatal("unsafe init container admitted")
		}
	}
	p = projectPod()
	ephemeral := clone(at(p, "spec", "containers", 0)).(object)
	delete(ephemeral, "resources")
	set(p, []any{ephemeral}, "spec", "ephemeralContainers")
	if !e.project("workload-isolation", p) {
		t.Fatal("unprivileged ephemeral container rejected")
	}
	set(ephemeral, true, "securityContext", "allowPrivilegeEscalation")
	if e.project("workload-isolation", p) {
		t.Fatal("ephemeral escalation admitted")
	}
	for key, value := range (object{"volumes": []any{object{"name": "root", "hostPath": object{"path": "/"}}}, "nodeName": "control-1", "tolerations": []any{object{"operator": "Exists"}}}) {
		p := projectPod()
		set(p, value, "spec", key)
		if e.project("workload-isolation", p) {
			t.Fatalf("host access %s admitted", key)
		}
	}
	p = projectPod()
	set(p, "verified-worker", "spec", "nodeName")
	if !e.admitted([]string{"workload-isolation"}, p, "portfolio", projectRunner, "UPDATE", p) {
		t.Fatal("unchanged scheduled node rejected")
	}
}

func TestProjectNetworkBoundary(t *testing.T) {
	e := newEvaluator(t)
	for _, peer := range []object{{"namespaceSelector": object{}}, {"ipBlock": object{"cidr": "100.64.0.0/10"}}, {"ipBlock": object{"cidr": "0.0.0.0/0"}}} {
		if e.project("project-network-boundary", egress(peer, object{"port": 443})) {
			t.Fatal("private or cross-namespace egress admitted")
		}
	}
	p := egress(publicPeer(), object{"port": 443, "protocol": "TCP"})
	appendAt(p, object{"to": []any{object{"podSelector": object{"matchLabels": object{"app": "database"}}}}, "ports": []any{object{"port": 5432}}}, "spec", "egress")
	if !e.project("project-network-boundary", p) {
		t.Fatal("public HTTPS or project database rejected")
	}
	set(p, 65535, "spec", "egress", 0, "ports", 0, "endPort")
	if e.project("project-network-boundary", p) {
		t.Fatal("port range escape admitted")
	}
	for _, protocol := range []string{"TCP", "UDP"} {
		p := egress(publicPeer(), object{"port": 7844, "protocol": protocol})
		if !e.project("project-network-boundary", p) {
			t.Fatal("tunnel egress rejected")
		}
		exceptions := at(p, "spec", "egress", 0, "to", 0, "ipBlock", "except").([]any)
		exceptions = append(exceptions[:3], exceptions[4:]...)
		set(p, exceptions, "spec", "egress", 0, "to", 0, "ipBlock", "except")
		if e.project("project-network-boundary", p) {
			t.Fatal("tunnel private network exception removed")
		}
	}
}

func TestProjectBaselineAndSecretOwnership(t *testing.T) {
	e := newEvaluator(t)
	old := object{"metadata": object{"name": "default-deny"}}
	if e.admitted([]string{"project-baseline-owner"}, object{}, "portfolio", projectRunner, "DELETE", old) || !e.admitted([]string{"project-baseline-owner"}, object{}, "portfolio", reconciler, "DELETE", old) {
		t.Fatal("baseline ownership boundary")
	}
	secret := object{"spec": object{"secretStoreRef": object{"kind": "SecretStore", "name": "runtime"}, "target": object{"name": "project-runtime"}}}
	if !e.project("project-secret-boundary", secret) {
		t.Fatal("project runtime secret rejected")
	}
	set(secret, []any{object{"extract": object{"key": "all"}}}, "spec", "dataFrom")
	if e.project("project-secret-boundary", secret) {
		t.Fatal("bulk secret import admitted")
	}
	delete(at(secret, "spec").(object), "dataFrom")
	set(secret, object{"kind": "ClusterSecretStore", "name": "shared"}, "spec", "secretStoreRef")
	if e.project("project-secret-boundary", secret) {
		t.Fatal("foreign secret store admitted")
	}
	secret = object{"spec": object{"secretStoreRef": object{"kind": "SecretStore", "name": "runtime"}, "target": object{"name": "project-registry"}, "data": []any{object{"secretKey": ".dockerconfigjson", "remoteRef": object{"key": "GHCR_DOCKER_CONFIG_JSON"}}}}}
	if e.project("project-secret-boundary", secret) || !e.admitted([]string{"project-secret-boundary"}, secret, "portfolio", reconciler, "CREATE", nil) {
		t.Fatal("registry credential ownership boundary")
	}
}
