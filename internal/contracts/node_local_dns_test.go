package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type nodeLocalDNS struct {
	Upstream struct {
		Metadata struct{ Name string }
		Spec     struct{ Selector map[string]string }
	}
	Config struct {
		Data struct {
			Corefile string `yaml:"Corefile"`
		}
	}
	Cache struct {
		Metadata struct{ Namespace string }
		Spec     struct {
			Template struct {
				Metadata struct{ Labels map[string]string }
				Spec     struct {
					Tolerations []struct{ Key, Operator, Value, Effect string }
					Containers  []struct {
						Image string
						Args  []string
						Ports []struct {
							Name          string
							ContainerPort int `yaml:"containerPort"`
						}
					}
				}
			}
		}
	}
}

type hostFirewall struct {
	CacheAddresses   []string `yaml:"firewall_node_local_dns_addresses"`
	CacheMetricsPort int      `yaml:"firewall_node_local_dns_metrics_port"`
}

type networkPort struct {
	Port     int
	Protocol string
}

type egressRule struct {
	To    []any
	Ports []networkPort
}

func decodeDocuments(t *testing.T, path string, decode func(kind string, document *yaml.Node)) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(read(t, filepath.Join(root(t), path))))
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		var header struct{ Kind string }
		if err := document.Decode(&header); err != nil {
			t.Fatal(err)
		}
		decode(header.Kind, &document)
	}
}

func loadNodeLocalDNS(t *testing.T) nodeLocalDNS {
	t.Helper()
	var cache nodeLocalDNS
	decodeDocuments(t, "platform/components/dns/node-local-dns.yaml", func(kind string, document *yaml.Node) {
		targets := map[string]any{"Service": &cache.Upstream, "ConfigMap": &cache.Config, "DaemonSet": &cache.Cache}
		target, ok := targets[kind]
		if !ok {
			t.Fatalf("unexpected %s in the node-local DNS component", kind)
		}
		if err := document.Decode(target); err != nil {
			t.Fatal(err)
		}
	})
	if len(cache.Cache.Spec.Template.Spec.Containers) != 1 {
		t.Fatal("node-local DNS cache must run one container")
	}
	return cache
}

func (c nodeLocalDNS) flags() map[string]string {
	flags := map[string]string{}
	for _, argument := range c.Cache.Spec.Template.Spec.Containers[0].Args {
		name, value, _ := strings.Cut(strings.TrimPrefix(argument, "-"), "=")
		flags[name] = value
	}
	return flags
}

func (c nodeLocalDNS) port(t *testing.T, name string) int {
	t.Helper()
	for _, port := range c.Cache.Spec.Template.Spec.Containers[0].Ports {
		if port.Name == name {
			return port.ContainerPort
		}
	}
	t.Fatalf("node-local DNS cache declares no %s port", name)
	return 0
}

func loadHostFirewall(t *testing.T) (hostFirewall, string) {
	t.Helper()
	repository := root(t)
	var firewall hostFirewall
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/roles/firewall/defaults/main.yml")), &firewall); err != nil {
		t.Fatal(err)
	}
	return firewall, string(read(t, filepath.Join(repository, "ansible/roles/firewall/templates/platform-host.nft.j2")))
}

