package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tailscale/hujson"
)

type tailnetRule struct {
	Action      string   `json:"action"`
	Source      []string `json:"src"`
	Proto       string   `json:"proto"`
	Destination []string `json:"dst"`
}

type tailnetTest struct {
	Source string   `json:"src"`
	Proto  string   `json:"proto"`
	Accept []string `json:"accept"`
	Deny   []string `json:"deny"`
}

type tailnetPolicy struct {
	Hosts map[string]string `json:"hosts"`
	ACLs  []tailnetRule     `json:"acls"`
	Tests []tailnetTest     `json:"tests"`
}

func parseTailnetPolicy(t *testing.T, data []byte) tailnetPolicy {
	t.Helper()
	standard, err := hujson.Standardize(data)
	if err != nil {
		t.Fatal(err)
	}
	var policy tailnetPolicy
	if err := json.NewDecoder(bytes.NewReader(standard)).Decode(&policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

func (p tailnetPolicy) sameEndpoint(a, b string) bool {
	return a == b || p.Hosts[a] == b || p.Hosts[b] == a || (p.Hosts[a] != "" && p.Hosts[a] == p.Hosts[b])
}

func (p tailnetPolicy) sourceMatches(rule, source string) bool {
	switch rule {
	case "*":
		return true
	case "autogroup:member":
		return strings.Contains(source, "@")
	}
	return p.sameEndpoint(rule, source)
}

func splitTailnetEndpoint(endpoint string) (string, string, error) {
	index := strings.LastIndex(endpoint, ":")
	if index <= 0 || index == len(endpoint)-1 {
		return "", "", fmt.Errorf("endpoint %q has no port", endpoint)
	}
	return endpoint[:index], endpoint[index+1:], nil
}

func tailnetPortMatches(ports string, port int) (bool, error) {
	for _, item := range strings.Split(ports, ",") {
		if item == "*" {
			return true, nil
		}
		low, high, isRange := strings.Cut(item, "-")
		if !isRange {
			high = low
		}
		first, err := strconv.Atoi(low)
		if err != nil {
			return false, fmt.Errorf("port %q: %w", item, err)
		}
		last, err := strconv.Atoi(high)
		if err != nil {
			return false, fmt.Errorf("port %q: %w", item, err)
		}
		if first <= port && port <= last {
			return true, nil
		}
	}
	return false, nil
}

func (p tailnetPolicy) allows(source, proto, destination string) (bool, error) {
	host, portText, err := splitTailnetEndpoint(destination)
	if err != nil {
		return false, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return false, fmt.Errorf("test destination %q: %w", destination, err)
	}
	for _, rule := range p.ACLs {
		if rule.Action != "accept" || (rule.Proto != "" && rule.Proto != proto) || !slices.ContainsFunc(rule.Source, func(candidate string) bool { return p.sourceMatches(candidate, source) }) {
			continue
		}
		for _, target := range rule.Destination {
			targetHost, ports, err := splitTailnetEndpoint(target)
			if err != nil {
				return false, err
			}
			if targetHost == "autogroup:self" || !p.sameEndpoint(targetHost, host) {
				continue
			}
			if matched, err := tailnetPortMatches(ports, port); err != nil || matched {
				return matched, err
			}
		}
	}
	return false, nil
}

func (p tailnetPolicy) failures() []string {
	var failures []string
	for _, test := range p.Tests {
		proto := test.Proto
		if proto == "" {
			proto = "tcp"
		}
		for _, expectation := range []struct {
			destinations []string
			want         bool
		}{{test.Accept, true}, {test.Deny, false}} {
			for _, destination := range expectation.destinations {
				allowed, err := p.allows(test.Source, proto, destination)
				switch {
				case err != nil:
					failures = append(failures, err.Error())
				case allowed != expectation.want:
					failures = append(failures, fmt.Sprintf("%s %s → %s: allowed=%t, want %t", test.Source, proto, destination, allowed, expectation.want))
				}
			}
		}
	}
	return failures
}

func TestTailnetPolicyTestsHoldUnderItsRules(t *testing.T) {
	policy := parseTailnetPolicy(t, read(t, filepath.Join(root(t), "tailscale/policy.hujson")))
	if len(policy.Tests) == 0 {
		t.Fatal("tailnet policy declares no tests")
	}
	for _, failure := range policy.failures() {
		t.Error(failure)
	}
}

func TestTailnetEvaluatorRejectsUngrantedExpectations(t *testing.T) {
	policy := parseTailnetPolicy(t, []byte(`{
  // Hosts and trailing commas are accepted as in the declared policy.
  "hosts": {"admin": "100.64.0.1"},
  "acls": [{"action": "accept", "src": ["admin"], "proto": "tcp", "dst": ["tag:target:22"]},],
  "tests": [
    {"src": "100.64.0.1", "accept": ["tag:target:22"]},
    {"src": "admin", "accept": ["tag:target:443"]},
    {"src": "admin", "deny": ["tag:target:22"]},
    {"src": "admin", "proto": "udp", "accept": ["tag:target:22"]},
    {"src": "tag:other", "accept": ["tag:target:22"]},
  ],
}`))
	want := []string{
		"admin tcp → tag:target:443: allowed=false, want true",
		"admin tcp → tag:target:22: allowed=true, want false",
		"admin udp → tag:target:22: allowed=false, want true",
		"tag:other tcp → tag:target:22: allowed=false, want true",
	}
	if got := policy.failures(); !slices.Equal(got, want) {
		t.Errorf("failures = %q, want %q", got, want)
	}
}

func TestReconcilerReachesOnlyTheAPIAndMonitor(t *testing.T) {
	const reconciler = "tag:infra-reconciler"
	policy := parseTailnetPolicy(t, read(t, filepath.Join(root(t), "tailscale/policy.hujson")))
	var outbound, inbound []string
	for _, rule := range policy.ACLs {
		for _, destination := range rule.Destination {
			host, _, err := splitTailnetEndpoint(destination)
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range rule.Source {
				grant := fmt.Sprintf("%s %s → %s", source, rule.Proto, destination)
				if source == reconciler || source == "*" {
					outbound = append(outbound, grant)
				}
				if host == reconciler || host == "*" {
					inbound = append(inbound, grant)
				}
			}
		}
	}
	if want := []string{reconciler + " tcp → tag:platform-control:6443", reconciler + " tcp → platform-monitor:8080"}; !slices.Equal(outbound, want) {
		t.Errorf("reconciler grants = %q, want %q", outbound, want)
	}
	if want := []string{"macie tcp → " + reconciler + ":22", "archie tcp → " + reconciler + ":22"}; !slices.Equal(inbound, want) {
		t.Errorf("grants to the reconciler = %q, want %q", inbound, want)
	}
}
