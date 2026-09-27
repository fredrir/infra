package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const canaryComponent = "platform/components/netpol-gate"

func canaryResources(t *testing.T) (statefulSet object, services map[string]object, budget object) {
	t.Helper()
	services = map[string]object{}
	for _, resource := range renderedTree(t, "platform/components", canaryComponent) {
		switch resource["kind"] {
		case "StatefulSet":
			statefulSet = resource
		case "Service":
			services[at(resource, "metadata", "name").(string)] = resource
		case "PodDisruptionBudget":
			budget = resource
		}
	}
	if statefulSet == nil || budget == nil {
		t.Fatal("canary StatefulSet or disruption budget missing")
	}
	return statefulSet, services, budget
}

func TestCanarySurvivesTheLossOfAnyOneNode(t *testing.T) {
	t.Parallel()
	statefulSet, _, budget := canaryResources(t)
	spec := at(statefulSet, "spec").(object)
	replicas := spec["replicas"].(int)
	if replicas < 2 || spec["podManagementPolicy"] != "Parallel" {
		t.Fatalf("%d replicas with %v management cannot outlive one node", replicas, spec["podManagementPolicy"])
	}
	labels := at(spec, "template", "metadata", "labels").(object)
	pod := at(spec, "template", "spec").(object)
	if !requiresCriticalNodes(pod["affinity"]) {
		t.Fatal("canary may run on volatile or control-plane nodes")
	}
	if _, tolerates := pod["tolerations"]; tolerates {
		t.Fatal("canary tolerates node taints")
	}
	spread, _ := at(pod, "affinity").(object)["podAntiAffinity"].(object)
	terms, _ := spread["requiredDuringSchedulingIgnoredDuringExecution"].([]any)
	if len(terms) != 1 || at(terms[0], "topologyKey") != "kubernetes.io/hostname" || fmt.Sprint(at(terms[0], "labelSelector", "matchLabels")) != fmt.Sprint(labels) {
		t.Fatal("canary replicas may share a node")
	}
	minimum := at(budget, "spec", "minAvailable").(int)
	if minimum < 1 || minimum >= replicas || fmt.Sprint(at(budget, "spec", "selector", "matchLabels")) != fmt.Sprint(labels) {
		t.Fatal("canary disruption budget must keep one replica and allow one drain")
	}
}

func TestEveryCanaryReplicaHasItsOwnServiceAddress(t *testing.T) {
	t.Parallel()
	statefulSet, services, _ := canaryResources(t)
	name := at(statefulSet, "metadata", "name").(string)
	replicas := at(statefulSet, "spec", "replicas").(int)
	if len(services) != replicas {
		t.Fatalf("%d replicas, %d services", replicas, len(services))
	}
	var addresses []string
	for replica := range replicas {
		service, ok := services[fmt.Sprintf("%s-%d", name, replica)]
		if !ok {
			t.Fatalf("replica %d has no service", replica)
		}
		selector := at(service, "spec", "selector").(object)
		if selector["statefulset.kubernetes.io/pod-name"] != fmt.Sprintf("%s-%d", name, replica) || len(selector) != 2 {
			t.Fatalf("service %v must select exactly one replica", selector)
		}
		ports := map[string]int{}
		for _, port := range at(service, "spec", "ports").([]any) {
			if _, retargeted := port.(object)["targetPort"]; retargeted {
				t.Fatal("canary ports must reach the same container ports")
			}
			ports[at(port, "name").(string)] = at(port, "port").(int)
		}
		if ports["allowed"] != 8080 || ports["denied"] != 8081 || len(ports) != 2 {
			t.Fatalf("canary ports %v differ from the gate defaults", ports)
		}
		addresses = append(addresses, at(service, "spec", "clusterIP").(string))
	}
	if distinct := slices.Compact(slices.Sorted(slices.Values(addresses))); len(distinct) != replicas {
		t.Fatalf("canary addresses %v are not distinct", addresses)
	}
}