func TestNodeLocalDNSCacheShadowsClusterDNSBehindTheHostFirewall(t *testing.T) {
	cache := loadNodeLocalDNS(t)
	flags := cache.flags()
	addresses := strings.Split(flags["localip"], ",")

	var k3s struct {
		ServiceCIDR string `yaml:"k3s_service_cidr"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), "ansible/roles/k3s/defaults/main.yml")), &k3s); err != nil {
		t.Fatal(err)
	}
	clusterDNS := netip.MustParsePrefix(k3s.ServiceCIDR).Masked().Addr()
	for range 10 {
		clusterDNS = clusterDNS.Next()
	}
	if !slices.Contains(addresses, clusterDNS.String()) {
		t.Errorf("cache binds %v, not the cluster DNS address %s that pods query", addresses, clusterDNS)
	}

	binds := regexp.MustCompile(`(?m)^\s*bind\s+(.+)$`).FindAllStringSubmatch(cache.Config.Data.Corefile, -1)
	servers := strings.Count(cache.Config.Data.Corefile, ":53 {")
	if servers == 0 || len(binds) != servers {
		t.Fatalf("%d of %d Corefile servers declare a bind", len(binds), servers)
	}
	for _, bind := range binds {
		if fields := strings.Fields(bind[1]); !slices.Equal(fields, addresses) {
			t.Errorf("Corefile binds %v, want the interface addresses %v", fields, addresses)
		}
	}

	if flags["upstreamsvc"] != cache.Upstream.Metadata.Name || cache.Upstream.Metadata.Name == "kube-dns" || cache.Upstream.Spec.Selector["k8s-app"] != "kube-dns" {
		t.Errorf("cache forwards to Service %q selecting %v; it must reach CoreDNS through a Service other than the address it shadows", flags["upstreamsvc"], cache.Upstream.Spec.Selector)
	}

	firewall, template := loadHostFirewall(t)
	if !slices.Equal(slices.Sorted(slices.Values(firewall.CacheAddresses)), slices.Sorted(slices.Values(addresses))) {
		t.Errorf("host firewall admits DNS to %v, cache binds %v", firewall.CacheAddresses, addresses)
	}
	if rule := `iifname "cni0" ip saddr {{ firewall_pod_cidr }} ip daddr { {{ firewall_node_local_dns_addresses | join(', ') }} } meta l4proto { tcp, udp } th dport 53 accept`; !strings.Contains(template, rule) {
		t.Error("host firewall drops pod DNS to the node-local cache, which blackholes every pod on a node running it")
	}
}

func TestNodeLocalDNSCacheMetricsReachPrometheus(t *testing.T) {
	cache := loadNodeLocalDNS(t)
	port := cache.port(t, "metrics")

	firewall, template := loadHostFirewall(t)
	if firewall.CacheMetricsPort != port {
		t.Errorf("host firewall admits metrics on %d, cache serves %d", firewall.CacheMetricsPort, port)
	}
	for _, rule := range []string{
		`iifname { "cni0", "flannel.1", "flannel-wg" } ip saddr {{ firewall_pod_cidr }} tcp dport { 6443, {{ firewall_node_local_dns_metrics_port }}, 10250 } accept`,
		`tcp dport { 2379, 2380, 6443, {{ firewall_node_local_dns_metrics_port }}, 10250 } accept`,
	} {
		if !strings.Contains(template, rule) {
			t.Errorf("host firewall lacks %q", rule)
		}
	}

	var monitor struct {
		Spec struct {
			SampleLimit       int `yaml:"sampleLimit"`
			NamespaceSelector struct {
				MatchNames []string `yaml:"matchNames"`
			} `yaml:"namespaceSelector"`
			Selector struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			}
			PodMetricsEndpoints []struct {
				Port            string
				HonorLabels     *bool `yaml:"honorLabels"`
				HonorTimestamps *bool `yaml:"honorTimestamps"`
			} `yaml:"podMetricsEndpoints"`
		}
	}
	found := false
	decodeDocuments(t, "platform/components/observability/node-local-dns.yaml", func(kind string, document *yaml.Node) {
		if kind == "PodMonitor" {
			found = true
			if err := document.Decode(&monitor); err != nil {
				t.Fatal(err)
			}
		}
	})
	if !found || !slices.Equal(monitor.Spec.NamespaceSelector.MatchNames, []string{cache.Cache.Metadata.Namespace}) || len(monitor.Spec.PodMetricsEndpoints) != 1 || monitor.Spec.PodMetricsEndpoints[0].Port != "metrics" {
		t.Fatalf("PodMonitor %+v does not scrape the cache metrics port", monitor.Spec)
	}
	if endpoint := monitor.Spec.PodMetricsEndpoints[0]; endpoint.HonorLabels == nil || *endpoint.HonorLabels || endpoint.HonorTimestamps == nil || *endpoint.HonorTimestamps || monitor.Spec.SampleLimit == 0 {
		t.Error("volatile workers serve their own cache metrics, so the scrape must bound samples and ignore target labels and timestamps")
	}
	for key, value := range monitor.Spec.Selector.MatchLabels {
		if cache.Cache.Spec.Template.Metadata.Labels[key] != value {
			t.Errorf("PodMonitor selects %s=%s, which cache pods lack", key, value)
		}
	}

	var egress struct {
		Spec struct{ Egress []egressRule }
	}
	decodeDocuments(t, "platform/components/observability/network.yaml", func(kind string, document *yaml.Node) {
		var header struct{ Metadata struct{ Name string } }
		if err := document.Decode(&header); err != nil {
			t.Fatal(err)
		}
		if header.Metadata.Name == "internal-observability" {
			if err := document.Decode(&egress); err != nil {
				t.Fatal(err)
			}
		}
	})
	if !slices.ContainsFunc(egress.Spec.Egress, func(rule egressRule) bool {
		return len(rule.To) == 0 && slices.Contains(rule.Ports, networkPort{Port: port, Protocol: "TCP"})
	}) {
		t.Errorf("Prometheus egress does not reach node port %d", port)
	}

	policy := parseTailnetPolicy(t, read(t, filepath.Join(root(t), "tailscale/policy.hujson")))
	scrapers := []string{"tag:platform-control", "tag:platform-worker"}
	kubeletTargets := map[string]bool{}
	for _, rule := range policy.ACLs {
		if !slices.ContainsFunc(scrapers, func(source string) bool { return slices.Contains(rule.Source, source) }) {
			continue
		}
		for _, destination := range rule.Destination {
			host, ports, err := splitTailnetEndpoint(destination)
			if err != nil {
				t.Fatal(err)
			}
			if matched, err := tailnetPortMatches(ports, 10250); err != nil {
				t.Fatal(err)
			} else if matched {
				kubeletTargets[host] = true
			}
		}
	}
	if len(kubeletTargets) == 0 {
		t.Fatal("tailnet policy grants no kubelet scrapes")
	}
	for _, source := range scrapers {
		for host := range kubeletTargets {
			if allowed, err := policy.allows(source, "tcp", fmt.Sprintf("%s:%d", host, port)); err != nil || !allowed {
				t.Errorf("%s cannot scrape the cache on %s:%d: %v", source, host, port, err)
			}
		}
	}
}

func TestNodeLocalDNSCacheToleratesEveryDeclaredNodeTaint(t *testing.T) {
	repository := root(t)
	cache := loadNodeLocalDNS(t)
	config := string(read(t, filepath.Join(repository, "ansible/roles/k3s/templates/config.yaml.j2")))
	match := regexp.MustCompile(`(?m)^node-taint: \["([^"]+)"\]`).FindStringSubmatch(config)
	if match == nil {
		t.Fatal("control-plane taint not found in the k3s configuration")
	}
	taints := []string{match[1]}
	var inventory struct {
		All map[string]any `yaml:"all"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/inventory/production.yml")), &inventory); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]map[string]any{}
	inventoryHosts(inventory.All, hosts)
	for _, variables := range hosts {
		declared, _ := variables["k3s_node_taints"].([]any)
		for _, taint := range declared {
			taints = append(taints, fmt.Sprint(taint))
		}
	}
	for _, taint := range taints {
		key, rest, _ := strings.Cut(taint, "=")
		value, effect, _ := strings.Cut(rest, ":")
		if !slices.ContainsFunc(cache.Cache.Spec.Template.Spec.Tolerations, func(toleration struct{ Key, Operator, Value, Effect string }) bool {
			return toleration.Key == key && toleration.Effect == effect && (toleration.Operator == "Exists" || (toleration.Operator == "Equal" && toleration.Value == value))
		}) {
			t.Errorf("cache does not tolerate %s, so pods on those nodes keep the cross-node DNS hop", taint)
		}
	}
	for _, toleration := range cache.Cache.Spec.Template.Spec.Tolerations {
		if toleration.Key == "" {
			t.Error("cache tolerates every taint")
		}
	}
}

func TestNodeLocalDNSCacheImageIsPinnedAndRecorded(t *testing.T) {
	image := loadNodeLocalDNS(t).Cache.Spec.Template.Spec.Containers[0].Image
	var versions struct{ Images map[string]string }
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), "platform/versions.yaml")), &versions); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^registry\.k8s\.io/dns/k8s-dns-node-cache:[0-9.]+@sha256:[a-f0-9]{64}$`).MatchString(image) || versions.Images["node-local-dns"] != image {
		t.Errorf("cache image %q is not the digest-pinned release recorded in platform/versions.yaml (%q)", image, versions.Images["node-local-dns"])
	}
}

