package contracts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func inventoryHosts(group map[string]any, hosts map[string]map[string]any) {
	members, _ := group["hosts"].(map[string]any)
	for name, variables := range members {
		if hosts[name] == nil {
			hosts[name] = map[string]any{}
		}
		values, _ := variables.(map[string]any)
		maps.Copy(hosts[name], values)
	}
	children, _ := group["children"].(map[string]any)
	for _, child := range children {
		if child, ok := child.(map[string]any); ok {
			inventoryHosts(child, hosts)
		}
	}
}

type relabeling struct {
	TargetLabel string `yaml:"targetLabel"`
	Replacement string
}

func TestBuildVmMetricsTargetMatchesItsHost(t *testing.T) {
	repository := root(t)
	var inventory struct {
		All map[string]any `yaml:"all"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/inventory/production.yml")), &inventory); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]map[string]any{}
	inventoryHosts(inventory.All, hosts)
	var buildHosts []string
	for name, variables := range hosts {
		if variables["build_vm_enabled"] == true {
			buildHosts = append(buildHosts, name)
		}
	}
	if len(buildHosts) != 1 {
		t.Fatalf("want exactly one build VM host, got %v", buildHosts)
	}
	host := hosts[buildHosts[0]]
	address, port := fmt.Sprint(host["tailscale_ip"]), fmt.Sprint(host["build_vm_metrics_port"])

	var scrape struct {
		Spec struct {
			KubernetesSDConfigs []struct {
				Selectors []struct{ Field string }
			} `yaml:"kubernetesSDConfigs"`
			Relabelings []relabeling
		}
	}
	var policy struct {
		Spec struct {
			Egress []struct {
				To []struct {
					IPBlock struct{ CIDR string } `yaml:"ipBlock"`
				}
				Ports []struct{ Port int }
			}
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(read(t, filepath.Join(repository, "platform/components/observability/build-vm.yaml"))))
	var kinds []string
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var header struct{ Kind string }
		if err := document.Decode(&header); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, header.Kind)
		switch header.Kind {
		case "ScrapeConfig":
			if err := document.Decode(&scrape); err != nil {
				t.Fatal(err)
			}
		case "NetworkPolicy":
			if err := document.Decode(&policy); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Contains(kinds, "ScrapeConfig") || !slices.Contains(kinds, "NetworkPolicy") {
		t.Fatalf("build VM metrics declare %v, want a ScrapeConfig and a NetworkPolicy", kinds)
	}
	if len(scrape.Spec.KubernetesSDConfigs) != 1 || len(scrape.Spec.KubernetesSDConfigs[0].Selectors) != 1 || scrape.Spec.KubernetesSDConfigs[0].Selectors[0].Field != "metadata.name="+buildHosts[0] {
		t.Errorf("scrape discovers %+v, want node %s", scrape.Spec.KubernetesSDConfigs, buildHosts[0])
	}
	if !slices.Contains(scrape.Spec.Relabelings, relabeling{TargetLabel: "__address__", Replacement: "$1:" + port}) {
		t.Errorf("scrape address does not use metrics port %s", port)
	}
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].To) != 1 || policy.Spec.Egress[0].To[0].IPBlock.CIDR != address+"/32" || len(policy.Spec.Egress[0].Ports) != 1 || fmt.Sprint(policy.Spec.Egress[0].Ports[0].Port) != port {
		t.Errorf("egress %+v, want only %s/32 on port %s", policy.Spec.Egress, address, port)
	}
}

func TestBuildVmReservationHoldsTheResidentGuestAndItsPageCache(t *testing.T) {
	repository := root(t)
	var defaults map[string]any
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/roles/build_vm/defaults/main.yml")), &defaults); err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		All map[string]any `yaml:"all"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/inventory/production.yml")), &inventory); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]map[string]any{}
	inventoryHosts(inventory.All, hosts)
	mebibytes := func(value any) int {
		var number int
		if _, err := fmt.Sscanf(fmt.Sprint(value), "%dMi", &number); err != nil || fmt.Sprintf("%dMi", number) != fmt.Sprint(value) {
			t.Fatalf("reservation %v is not a whole number of MiB", value)
		}
		return number
	}
	const qemuResidentOverhead, pageCacheFloor = 67, 957
	for name, variables := range hosts {
		if variables["build_vm_enabled"] != true {
			continue
		}
		setting := func(key string) any {
			if value, ok := variables[key]; ok {
				return value
			}
			return defaults[key]
		}
		guest, overhead, reserved := setting("build_vm_memory_mib").(int), setting("build_vm_memory_overhead_mib").(int), mebibytes(setting("build_vm_system_reserved_memory"))
		if overhead < qemuResidentOverhead+pageCacheFloor || guest+overhead > reserved {
			t.Errorf("%s reserves %d MiB for a %d MiB guest with %d MiB overhead, below QEMU's resident guest plus %d MiB and a %d MiB page cache", name, reserved, guest, overhead, qemuResidentOverhead, pageCacheFloor)
		}
		if mebibytes(variables["k3s_system_reserved_memory"]) != reserved {
			t.Errorf("%s reserves %v for Kubernetes and %dMi for its build VM", name, variables["k3s_system_reserved_memory"], reserved)
		}
	}
	unit := string(read(t, filepath.Join(repository, "ansible/roles/build_vm/templates/infra-build-vm.service.j2")))
	if !strings.Contains(unit, "MemoryMax={{ build_vm_system_reserved_memory | regex_replace('Mi$', 'M') }}\n") {
		t.Error("the build VM service is not capped at its Kubernetes reservation")
	}
}
