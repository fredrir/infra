package contracts

import (
	"bufio"
	"bytes"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestProvisionedValuesAreSet(t *testing.T) {
	repository := root(t)
	if key := strings.TrimSpace(string(read(t, filepath.Join(repository, "ansible/files/reconciliation.pub")))); !regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/=]+( .*)?$`).MatchString(key) {
		t.Errorf("ansible/files/reconciliation.pub holds %q, not fredrir-11's public key", key)
	}
	var defaults struct {
		Apply struct {
			Runner struct {
				InstallationID *int64 `yaml:"installation_id"`
			} `yaml:"runner"`
		} `yaml:"reconciler_apply"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/roles/reconciler/defaults/main.yml")), &defaults); err != nil {
		t.Fatal(err)
	}
	if id := defaults.Apply.Runner.InstallationID; id == nil || *id <= 0 {
		t.Error("reconciler_apply.runner.installation_id is not the runner App's installation ID")
	}
	known := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(read(t, filepath.Join(repository, "ansible/files/reconciliation_known_hosts"))))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for _, name := range strings.Split(fields[0], ",") {
			known[name] = true
		}
	}
	var missing []string
	for _, name := range sshTargets(t, read(t, filepath.Join(repository, "ansible/inventory/production.yml"))) {
		if !known[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("ansible/files/reconciliation_known_hosts has no verified key for %v", missing)
	}
}

func sshTargets(t *testing.T, inventory []byte) []string {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(inventory, &document); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]map[string]any{}
	var collect func(any)
	collect = func(node any) {
		group, ok := node.(map[string]any)
		if !ok {
			return
		}
		if members, ok := group["hosts"].(map[string]any); ok {
			for name, variables := range members {
				if hosts[name] == nil {
					hosts[name] = map[string]any{}
				}
				if values, ok := variables.(map[string]any); ok {
					for key, value := range values {
						hosts[name][key] = value
					}
				}
			}
		}
		if children, ok := group["children"].(map[string]any); ok {
			for name, child := range children {
				if name != "reconcilers" {
					collect(child)
				}
			}
		}
	}
	collect(document["all"])
	alias := regexp.MustCompile(`HostKeyAlias=(\S+)`)
	jump := regexp.MustCompile(`ProxyJump=\S+@\{\{ hostvars\['([^']+)'\]\.tailscale_ip`)
	var targets []string
	for name, variables := range hosts {
		arguments, _ := variables["ansible_ssh_common_args"].(string)
		if match := alias.FindStringSubmatch(arguments); match != nil {
			targets = append(targets, match[1])
		} else if address, ok := variables["tailscale_ip"].(string); ok {
			targets = append(targets, address)
		} else if _, planned := variables["status"]; !planned {
			t.Errorf("host %s has neither a host key alias nor a tailnet address", name)
		}
		if match := jump.FindStringSubmatch(arguments); match != nil {
			if address, ok := hosts[match[1]]["tailscale_ip"].(string); ok {
				targets = append(targets, address)
			}
		}
	}
	slices.Sort(targets)
	return slices.Compact(targets)
}
