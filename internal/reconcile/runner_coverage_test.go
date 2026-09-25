package reconcile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type ansiblePlay struct {
	CheckMode bool             `yaml:"check_mode"`
	Roles     []string         `yaml:"roles"`
	Tasks     []map[string]any `yaml:"tasks"`
	Handlers  []map[string]any `yaml:"handlers"`
}

type ansibleTask struct {
	Key       string
	Module    string
	CheckMode bool
	Notify    []string
}

func loadAnsible[T any](t *testing.T, root, file string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := yaml.Unmarshal(data, &value); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return value
}

func ansibleStrings(t *testing.T, value any) []string {
	t.Helper()
	switch value := value.(type) {
	case nil:
		return nil
	case string:
		return []string{value}
	case []any:
		var values []string
		for _, item := range value {
			values = append(values, ansibleStrings(t, item)...)
		}
		return values
	default:
		t.Fatalf("unexpected Ansible list %v", value)
		return nil
	}
}

func walkAnsiblePlays(t *testing.T, root, file string, visit func(ansibleTask)) []ansiblePlay {
	t.Helper()
	plays := loadAnsible[[]ansiblePlay](t, root, file)
	for _, play := range plays {
		for _, role := range play.Roles {
			walkAnsibleFile(t, root, filepath.Join("roles", role, "tasks", "main.yml"), play.CheckMode, nil, visit)
		}
		walkAnsibleTasks(t, root, file, play.Tasks, play.CheckMode, nil, visit)
	}
	return plays
}

func walkAnsibleFile(t *testing.T, root, file string, checkMode bool, notify []string, visit func(ansibleTask)) {
	t.Helper()
	walkAnsibleTasks(t, root, file, loadAnsible[[]map[string]any](t, root, file), checkMode, notify, visit)
}

func walkAnsibleTasks(t *testing.T, root, file string, tasks []map[string]any, inheritedCheckMode bool, inheritedNotify []string, visit func(ansibleTask)) {
	t.Helper()
	for _, task := range tasks {
		name, _ := task["name"].(string)
		checkMode, notify := inheritedCheckMode, inheritedNotify
		if value, ok := task["check_mode"]; ok {
			if checkMode, ok = value.(bool); !ok {
				t.Fatalf("%s: task %q has non-literal check_mode %v", file, name, value)
			}
		}
		if value, ok := task["notify"]; ok {
			notify = ansibleStrings(t, value)
		}
		if _, ok := task["block"]; ok {
			for _, section := range []string{"block", "rescue", "always"} {
				items, _ := task[section].([]any)
				nested := make([]map[string]any, 0, len(items))
				for _, item := range items {
					child, ok := item.(map[string]any)
					if !ok {
						t.Fatalf("%s: block %q contains %v", file, name, item)
					}
					nested = append(nested, child)
				}
				walkAnsibleTasks(t, root, file, nested, checkMode, notify, visit)
			}
			continue
		}
		var modules []string
		for key := range task {
			if strings.HasPrefix(key, "ansible.builtin.") {
				modules = append(modules, key)
			}
		}
		if len(modules) != 1 {
			t.Fatalf("%s: task %q uses modules %v, want exactly one ansible.builtin module", file, name, modules)
		}
		switch module := modules[0]; module {
		case "ansible.builtin.import_tasks", "ansible.builtin.include_tasks":
			walkAnsibleFile(t, root, filepath.Join(filepath.Dir(file), task[module].(string)), checkMode, notify, visit)
		case "ansible.builtin.import_role", "ansible.builtin.include_role":
			role := task[module].(map[string]any)
			tasksFrom, _ := role["tasks_from"].(string)
			if tasksFrom == "" {
				tasksFrom = "main"
			}
			if filepath.Ext(tasksFrom) == "" {
				tasksFrom += ".yml"
			}
			walkAnsibleFile(t, root, filepath.Join("roles", role["name"].(string), "tasks", tasksFrom), checkMode, notify, visit)
		default:
			visit(ansibleTask{Key: file + ": " + name, Module: module, CheckMode: checkMode, Notify: notify})
		}
	}
}

func TestRunnerVerificationComparesEveryAppliedDeclaration(t *testing.T) {
	root := filepath.Join("..", "..", "ansible")
	declarative := []string{
		"ansible.builtin.apt",
		"ansible.builtin.blockinfile",
		"ansible.builtin.copy",
		"ansible.builtin.file",
		"ansible.builtin.group",
		"ansible.builtin.lineinfile",
		"ansible.builtin.service",
		"ansible.builtin.systemd",
		"ansible.builtin.systemd_service",
		"ansible.builtin.template",
		"ansible.builtin.user",
	}
	verifiedByProbe := []string{
		"roles/infra_binary/tasks/main.yml: Create root-owned artifact cache",
		"roles/infra_binary/tasks/main.yml: Activate verified binary",
		"roles/build_runner/tasks/remove.yml: Remove retired runner",
	}
	applyOnly := []string{
		"roles/infra_binary/tasks/main.yml: Remove superseded binary revisions",
		"roles/build_runner/tasks/main.yml: Remove superseded runner archives",
		"roles/build_runner/tasks/repository.yml: Record incomplete runner replacement",
		"roles/build_runner/tasks/repository.yml: Complete runner replacement",
		"roles/build_runner/tasks/remove.yml: Stop retired runner service",
		"roles/build_runner/tasks/remove.yml: Remove retired runner unit and resource override",
		"roles/build_runner/tasks/remove.yml: Reload systemd without retired runner",
		"roles/build_engine/tasks/main.yml: Stop retired Bazel cache",
		"roles/build_engine/tasks/main.yml: Remove retired native Bazel state",
		"roles/build_engine/tasks/main.yml: Reload units without retired Bazel cache",
	}
	applied := map[string]bool{}
	walkAnsiblePlays(t, root, "build-runners.yml", func(task ansibleTask) {
		if slices.Contains(declarative, task.Module) {
			applied[task.Key] = true
		}
	})
	var compared []ansibleTask
	verification := walkAnsiblePlays(t, root, "verify-runners.yml", func(task ansibleTask) {
		if slices.Contains(declarative, task.Module) {
			compared = append(compared, task)
		}
	})
	var driftTopics []string
	for _, play := range verification {
		for _, handler := range play.Handlers {
			if _, ok := handler["ansible.builtin.fail"]; ok {
				driftTopics = append(driftTopics, ansibleStrings(t, handler["name"])...)
				driftTopics = append(driftTopics, ansibleStrings(t, handler["listen"])...)
			}
		}
	}
	verified := map[string]bool{}
	for _, task := range compared {
		verified[task.Key] = true
		if !applied[task.Key] {
			t.Errorf("verification compares %s, which the runner play never applies", task.Key)
		}
		if !task.CheckMode {
			t.Errorf("verification applies %s instead of comparing it", task.Key)
		}
		if !slices.ContainsFunc(task.Notify, func(topic string) bool { return slices.Contains(driftTopics, topic) }) {
			t.Errorf("drift reported by %s does not fail verification", task.Key)
		}
	}
	for key := range applied {
		if !verified[key] && !slices.Contains(verifiedByProbe, key) && !slices.Contains(applyOnly, key) {
			t.Errorf("%s is applied but never compared by verification", key)
		}
	}
	for _, key := range slices.Concat(verifiedByProbe, applyOnly) {
		if !applied[key] || verified[key] {
			t.Errorf("stale verification exemption %s", key)
		}
	}
	if len(verified) == 0 {
		t.Fatal("verification compares no declared state")
	}
}
