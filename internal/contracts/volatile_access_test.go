package contracts

import (
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestVolatileHostsAreReachedWithoutAgentOrJumpAndWithStrictHostKeys(t *testing.T) {
	var inventory struct {
		All struct {
			Children map[string]yaml.Node `yaml:"children"`
		} `yaml:"all"`
	}
	data := read(t, filepath.Join(root(t), "ansible/inventory/production.yml"))
	if err := yaml.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	var group struct {
		Hosts map[string]any `yaml:"hosts"`
	}
	volatile := inventory.All.Children["volatile"]
	if err := volatile.Decode(&group); err != nil || len(group.Hosts) == 0 {
		t.Fatalf("no volatile hosts: %v", err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for host := range group.Hosts {
		arguments, found := hostVariable(document, host, "ansible_ssh_common_args")
		if !found {
			t.Errorf("%s declares no SSH arguments", host)
			continue
		}
		fields := strings.Fields(arguments)
		for _, required := range []string{"ForwardAgent=no", "StrictHostKeyChecking=yes", "HostKeyAlias=" + host} {
			if !containsOption(fields, required) {
				t.Errorf("%s SSH arguments %q lack %s", host, arguments, required)
			}
		}
		if strings.Contains(arguments, "ProxyJump") || strings.Contains(arguments, "ProxyCommand") {
			t.Errorf("%s is reached through another host: %q", host, arguments)
		}
	}
}

func containsOption(fields []string, option string) bool {
	for index, field := range fields {
		if field == "-o" && index+1 < len(fields) && fields[index+1] == option {
			return true
		}
	}
	return false
}

func hostVariable(node any, host, name string) (string, bool) {
	switch node := node.(type) {
	case map[string]any:
		if hosts, ok := node["hosts"].(map[string]any); ok {
			if variables, ok := hosts[host].(map[string]any); ok {
				if value, ok := variables[name].(string); ok {
					return value, true
				}
			}
		}
		for _, child := range node {
			if value, ok := hostVariable(child, host, name); ok {
				return value, true
			}
		}
	}
	return "", false
}
