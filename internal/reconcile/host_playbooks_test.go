package reconcile

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
)

func writeAnsibleTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, data := range files {
		path = filepath.Join(root, "ansible", path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestHostPlaybooksFollowRoleClosures(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, test := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{"cluster role", []string{"ansible/roles/k3s/tasks/main.yml"}, []string{"k3s.yml", "volatile.yml"}},
		{"cluster playbook", []string{"ansible/k3s.yml"}, []string{"k3s.yml"}},
		{"secret decryption", []string{"ansible/roles/host_secrets/tasks/main.yml"}, []string{"control-backup.yml", "external.yml"}},
		{"shared transport role", []string{"ansible/roles/tailscale/tasks/install.yml"}, []string{"transport.yml", "external.yml", "volatile.yml"}},
		{"role used through imports", []string{"ansible/roles/infra_binary/tasks/main.yml"}, []string{"control-backup.yml", "infra-cli.yml", "build-runners.yml", "external.yml"}},
		{"monitor role", []string{"ansible/roles/gatus/tasks/main.yml"}, []string{"external.yml"}},
		{"runner fleet", []string{"build/runners.json"}, []string{"build-runners.yml"}},
		{"CLI release", []string{"build/cli-release.json"}, []string{"build-runners.yml", "external.yml"}},
		{"cluster role and monitor", []string{"ansible/roles/k3s/tasks/main.yml", "ansible/roles/gatus/tasks/main.yml", "docs/runbook.md", "platform/components/observability/monitoring.yaml"}, []string{"k3s.yml", "external.yml", "volatile.yml"}},
		{"inventory", []string{"ansible/roles/k3s/tasks/main.yml", "ansible/inventory/production.yml"}, nil},
		{"configuration", []string{"ansible/ansible.cfg"}, nil},
		{"strategy plugin", []string{"ansible/plugins/strategy/mitogen_linear.py"}, nil},
		{"shared file", []string{"ansible/files/reconciliation.pub"}, nil},
		{"fact gathering", []string{"ansible/facts.yml"}, nil},
		{"convergence order", []string{"ansible/reconcile.yml"}, nil},
		{"playbook group", []string{"ansible/site.yml"}, nil},
		{"verification playbook", []string{"ansible/verify.yml"}, nil},
		{"infrastructure", []string{"ansible/roles/k3s/tasks/main.yml", "tofu/main.tf"}, nil},
		{"host secret", []string{"secrets/tailscale.yaml"}, nil},
		{"shared input", []string{"ansible/roles/k3s/tasks/main.yml", ".sops.yaml"}, nil},
		{"unclassified input", []string{"ansible/roles/k3s/tasks/main.yml", "new-input"}, nil},
		{"undeclared role", []string{"ansible/roles/retired/tasks/main.yml"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := scopedHostPlaybooks(root, test.paths)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("host playbooks %q, want %q", got, test.want)
			}
		})
	}
}

