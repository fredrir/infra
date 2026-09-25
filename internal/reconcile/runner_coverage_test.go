package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type ansiblePlay struct {
	Name              string           `yaml:"name"`
	Hosts             string           `yaml:"hosts"`
	IgnoreUnreachable bool             `yaml:"ignore_unreachable"`
	ImportPlaybook    string           `yaml:"import_playbook"`
	Tags              any              `yaml:"tags"`
	Serial            any              `yaml:"serial"`
	CheckMode         *bool            `yaml:"check_mode"`
	Vars              map[string]any   `yaml:"vars"`
	PreTasks          []map[string]any `yaml:"pre_tasks"`
	Roles             []ansibleRole    `yaml:"roles"`
	Tasks             []map[string]any `yaml:"tasks"`
	Handlers          []map[string]any `yaml:"handlers"`
}

var playKeywords = []string{"any_errors_fatal", "become", "check_mode", "gather_facts", "handlers", "hosts", "ignore_unreachable", "import_playbook", "name", "order", "pre_tasks", "roles", "serial", "tags", "tasks", "vars"}

func (p *ansiblePlay) UnmarshalYAML(node *yaml.Node) error {
	if err := classifiedKeywords(node, "play", playKeywords); err != nil {
		return err
	}
	type play ansiblePlay
	return node.Decode((*play)(p))
}

type ansibleRole struct {
	Name      string         `yaml:"role"`
	CheckMode *bool          `yaml:"check_mode"`
	When      any            `yaml:"when"`
	Vars      map[string]any `yaml:"vars"`
	Tags      any            `yaml:"tags"`
}

func (r *ansibleRole) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&r.Name)
	}
	if err := classifiedKeywords(node, "role entry", []string{"check_mode", "role", "tags", "vars", "when"}); err != nil {
		return err
	}
	type role ansibleRole
	return node.Decode((*role)(r))
}

func classifiedKeywords(node *yaml.Node, kind string, keywords []string) error {
	for index := 0; index+1 < len(node.Content); index += 2 {
		if key := node.Content[index]; !slices.Contains(keywords, key.Value) {
			return fmt.Errorf("line %d: %s uses unclassified keyword %s", key.Line, kind, key.Value)
		}
	}
	return nil
}

type ansibleTask struct {
	Key                string
	Module             string
	Role               string
	Definition         map[string]any
	Inherited          []any
	CheckMode          bool
	CheckModeDeclared  bool
	EnclosingCheckMode bool
	IgnoreErrors       bool
	Notify             []string
	When               []string
}

var (
	checkModeModules = []string{
		"ansible.builtin.apt",
		"ansible.builtin.copy",
		"ansible.builtin.file",
		"ansible.builtin.get_url",
		"ansible.builtin.hostname",
		"ansible.builtin.lineinfile",
		"ansible.builtin.systemd_service",
		"ansible.builtin.template",
		"ansible.builtin.user",
	}
	readOnlyModules = []string{
		"ansible.builtin.assert",
		"ansible.builtin.fail",
		"ansible.builtin.find",
		"ansible.builtin.getent",
		"ansible.builtin.meta",
		"ansible.builtin.set_fact",
		"ansible.builtin.setup",
		"ansible.builtin.slurp",
		"ansible.builtin.stat",
	}
	checkModeSkippedModules = []string{
		"ansible.builtin.command",
		"ansible.builtin.shell",
		"ansible.builtin.unarchive",
		"ansible.builtin.uri",
		"ansible.builtin.wait_for",
		"ansible.builtin.wait_for_connection",
	}
)

func classifiedModule(t *testing.T, task ansibleTask) bool {
	t.Helper()
	if slices.Contains(checkModeModules, task.Module) || slices.Contains(readOnlyModules, task.Module) || slices.Contains(checkModeSkippedModules, task.Module) {
		return true
	}
	t.Errorf("%s uses unclassified module %s", task.Key, task.Module)
	return false
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
	case bool:
		return []string{fmt.Sprint(value)}
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

func ansibleBlockSection(t *testing.T, file string, block map[string]any, section string) []map[string]any {
	t.Helper()
	items, _ := block[section].([]any)
	tasks := make([]map[string]any, 0, len(items))
	for _, item := range items {
		task, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s: block %q contains %v", file, block["name"], item)
		}
		tasks = append(tasks, task)
	}
	return tasks
}