func TestCanaryIsAnEndpointOnlyWhileItsDeniedPortListens(t *testing.T) {
	t.Parallel()
	statefulSet, services, _ := canaryResources(t)
	container := at(statefulSet, "spec", "template", "spec", "containers", 0).(object)
	ports := map[any]int{}
	for _, port := range container["ports"].([]any) {
		ports[at(port, "name")] = at(port, "containerPort").(int)
	}
	probe, _ := container["readinessProbe"].(object)
	socket, _ := probe["tcpSocket"].(object)
	if target := socket["port"]; target != 8081 && ports[target] != 8081 {
		t.Fatalf("readiness %v must be a TCP check of the denied port 8081, or a canary without that listener refuses it and passes every gate", probe)
	}
	for name, service := range services {
		spec := at(service, "spec").(object)
		if publish, set := spec["publishNotReadyAddresses"]; set && publish != false {
			t.Fatalf("%s publishes canaries whose denied port may not listen", name)
		}
		if selector, _ := spec["selector"].(object); len(selector) == 0 {
			t.Fatalf("%s needs a selector so only ready canaries become endpoints", name)
		}
	}
	for _, resource := range renderedTree(t, "platform/components", canaryComponent) {
		if resource["kind"] == "Endpoints" || resource["kind"] == "EndpointSlice" {
			t.Fatalf("static %s bypasses canary readiness", resource["kind"])
		}
	}
}

func TestEveryUntrustedNamespaceMayReachOnlyTheCanaryControlPort(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	var untrusted, reachable []string
	for tree, path := range map[string]string{"platform/components": "platform/components/runners", "platform": "platform/projects"} {
		for _, resource := range renderedTree(t, tree, path) {
			name, _ := at(resource, "metadata", "name").(string)
			switch resource["kind"] {
			case "Namespace":
				if labels, _ := at(resource, "metadata").(object)["labels"].(object); labels["infra.fredrir.com/tier"] == "ci" || labels["infra.fredrir.com/tier"] == "project" {
					untrusted = append(untrusted, name)
				}
			case "NetworkPolicy":
				if name != "netpol-gate" {
					continue
				}
				reachable = append(reachable, at(resource, "metadata", "namespace").(string))
				if !e.project("project-network-boundary", resource) {
					t.Fatal("the canary egress policy is rejected in project namespaces")
				}
				escape := clone(resource).(object)
				set(escape, 8081, "spec", "egress", 0, "ports", 0, "port")
				if e.project("project-network-boundary", escape) {
					t.Fatal("a project may allow the canary's denied port")
				}
			}
		}
	}
	slices.Sort(untrusted)
	slices.Sort(reachable)
	if len(untrusted) < 7 || !slices.Equal(untrusted, reachable) {
		t.Fatalf("untrusted namespaces %v, namespaces that may reach the canary %v", untrusted, reachable)
	}
	old := object{"metadata": object{"name": "netpol-gate"}}
	if e.admitted([]string{"project-baseline-owner"}, object{}, "portfolio", projectRunner, "DELETE", old) {
		t.Fatal("a project may delete its canary egress policy")
	}
}

func TestCanaryReconcilesIndependentlyOfTheWorkloadsItGates(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "platform/clusters/production/root.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var canary object
	for _, kustomization := range yamlObjects(t, data) {
		name := at(kustomization, "metadata", "name")
		if at(kustomization, "spec", "path") == "./"+canaryComponent {
			canary = kustomization
		}
		dependencies, _ := at(kustomization, "spec").(object)["dependsOn"].([]any)
		for _, dependency := range dependencies {
			if at(dependency, "name") == "platform-netpol-gate" {
				t.Errorf("%s waits for the canary, so a canary fault blocks its reconciliation", name)
			}
		}
	}
	if canary == nil || at(canary, "metadata", "name") != "platform-netpol-gate" {
		t.Fatal("no Flux Kustomization applies the canary")
	}
	spec := at(canary, "spec").(object)
	if fmt.Sprint(spec["dependsOn"]) != fmt.Sprint([]any{object{"name": "platform-policy"}}) || spec["wait"] != true || spec["prune"] != true {
		t.Fatalf("canary must depend only on platform-policy and report its health, got %v", spec)
	}
}
