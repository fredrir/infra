package contracts

import (
	"bytes"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestNetpolCanaryAddressesAreReservedServiceAddresses(t *testing.T) {
	repository := root(t)
	var k3s struct {
		ServiceCIDR string `yaml:"k3s_service_cidr"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/roles/k3s/defaults/main.yml")), &k3s); err != nil {
		t.Fatal(err)
	}
	services, err := netip.ParsePrefix(k3s.ServiceCIDR)
	if err != nil {
		t.Fatal(err)
	}
	size := 1 << (32 - services.Bits())
	static := min(max(16, size/16), 256)
	first := services.Masked().Addr().As4()
	decoder := yaml.NewDecoder(bytes.NewReader(read(t, filepath.Join(repository, "platform/components/netpol-gate/canary.yaml"))))
	var addresses []netip.Addr
	for {
		var resource struct {
			Kind string
			Spec struct {
				ClusterIP string `yaml:"clusterIP"`
			}
		}
		if err := decoder.Decode(&resource); err != nil {
			break
		}
		if resource.Kind != "Service" {
			continue
		}
		address, err := netip.ParseAddr(resource.Spec.ClusterIP)
		if err != nil {
			t.Fatalf("canary service without a fixed address: %v", err)
		}
		offset := int(address.As4()[2])<<8 | int(address.As4()[3]) - (int(first[2])<<8 | int(first[3]))
		if !services.Contains(address) || offset <= 10 || offset >= static {
			t.Fatalf("%s must sit in the static band of %s, above the API and DNS addresses", address, services)
		}
		addresses = append(addresses, address)
	}
	if len(addresses) < 2 {
		t.Fatalf("expected a fixed address per canary replica, got %v", addresses)
	}
}

func TestNetpolCanaryAcceptsEveryPodAddress(t *testing.T) {
	repository := root(t)
	var k3s struct {
		PodCIDR string `yaml:"k3s_pod_cidr"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/roles/k3s/defaults/main.yml")), &k3s); err != nil {
		t.Fatal(err)
	}
	_, pods, err := net.ParseCIDR(k3s.PodCIDR)
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(read(t, filepath.Join(repository, "platform/components/netpol-gate/network.yaml"))))
	for {
		var policy struct {
			Metadata struct{ Name string }
			Spec     struct {
				Ingress []struct {
					From []struct {
						IPBlock *struct{ CIDR string } `yaml:"ipBlock"`
					}
					Ports []struct{ Port int }
				}
			}
		}
		if err := decoder.Decode(&policy); err != nil {
			t.Fatal("canary ingress policy missing")
		}
		if policy.Metadata.Name != "canary-probes" {
			continue
		}
		rules := policy.Spec.Ingress
		if len(rules) != 1 || len(rules[0].From) != 1 || rules[0].From[0].IPBlock == nil || rules[0].From[0].IPBlock.CIDR != pods.String() {
			t.Fatalf("canary ingress must admit exactly the pod network %s so a refusal can only come from the client's egress policy", pods)
		}
		ports := []int{}
		for _, port := range rules[0].Ports {
			ports = append(ports, port.Port)
		}
		if !slices.Equal(ports, []int{8080, 8081}) {
			t.Fatalf("canary ingress must admit both probe ports, got %v", ports)
		}
		return
	}
}