func requireAnsibleTaskNames(t *testing.T, file string, tasks []map[string]any, seen map[string]bool) {
	t.Helper()
	for _, task := range tasks {
		name, _ := task["name"].(string)
		if name == "" || seen[name] {
			t.Errorf("%s: task names must be unique and non-empty, found %q", file, name)
		}
		seen[name] = true
		if _, ok := task["block"]; ok {
			for _, section := range []string{"block", "rescue", "always"} {
				requireAnsibleTaskNames(t, file, ansibleBlockSection(t, file, task, section), seen)
			}
		}
	}
}

func walkAnsiblePlays(t *testing.T, root, file string, visit func(ansibleTask)) []ansiblePlay {
	t.Helper()
	plays := loadAnsible[[]ansiblePlay](t, root, file)
	for _, play := range plays {
		walkAnsiblePlay(t, root, file, play, visit)
	}
	return plays
}

func declareCheckMode(task *ansibleTask, value *bool) {
	if value != nil {
		task.CheckMode, task.CheckModeDeclared = *value, true
	}
}

func walkAnsiblePlay(t *testing.T, root, file string, play ansiblePlay, visit func(ansibleTask)) {
	t.Helper()
	inherited := ansibleTask{Inherited: []any{play.Vars}}
	declareCheckMode(&inherited, play.CheckMode)
	names := map[string]bool{}
	requireAnsibleTaskNames(t, file, play.PreTasks, names)
	walkAnsibleTasks(t, root, file, play.PreTasks, inherited, visit)
	for _, role := range play.Roles {
		roleTask := inherited
		roleTask.Role = role.Name
		declareCheckMode(&roleTask, role.CheckMode)
		roleTask.When = append(slices.Clone(inherited.When), ansibleStrings(t, role.When)...)
		roleTask.Inherited = append(slices.Clone(inherited.Inherited), role.Vars)
		walkAnsibleFile(t, root, filepath.Join("roles", role.Name, "tasks", "main.yml"), roleTask, visit)
	}
	requireAnsibleTaskNames(t, file, play.Tasks, names)
	walkAnsibleTasks(t, root, file, play.Tasks, inherited, visit)
}

func walkAnsibleFile(t *testing.T, root, file string, inherited ansibleTask, visit func(ansibleTask)) {
	t.Helper()
	tasks := loadAnsible[[]map[string]any](t, root, file)
	requireAnsibleTaskNames(t, file, tasks, map[string]bool{})
	walkAnsibleTasks(t, root, file, tasks, inherited, visit)
}

func walkAnsibleTasks(t *testing.T, root, file string, tasks []map[string]any, inherited ansibleTask, visit func(ansibleTask)) {
	t.Helper()
	for _, definition := range tasks {
		name, _ := definition["name"].(string)
		task := ansibleTask{
			Key:                file + ": " + name,
			Role:               inherited.Role,
			Definition:         definition,
			Inherited:          inherited.Inherited,
			CheckMode:          inherited.CheckMode,
			CheckModeDeclared:  inherited.CheckModeDeclared,
			EnclosingCheckMode: inherited.CheckMode,
			IgnoreErrors:       inherited.IgnoreErrors,
			Notify:             inherited.Notify,
			When:               append(slices.Clone(inherited.When), ansibleStrings(t, definition["when"])...),
		}
		if value, ok := definition["check_mode"]; ok {
			if task.CheckMode, ok = value.(bool); !ok {
				t.Fatalf("%s has non-literal check_mode %v", task.Key, value)
			}
			task.CheckModeDeclared = true
		}
		if value, ok := definition["ignore_errors"]; ok {
			task.IgnoreErrors = value != false
		}
		if value, ok := definition["notify"]; ok {
			task.Notify = ansibleStrings(t, value)
		}
		if _, ok := definition["block"]; ok {
			block := task
			block.Inherited = append(slices.Clone(task.Inherited), definition["vars"])
			for _, section := range []string{"block", "rescue", "always"} {
				walkAnsibleTasks(t, root, file, ansibleBlockSection(t, file, definition, section), block, visit)
			}
			continue
		}
		var modules []string
		for key := range definition {
			if strings.HasPrefix(key, "ansible.builtin.") {
				modules = append(modules, key)
			}
		}
		if len(modules) != 1 {
			t.Fatalf("%s uses modules %v, want exactly one ansible.builtin module", task.Key, modules)
		}
		switch task.Module = modules[0]; task.Module {
		case "ansible.builtin.import_tasks", "ansible.builtin.include_tasks":
			walkAnsibleFile(t, root, filepath.Join(filepath.Dir(file), definition[task.Module].(string)), task, visit)
		case "ansible.builtin.import_role", "ansible.builtin.include_role":
			role := definition[task.Module].(map[string]any)
			tasksFrom, _ := role["tasks_from"].(string)
			if tasksFrom == "" {
				tasksFrom = "main"
			}
			if filepath.Ext(tasksFrom) == "" {
				tasksFrom += ".yml"
			}
			task.Role = role["name"].(string)
			walkAnsibleFile(t, root, filepath.Join("roles", task.Role, "tasks", tasksFrom), task, visit)
		default:
			visit(task)
		}
	}
}

