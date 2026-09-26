package contracts

import (
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestTailscaleUnderlayNeverEntersThePodNetwork(t *testing.T) {
	repository := root(t)
	var firewall struct {
		PodCIDR string `yaml:"firewall_pod_cidr"`
	}
	var k3s struct {
		PodCIDR string `yaml:"k3s_pod_cidr"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/roles/firewall/defaults/main.yml")), &firewall); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/roles/k3s/defaults/main.yml")), &k3s); err != nil {
		t.Fatal(err)
	}
	if firewall.PodCIDR == "" || firewall.PodCIDR != k3s.PodCIDR {
		t.Fatalf("firewall pod CIDR %q differs from k3s pod CIDR %q", firewall.PodCIDR, k3s.PodCIDR)
	}
	template := string(read(t, filepath.Join(repository, "ansible/roles/firewall/templates/platform-host.nft.j2")))
	_, output, found := strings.Cut(template, "chain output {")
	if !found || !strings.Contains(output, "type filter hook output") || !strings.Contains(output, "meta mark and 0xff0000 == 0x80000 ip daddr {{ firewall_pod_cidr }} drop") {
		t.Fatal("Tailscale's bypass-marked traffic can be routed through flannel, which itself rides the tailnet")
	}
}