func TestRunnerPlaybookConvergesLast(t *testing.T) {
	graph, err := loadHostPlaybookGraph(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	converged := convergencePlaybooks(Selection{Ansible: true, HostScope: HostScopeFull, HostPlaybooks: graph.order})
	if converged[0] != factsPlaybook || converged[len(converged)-1] != runnerPlaybook {
		t.Fatalf("runner repairs append %s, but convergence order is %q", runnerPlaybook, converged)
	}
}

func TestHostPlaybookGraphFollowsNestedRoleReferences(t *testing.T) {
	root := writeAnsibleTree(t, map[string]string{
		"reconcile.yml":                 "- import_playbook: facts.yml\n- import_playbook: group.yml\n",
		"facts.yml":                     "- name: Gather\n  hosts: all\n",
		"group.yml":                     "- import_playbook: first.yml\n- import_playbook: second.yml\n",
		"first.yml":                     "- name: First\n  hosts: all\n  roles:\n  - role: base\n",
		"second.yml":                    "- name: Second\n  hosts: all\n  tasks:\n  - name: Guarded\n    block:\n    - name: Apply\n      ansible.builtin.include_role:\n        name: guarded\n    rescue:\n    - name: Recover\n      ansible.builtin.import_role:\n        name: recovery\n        tasks_from: undo.yml\n",
		"external.yml":                  "- name: Monitor\n  hosts: all\n",
		"volatile.yml":                  "- name: Volatile\n  hosts: all\n  roles: [leaf]\n",
		"roles/base/meta/main.yml":      "dependencies:\n- role: shared\n",
		"roles/base/tasks/main.yml":     "- name: Nested\n  ansible.builtin.include_tasks: nested.yml\n",
		"roles/base/tasks/nested.yml":   "- name: Leaf\n  ansible.builtin.import_role:\n    name: leaf\n",
		"roles/shared/tasks/main.yml":   "- name: Shared\n  ansible.builtin.debug:\n",
		"roles/leaf/tasks/main.yml":     "- name: Leaf\n  ansible.builtin.debug:\n",
		"roles/leaf/handlers/main.yml":  "- name: Handler\n  ansible.builtin.import_role:\n    name: handled\n",
		"roles/handled/tasks/main.yml":  "- name: Handled\n  ansible.builtin.debug:\n",
		"roles/guarded/tasks/main.yml":  "- name: Guarded\n  ansible.builtin.debug:\n",
		"roles/recovery/tasks/undo.yml": "- name: Undo\n  ansible.builtin.debug:\n",
	})
	for _, test := range []struct {
		path string
		want []string
	}{
		{"ansible/roles/shared/tasks/main.yml", []string{"first.yml"}},
		{"ansible/roles/handled/tasks/main.yml", []string{"first.yml", "volatile.yml"}},
		{"ansible/roles/recovery/tasks/undo.yml", []string{"second.yml"}},
		{"ansible/group.yml", nil},
		{"ansible/second.yml", []string{"second.yml"}},
	} {
		got, err := scopedHostPlaybooks(root, []string{test.path})
		if err != nil || !slices.Equal(got, test.want) {
			t.Errorf("%s selected %q (%v), want %q", test.path, got, err, test.want)
		}
	}
}

func TestHostPlaybookGraphRejectsUnresolvableReferences(t *testing.T) {
	valid := map[string]string{
		"reconcile.yml":             "- import_playbook: facts.yml\n- import_playbook: first.yml\n",
		"facts.yml":                 "- name: Gather\n  hosts: all\n",
		"first.yml":                 "- name: First\n  hosts: all\n  roles: [base]\n",
		"external.yml":              "- name: Monitor\n  hosts: all\n",
		"volatile.yml":              "- name: Volatile\n  hosts: all\n",
		"roles/base/tasks/main.yml": "- name: Base\n  ansible.builtin.debug:\n",
	}
	if _, err := loadHostPlaybookGraph(writeAnsibleTree(t, valid)); err != nil {
		t.Fatalf("valid tree rejected: %v", err)
	}
	for name, change := range map[string]map[string]string{
		"templated role":          {"first.yml": "- name: First\n  hosts: all\n  tasks:\n  - name: Dynamic\n    ansible.builtin.include_role:\n      name: \"{{ selected }}\"\n"},
		"undeclared role":         {"first.yml": "- name: First\n  hosts: all\n  roles: [missing]\n"},
		"escaping task include":   {"roles/base/tasks/main.yml": "- name: Other\n  ansible.builtin.include_tasks: ../../other/tasks/main.yml\n"},
		"templated task include":  {"roles/base/tasks/main.yml": "- name: Other\n  ansible.builtin.include_tasks:\n    file: \"{{ selected }}.yml\"\n"},
		"playbook task include":   {"first.yml": "- name: First\n  hosts: all\n  tasks:\n  - name: Shared\n    ansible.builtin.import_tasks: shared.yml\n"},
		"inline convergence":      {"reconcile.yml": "- import_playbook: facts.yml\n- name: Inline\n  hosts: all\n"},
		"facts after convergence": {"reconcile.yml": "- import_playbook: first.yml\n- import_playbook: facts.yml\n"},
		"repeated convergence":    {"reconcile.yml": "- import_playbook: facts.yml\n- import_playbook: first.yml\n- import_playbook: first.yml\n"},
		"conditional import":      {"reconcile.yml": "- import_playbook: facts.yml\n- import_playbook: first.yml\n  when: enabled\n"},
		"nested import":           {"external.yml": "- import_playbook: first.yml\n"},
		"import cycle":            {"reconcile.yml": "- import_playbook: facts.yml\n- import_playbook: reconcile.yml\n"},
		"unsupported roles":       {"first.yml": "- name: First\n  hosts: all\n  roles: base\n"},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{}
			for path, data := range valid {
				files[path] = data
			}
			for path, data := range change {
				files[path] = data
			}
			root := writeAnsibleTree(t, files)
			if _, err := loadHostPlaybookGraph(root); err == nil {
				t.Fatal("unresolvable reference accepted")
			}
			if _, err := scopedHostPlaybooks(root, []string{"ansible/roles/base/tasks/main.yml"}); err == nil {
				t.Fatal("unresolvable reference scoped host convergence")
			}
		})
	}
}

