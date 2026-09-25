package reconcile

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type inventory struct {
	groups map[string][]string
	vars   map[string]map[string]any
}

func loadInventory(t *testing.T, root string) inventory {
	t.Helper()
	document := loadAnsible[map[string]any](t, root, "inventory/production.yml")
	loaded := inventory{groups: map[string][]string{}, vars: map[string]map[string]any{}}
	var visit func(string, map[string]any) []string
	visit = func(name string, group map[string]any) []string {
		var members []string
		hosts, _ := group["hosts"].(map[string]any)
		for _, host := range slices.Sorted(maps.Keys(hosts)) {
			members = append(members, host)
			if loaded.vars[host] == nil {
				loaded.vars[host] = map[string]any{}
			}
			if declared, ok := hosts[host].(map[string]any); ok {
				maps.Copy(loaded.vars[host], declared)
			}
		}
		children, _ := group["children"].(map[string]any)
		for _, child := range slices.Sorted(maps.Keys(children)) {
			definition, _ := children[child].(map[string]any)
			members = append(members, visit(child, definition)...)
		}
		slices.Sort(members)
		loaded.groups[name] = slices.Compact(members)
		return loaded.groups[name]
	}
	visit("all", document["all"].(map[string]any))
	return loaded
}

func (i inventory) resolve(pattern string) []string {
	var selected []string
	var excluded []string
	for _, term := range strings.FieldsFunc(pattern, func(r rune) bool { return r == ':' || r == ',' }) {
		name := strings.TrimLeft(term, "!&")
		hosts, group := i.groups[name]
		if !group {
			hosts = []string{name}
		}
		if strings.HasPrefix(term, "!") {
			excluded = append(excluded, hosts...)
		} else {
			selected = append(selected, hosts...)
		}
	}
	return slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(selected))), func(host string) bool { return slices.Contains(excluded, host) })
}

func TestVolatileHostsStayUntrusted(t *testing.T) {
	root := filepath.Join("..", "..", "ansible")
	fleet := loadInventory(t, root)
	for _, host := range fleet.groups["volatile"] {
		vars := fleet.vars[host]
		if !slices.Contains(fleet.groups["agent"], host) {
			t.Errorf("%s is not a K3s agent", host)
		}
		for _, trusted := range []string{"server", "build_engines", "external", "infra_cli_targets", "build_vm_hosts"} {
			if slices.Contains(fleet.groups[trusted], host) {
				t.Errorf("%s holds the trusted %s role", host, trusted)
			}
		}
		arguments, _ := vars["ansible_ssh_common_args"].(string)
		if !strings.Contains(arguments, "-o ForwardAgent=no") || strings.Contains(arguments, "ProxyJump") {
			t.Errorf("%s SSH arguments %q forward the agent or jump through another host", host, arguments)
		}
		if vars["ansible_host"] != "{{ tailscale_ip }}" {
			t.Errorf("%s is reached outside its Tailnet address: %v", host, vars["ansible_host"])
		}
		labels := ansibleStrings(t, vars["k3s_node_labels"])
		if !slices.Contains(labels, "node-restriction.kubernetes.io/volatile=true") || slices.ContainsFunc(labels, func(label string) bool {
			return strings.HasPrefix(label, "node-restriction.kubernetes.io/critical=") || strings.HasPrefix(label, "node-restriction.kubernetes.io/stateful=")
		}) {
			t.Errorf("%s labels %q admit production workloads", host, labels)
		}
		if !slices.Contains(ansibleStrings(t, vars["k3s_node_taints"]), "node-restriction.kubernetes.io/volatile=true:NoSchedule") {
			t.Errorf("%s is not tainted for volatile workloads", host)
		}
		for other, otherVars := range fleet.vars {
			if arguments, _ := otherVars["ansible_ssh_common_args"].(string); other != host && strings.Contains(arguments, host) {
				t.Errorf("%s connects through volatile %s", other, host)
			}
		}
	}
}

func TestSharedPlaysExcludeVolatileHosts(t *testing.T) {
	root := filepath.Join("..", "..", "ansible")
	fleet := loadInventory(t, root)
	volatile := fleet.groups["volatile"]
	walked := 0
	var walk func(string)
	walk = func(file string) {
		for _, play := range loadAnsible[[]ansiblePlay](t, root, file) {
			if play.ImportPlaybook != "" {
				walk(play.ImportPlaybook)
				continue
			}
			walked++
			hosts := fleet.resolve(play.Hosts)
			shared := slices.ContainsFunc(hosts, func(host string) bool { return !slices.Contains(volatile, host) })
			reachesVolatile := slices.ContainsFunc(hosts, func(host string) bool { return slices.Contains(volatile, host) })
			switch {
			case shared && reachesVolatile:
				t.Errorf("%s: play %q runs volatile hosts beside the fleet", file, play.Name)
			case shared && play.IgnoreUnreachable:
				t.Errorf("%s: play %q hides unreachable fleet hosts", file, play.Name)
			case play.Hosts == "volatile" && !play.IgnoreUnreachable:
				t.Errorf("%s: play %q fails the run when a volatile host is lost", file, play.Name)
			}
		}
	}
	for _, playbook := range []string{"reconcile.yml", "external.yml", "verify.yml", "build-runners.yml", "verify-runners.yml"} {
		walk(playbook)
	}
	if walked == 0 {
		t.Fatal("no plays walked")
	}
}

func TestFleetTemplatesNeverReadVolatileHosts(t *testing.T) {
	root := filepath.Join("..", "..", "ansible")
	fleet := loadInventory(t, root)
	reference := regexp.MustCompile(`(inventory_hostname\s+(?:not\s+)?in\s+)?groups(?:\[['"]([A-Za-z0-9_]+)['"]\]|\.get\(['"]([A-Za-z0-9_]+)['"])`)
	scanned := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		relative, _ := filepath.Rel(root, path)
		for _, host := range fleet.groups["volatile"] {
			for _, quote := range []string{"'", `"`} {
				if strings.Contains(string(data), "hostvars["+quote+host+quote+"]") {
					t.Errorf("%s reads volatile %s", relative, host)
				}
			}
		}
		for _, match := range reference.FindAllStringSubmatch(string(data), -1) {
			group := match[2] + match[3]
			if match[1] == "" && slices.ContainsFunc(fleet.groups[group], func(host string) bool { return slices.Contains(fleet.groups["volatile"], host) }) {
				t.Errorf("%s iterates %s, which contains volatile hosts", relative, group)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("no Ansible files scanned")
	}
}
