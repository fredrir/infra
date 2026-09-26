package reconcile

import (
	"bytes"
	"encoding/json"
	"errors"
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
	playbooks, err := filepath.Glob(filepath.Join(root, "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(playbooks))
	for index, path := range playbooks {
		names[index] = filepath.Base(path)
	}
	inventory := filepath.Join(root, "inventory", "production.yml")
	inventoryList := exec.Command(filepath.Join(filepath.Dir(playbook), "ansible-inventory"), "-i", inventory, "--list")
	hosts := exec.Command(playbook, append([]string{"-i", inventory, "--list-hosts"}, names...)...)
	var groups, listing, inventoryErrors, listingErrors bytes.Buffer
	inventoryList.Stdout, inventoryList.Stderr, hosts.Stdout, hosts.Stderr = &groups, &inventoryErrors, &listing, &listingErrors
	for _, command := range []*exec.Cmd{inventoryList, hosts} {
		command.Dir, command.Env = root, append(os.Environ(), "ANSIBLE_CONFIG="+filepath.Join(root, "ansible.cfg"))
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(inventoryList.Wait(), hosts.Wait()); err != nil {
		t.Fatalf("%v\n%s%s", err, inventoryErrors.String(), listingErrors.String())
	}
	var parsed map[string]struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal(groups.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	reconcilers := parsed[reconcilerGroup].Hosts
	if len(reconcilers) == 0 {
		t.Fatal("inventory declares no reconciler host")
	}
	sections := playbookSections(listing.String())
	for _, name := range names {
		section, found := sections[name]
		if !found {
			t.Fatalf("%s is missing from the host listing\n%s", name, listing.String())
		}
		for _, host := range reconcilers {
			listed := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(host) + `$`).MatchString(section)
			if listed != (name == "reconciler.yml") {
				t.Errorf("%s lists reconciler %s: %t\n%s", name, host, listed, section)
			}
		}
	}
}

func playbookSections(listing string) map[string]string {
	headers := regexp.MustCompile(`(?m)^playbook: (.+)$`).FindAllStringSubmatchIndex(listing, -1)
	sections := make(map[string]string, len(headers))
	for index, header := range headers {
		end := len(listing)
		if index+1 < len(headers) {
			end = headers[index+1][0]
		}
		sections[listing[header[2]:header[3]]] = listing[header[1]:end]
	}
	return sections
}