var (
	playbookLookupPattern = regexp.MustCompile(`playbook_dir \+ '/\.\./([^']+)'`)
	untrackedReadPattern  = regexp.MustCompile(`\b(role_path|inventory_dir)\b|roles/`)
)

func TestHostPlaybookInputsOutsideAnsibleRouteToTheirReaders(t *testing.T) {
	root := filepath.Join("..", "..")
	graph, err := loadHostPlaybookGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	readers := map[string][]string{}
	err = filepath.WalkDir(filepath.Join(root, "ansible"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		text := string(data)
		if reference := untrackedReadPattern.FindString(text); reference != "" {
			t.Errorf("%s reads through %s, which host playbook scoping does not follow", relative, reference)
		}
		if strings.Contains(text, "ansible_parent_role_paths") && relative != "ansible/roles/host_secrets/tasks/install.yml" {
			t.Errorf("%s reads another role's files", relative)
		}
		for _, match := range playbookLookupPattern.FindAllStringSubmatch(text, -1) {
			if strings.HasPrefix(match[1], "ansible/") {
				t.Errorf("%s reads Ansible input %s outside its role", relative, match[1])
			}
			readers[match[1]] = append(readers[match[1]], graph.users(filepath.ToSlash(relative))...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(readers) == 0 {
		t.Fatal("no external host inputs found")
	}
	for input, playbooks := range readers {
		selected, err := scopedHostPlaybooks(root, []string{input})
		if err != nil {
			t.Fatal(err)
		}
		if selected == nil {
			continue
		}
		for _, playbook := range playbooks {
			if !slices.Contains(selected, playbook) {
				t.Errorf("%s changes select %q without %s, which reads it", input, selected, playbook)
			}
		}
	}
}

func TestSelectionScopesHostPlaybooks(t *testing.T) {
	for _, test := range []struct {
		name  string
		paths []string
		full  bool
		want  []string
	}{
		{name: "cluster role", paths: []string{"ansible/roles/k3s/tasks/main.yml", "internal/reconcile/volatile_test.go"}, want: []string{"k3s.yml", "volatile.yml"}},
		{name: "inventory", paths: []string{"ansible/roles/k3s/tasks/main.yml", "ansible/inventory/production.yml"}},
		{name: "explicit full", paths: []string{"ansible/roles/k3s/tasks/main.yml"}, full: true},
		{name: "runner scope", paths: []string{"build/runners.json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := &Commands{Runner: ci.Runner{Dir: filepath.Join("..", ".."), Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				if options.Args[0] == "diff" {
					return process.Result{Stdout: []byte(strings.Join(test.paths, "\n") + "\n")}, nil
				}
				return process.Result{}, nil
			}}}
			selected, err := commands.Select(context.Background(), strings.Repeat("a", 40), test.full)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(selected.HostPlaybooks, test.want) {
				t.Fatalf("host playbooks %q, want %q: %+v", selected.HostPlaybooks, test.want, selected)
			}
			if test.want != nil && (effectiveHostScope(selected) != HostScopeFull || !slices.Contains(selected.Reasons, "host playbooks: "+strings.Join(test.want, ", "))) {
				t.Fatalf("scoped selection unexplained: %+v", selected)
			}
		})
	}
}
