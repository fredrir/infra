package contracts

import (
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestBuildVmGuestCannotOpenConnectionsToTheHostTailnetOrCluster(t *testing.T) {
	repository := root(t)
	rules := string(read(t, filepath.Join(repository, "ansible/roles/build_vm/files/infra-build-vm-egress.nft")))
	unit := string(read(t, filepath.Join(repository, "ansible/roles/build_vm/templates/infra-build-vm.service.j2")))
	filter := string(read(t, filepath.Join(repository, "ansible/roles/build_vm/files/infra-build-vm-egress.service")))
	var k3s struct {
		PodCIDR     string `yaml:"k3s_pod_cidr"`
		ServiceCIDR string `yaml:"k3s_service_cidr"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/roles/k3s/defaults/main.yml")), &k3s); err != nil {
		t.Fatal(err)
	}
	user := regexp.MustCompile(`(?m)^User=(\S+)$`).FindStringSubmatch(unit)
	if user == nil || !strings.Contains(rules, `meta skuid != "`+user[1]+`" accept`) {
		t.Fatalf("egress filter does not select the guest's QEMU account %v", user)
	}
	chain := rules[strings.Index(rules, "chain output"):]
	for _, rule := range []string{"ct state { established, related } accept", "fib daddr type local drop", "ip daddr @guest_denied_ipv4 drop", "ip6 daddr @guest_denied_ipv6 drop"} {
		if !strings.Contains(chain, rule) {
			t.Errorf("egress filter lacks %q", rule)
		}
	}
	if accepted := regexp.MustCompile(`ct state [^\n]*accept`).FindAllString(chain, -1); len(accepted) != 1 {
		t.Errorf("egress filter accepts connection states %q; untracked packets must reach the drops", accepted)
	}
	if !strings.HasPrefix(rules, "add table inet infra_build_vm\ndelete table inet infra_build_vm\n") {
		t.Error("reloading the egress filter keeps set elements removed from its definition")
	}
	var denied []netip.Prefix
	for _, set := range regexp.MustCompile(`elements = \{([^}]*)\}`).FindAllStringSubmatch(rules, -1) {
		for element := range strings.SplitSeq(set[1], ",") {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(element))
			if err != nil {
				address, addressErr := netip.ParseAddr(strings.TrimSpace(element))
				if addressErr != nil {
					t.Fatal(err)
				}
				prefix = netip.PrefixFrom(address, address.BitLen())
			}
			denied = append(denied, prefix)
		}
	}
	for _, required := range []string{k3s.PodCIDR, k3s.ServiceCIDR, "100.64.0.0/10", "fd7a:115c:a1e0::/48", "169.254.169.254/32", "127.0.0.1/32", "::1/128"} {
		prefix := netip.MustParsePrefix(required)
		covered := false
		for _, candidate := range denied {
			covered = covered || candidate.Bits() <= prefix.Bits() && candidate.Contains(prefix.Addr())
		}
		if !covered {
			t.Errorf("guest can open connections to %s", required)
		}
	}
	if !strings.Contains(filter, "RequiredBy=infra-build-vm.service") || !strings.Contains(filter, "Before=infra-build-vm.service") {
		t.Error("the guest can start without its egress filter")
	}
}