func TestRenovateUpdatesTheNodeLocalDNSImageEverywhereItIsPinned(t *testing.T) {
	repository := root(t)
	image := loadNodeLocalDNS(t).Cache.Spec.Template.Spec.Containers[0].Image
	var config struct {
		CustomManagers []struct {
			ManagerFilePatterns []string `json:"managerFilePatterns"`
			MatchStrings        []string `json:"matchStrings"`
			DatasourceTemplate  string   `json:"datasourceTemplate"`
		} `json:"customManagers"`
	}
	if err := json.Unmarshal(read(t, filepath.Join(repository, "renovate.json")), &config); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"platform/components/dns/node-local-dns.yaml", "platform/versions.yaml"} {
		content := string(read(t, filepath.Join(repository, path)))
		tracked := 0
		for _, manager := range config.CustomManagers {
			if manager.DatasourceTemplate != "docker" || !slices.ContainsFunc(manager.ManagerFilePatterns, func(pattern string) bool {
				return regexp.MustCompile(strings.Trim(pattern, "/")).MatchString(path)
			}) {
				continue
			}
			for _, matchString := range manager.MatchStrings {
				expression := regexp.MustCompile(matchString)
				for _, match := range expression.FindAllStringSubmatch(content, -1) {
					groups := map[string]string{}
					for index, name := range expression.SubexpNames() {
						groups[name] = match[index]
					}
					if groups["depName"]+":"+groups["currentValue"]+"@"+groups["currentDigest"] == image {
						tracked++
					}
				}
			}
		}
		if pinned := strings.Count(content, image); tracked == 0 || tracked != pinned {
			t.Errorf("%s pins %s %d times; Renovate tracks %d of them with their digest", path, image, pinned, tracked)
		}
	}
}