func TestRunnerVerificationComparesEveryAppliedDeclaration(t *testing.T) {
	root := filepath.Join("..", "..", "ansible")
	probedTasks := []string{
		"roles/infra_binary/tasks/main.yml: Create root-owned artifact cache",
		"roles/infra_binary/tasks/main.yml: Download verified compiled binary",
		"roles/infra_binary/tasks/main.yml: Activate verified binary",
		"roles/build_runner/tasks/remove.yml: Remove retired runner",
	}
	applyOnlyTasks := []string{
		"roles/infra_binary/tasks/main.yml: Remove superseded binary revisions",
		"roles/build_runner/tasks/node-exporter.yml: Download verified node exporter archive",
		"roles/build_runner/tasks/node-exporter.yml: Remove superseded node exporter releases",
		"roles/build_runner/tasks/main.yml: Restart replaced node exporter",
		"roles/build_runner/tasks/main.yml: Download verified runner archive",
		"roles/build_runner/tasks/main.yml: Remove superseded runner archives",
		"roles/build_runner/tasks/repository.yml: Remove unregistered runner identity",
		"roles/build_runner/tasks/repository.yml: Record incomplete runner replacement",
		"roles/build_runner/tasks/repository.yml: Complete runner replacement",
		"roles/build_runner/tasks/remove.yml: Stop retired runner service",
		"roles/build_runner/tasks/remove.yml: Remove retired runner unit and resource override",
		"roles/build_runner/tasks/remove.yml: Reload systemd without retired runner",
	}
	hostReads := []string{
		"verify-runners.yml: Probe observed runner host",
		"roles/host_packages/tasks/main.yml: Check declared packages",
		"roles/build_runner/tasks/state.yml: Find stale runner slice",
		"roles/build_runner/tasks/state.yml: Find stale guest metric services",
		"roles/build_runner/tasks/repository-services.yml: Find stale runner services",
		"roles/build_engine/tasks/state.yml: Find stale build engine",
	}
	applied := map[string]bool{}
	walkAnsiblePlays(t, root, "build-runners.yml", func(task ansibleTask) {
		if classifiedModule(t, task) && slices.Contains(checkModeModules, task.Module) {
			applied[task.Key] = true
		}
	})
	var compared []ansibleTask
	read := map[string]bool{}
	verification := walkAnsiblePlays(t, root, "verify-runners.yml", func(task ansibleTask) {
		if task.IgnoreErrors {
			t.Errorf("verification ignores errors of %s", task.Key)
		}
		if switchesOnCheckMode(task) {
			t.Errorf("verification runs %s differently from the runner play", task.Key)
		}
		switch {
		case !classifiedModule(t, task):
		case !task.CheckMode:
			read[task.Key] = true
			if !slices.Contains(hostReads, task.Key) || task.Definition["changed_when"] != false {
				t.Errorf("verification runs %s outside check mode without being a declared host read", task.Key)
			}
		case slices.Contains(checkModeModules, task.Module):
			compared = append(compared, task)
		case slices.Contains(checkModeSkippedModules, task.Module):
			t.Errorf("verification skips %s in check mode", task.Key)
		}
	})
	for _, key := range hostReads {
		if !read[key] {
			t.Errorf("verification never reads %s", key)
		}
	}
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
		if !slices.ContainsFunc(task.Notify, func(topic string) bool { return slices.Contains(driftTopics, topic) }) {
			t.Errorf("drift reported by %s does not fail verification", task.Key)
		}
		for _, keyword := range []string{"changed_when", "failed_when"} {
			if _, ok := task.Definition[keyword]; ok {
				t.Errorf("%s overrides %s, which hides drift from verification", task.Key, keyword)
			}
		}
	}
	for key := range applied {
		if !verified[key] && !slices.Contains(probedTasks, key) && !slices.Contains(applyOnlyTasks, key) {
			t.Errorf("%s is applied but never compared by verification", key)
		}
	}
	for _, key := range slices.Concat(probedTasks, applyOnlyTasks) {
		if !applied[key] || verified[key] {
			t.Errorf("stale verification exemption %s", key)
		}
	}
	if len(verified) == 0 {
		t.Fatal("verification compares no declared state")
	}
}
