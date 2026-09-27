package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func portNumber(t *testing.T, value any) int {
	t.Helper()
	var number int
	if _, err := fmt.Sscan(fmt.Sprint(value), &number); err != nil {
		t.Fatalf("port %v is not numeric", value)
	}
	return number
}

func TestPrometheusReachesEveryObjectStoreScrapePort(t *testing.T) {
	t.Parallel()
	servicePorts := map[string]int{}
	var scraped []string
	for _, resource := range objectStoreResources(t) {
		name, _ := lookup(resource, "metadata", "name").(string)
		switch resource["kind"] {
		case "Service":
			if !strings.HasPrefix(name, "seaweedfs-") {
				continue
			}
			ports, _ := lookup(resource, "spec", "ports").([]any)
			for _, port := range ports {
				portName, _ := lookup(port, "name").(string)
				number := portNumber(t, lookup(port, "port"))
				if previous, ok := servicePorts[portName]; ok && previous != number {
					t.Fatalf("object store services disagree on port %s", portName)
				}
				servicePorts[portName] = number
			}
		case "ServiceMonitor":
			endpoints, _ := lookup(resource, "spec", "endpoints").([]any)
			for _, endpoint := range endpoints {
				port, _ := lookup(endpoint, "port").(string)
				scraped = append(scraped, port)
			}
		}
	}
	if len(scraped) == 0 {
		t.Fatal("object store declares no scraped endpoints")
	}

	var allowed []int
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/observability/network.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range yamlObjects(t, data) {
		if resource["kind"] != "NetworkPolicy" || lookup(resource, "metadata", "name") != "internal-observability" {
			continue
		}
		rules, _ := lookup(resource, "spec", "egress").([]any)
		for _, rule := range rules {
			peers, _ := lookup(rule, "to").([]any)
			if !slices.ContainsFunc(peers, func(peer any) bool {
				return lookup(peer, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") == "object-store" &&
					lookup(peer, "podSelector", "matchLabels", "app.kubernetes.io/name") == "seaweedfs"
			}) {
				continue
			}
			ports, _ := lookup(rule, "ports").([]any)
			for _, port := range ports {
				allowed = append(allowed, portNumber(t, lookup(port, "port")))
			}
		}
	}
	for _, name := range scraped {
		port, ok := servicePorts[name]
		if !ok {
			t.Errorf("ServiceMonitor scrapes %s, which no object store service declares", name)
			continue
		}
		if !slices.Contains(allowed, port) {
			t.Errorf("Prometheus egress does not reach the object store's %s port %d", name, port)
		}
	}
}
