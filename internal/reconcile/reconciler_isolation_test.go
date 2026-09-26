package reconcile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const reconcilerGroup = "reconcilers"

type inventoryGroup struct {
	Hosts    map[string]any            `yaml:"hosts"`
	Children map[string]inventoryGroup `yaml:"children"`
}

func inventoryGroups(t *testing.T, root string) (map[string][]string, map[string]bool) {
	t.Helper()
	var inventory struct {
		All inventoryGroup `yaml:"all"`
	}
	data, err := os.ReadFile(filepath.Join(root, "inventory", "production.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	descendants, hosts := map[string][]string{}, map[string]bool{}
	var visit func(name string, group inventoryGroup) []string
	visit = func(name string, group inventoryGroup) []string {
		below := []string{}
		for host := range group.Hosts {
			hosts[host] = true
		}
		for child, members := range group.Children {
			below = append(append(below, child), visit(child, members)...)
		}
		descendants[name] = append(append([]string{}, descendants[name]...), below...)
		return below
	}
	visit("all", inventory.All)
	return descendants, hosts
}

type playTarget struct {
	Hosts          string `yaml:"hosts"`
	ImportPlaybook string `yaml:"import_playbook"`
}

func playPatterns(t *testing.T, root, file string) []string {
	t.Helper()
	var patterns []string
	for _, play := range loadAnsible[[]playTarget](t, root, file) {
		if play.ImportPlaybook != "" {
			patterns = append(patterns, playPatterns(t, root, play.ImportPlaybook)...)
			continue
		}
		patterns = append(patterns, play.Hosts)
	}
	return patterns
}

func TestOnlyTheReconcilerPlaybookSelectsTheReconcilerGroup(t *testing.T) {
	root := filepath.Join("..", "..", "ansible")
	descendants, hosts := inventoryGroups(t, root)
	if _, declared := descendants[reconcilerGroup]; !declared || !slices.Contains(descendants["all"], reconcilerGroup) {
		t.Fatalf("inventory does not declare the %s group", reconcilerGroup)
	}
	for group, below := range descendants {
		if group != "all" && slices.Contains(below, reconcilerGroup) {
			t.Errorf("%s is nested in %s, which fleet plays select", reconcilerGroup, group)
		}
	}
	playbooks, err := filepath.Glob(filepath.Join(root, "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wildcard := regexp.MustCompile(`[*?~\[\]{}]`)
	for _, playbook := range playbooks {
		name := filepath.Base(playbook)
		for _, pattern := range playPatterns(t, root, name) {
			if name == "reconciler.yml" {
				if pattern != reconcilerGroup {
					t.Errorf("reconciler.yml selects %q, want only %s", pattern, reconcilerGroup)
				}
				continue
			}
			for _, token := range strings.FieldsFunc(pattern, func(r rune) bool { return r == ':' || r == ',' }) {
				token = strings.TrimLeft(token, "!&")
				switch {
				case wildcard.MatchString(token) || token == "all" || token == "ungrouped" || token == reconcilerGroup:
					t.Errorf("%s selects %q, which can include the reconciler", name, pattern)
				case slices.Contains(descendants[token], reconcilerGroup):
					t.Errorf("%s selects %s, which contains the reconciler", name, token)
				case descendants[token] == nil && !hosts[token] && !(name == "tailscale-bootstrap.yml" && token == "tailscale_bootstrap"):
					t.Errorf("%s selects undeclared %q", name, token)
				}
			}
		}
	}
}

func TestFleetPlaybooksNeverListADeclaredReconciler(t *testing.T) {
	playbook, err := exec.LookPath("ansible-playbook")
	if err != nil {
		if playbook, err = filepath.Abs(filepath.Join("..", "..", ".venv", "bin", "ansible-playbook")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(playbook); err != nil {
			t.Skip("requires ansible-playbook")
		}
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "ansible"))
	if err != nil {
		t.Fatal(err)
	}
	inventory := filepath.Join(root, "inventory", "production.yml")
	listing := exec.Command(filepath.Join(filepath.Dir(playbook), "ansible-inventory"), "-i", inventory, "--list")
	listing.Dir, listing.Env = root, append(os.Environ(), "ANSIBLE_CONFIG="+filepath.Join(root, "ansible.cfg"))
	groups, err := listing.Output()
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal(groups, &parsed); err != nil {
		t.Fatal(err)
	}
	reconcilers := parsed[reconcilerGroup].Hosts
	if len(reconcilers) == 0 {
		t.Fatal("inventory declares no reconciler host")
	}
	playbooks, err := filepath.Glob(filepath.Join(root, "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range playbooks {
		command := exec.Command(playbook, "-i", inventory, "--list-hosts", filepath.Base(path))
		command.Dir, command.Env = root, append(os.Environ(), "ANSIBLE_CONFIG="+filepath.Join(root, "ansible.cfg"))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", filepath.Base(path), err, output)
		}
		for _, host := range reconcilers {
			listed := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(host) + `$`).Match(output)
			if listed != (filepath.Base(path) == "reconciler.yml") {
				t.Errorf("%s lists reconciler %s: %t\n%s", filepath.Base(path), host, listed, output)
			}
		}
	}
}
