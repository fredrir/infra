package reconciler

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type roleTask struct {
	Name    string         `yaml:"name"`
	Tags    []string       `yaml:"tags"`
	File    map[string]any `yaml:"ansible.builtin.file"`
	Command map[string]any `yaml:"ansible.builtin.command"`
	Assert  map[string]any `yaml:"ansible.builtin.assert"`
	Other   map[string]any `yaml:",inline"`
}

func TestSSHIdentityIsGeneratedOnTheReconcilerAndNeverLeavesIt(t *testing.T) {
	var defaults struct {
		Identity string `yaml:"reconciler_ssh_identity"`
	}
	if err := yaml.Unmarshal(roleFile(t, "defaults/main.yml"), &defaults); err != nil {
		t.Fatal(err)
	}
	if defaults.Identity != "/etc/infra-reconcile/ssh/id_ed25519" {
		t.Fatalf("reconciler_ssh_identity = %q", defaults.Identity)
	}
	var tasks []roleTask
	if err := yaml.Unmarshal(roleFile(t, "tasks/main.yml"), &tasks); err != nil {
		t.Fatal(err)
	}
	var tagged []string
	generated := false
	for _, task := range tasks {
		if slices.Contains(task.Tags, "ssh_identity") {
			tagged = append(tagged, task.Name)
		}
		for module := range task.Other {
			if strings.Contains(module, "copy") || strings.Contains(module, "fetch") || strings.Contains(module, "slurp") || strings.Contains(module, "template") {
				if strings.Contains(strings.ReplaceAll(yamlString(t, task.Other[module]), "{{ reconciler_ssh_identity }}", defaults.Identity), "/etc/infra-reconcile/ssh") {
					t.Errorf("%q moves the SSH identity with %s", task.Name, module)
				}
			}
		}
		if task.File != nil && task.File["path"] == "{{ reconciler_ssh_identity | dirname }}" && (task.File["mode"] != "0700" || task.File["owner"] != "root") {
			t.Errorf("%q leaves the identity directory %v", task.Name, task.File)
		}
		if task.Command != nil && task.Command["creates"] == "{{ reconciler_ssh_identity }}" {
			argv, _ := task.Command["argv"].([]any)
			generated = len(argv) > 0 && argv[0] == "ssh-keygen" && slices.Contains(argv, any("ed25519")) && slices.Contains(argv, any(""))
		}
	}
	if !generated {
		t.Error("the role does not generate an unencrypted ed25519 identity once on the host")
	}
	if want := []string{"Create the reconciler configuration directory", "Create the private SSH identity directory", "Generate the reconciler SSH identity", "Read the reconciler SSH public key", "Report the reconciler SSH public key"}; !slices.Equal(tagged, want) {
		t.Errorf("ssh_identity runs %q, want %q", tagged, want)
	}
}

func yamlString(t *testing.T, value any) string {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestVerificationSharesOnlyTheReconciliationDirectory(t *testing.T) {
	var tasks []struct {
		Name  string         `yaml:"name"`
		Group map[string]any `yaml:"ansible.builtin.group"`
		User  map[string]any `yaml:"ansible.builtin.user"`
		File  map[string]any `yaml:"ansible.builtin.file"`
		Loop  any            `yaml:"loop"`
	}
	if err := yaml.Unmarshal(roleFile(t, "tasks/main.yml"), &tasks); err != nil {
		t.Fatal(err)
	}
	var group, member, shared bool
	for _, task := range tasks {
		switch {
		case task.Group != nil:
			group = task.Group["name"] == "infra-reconcile" && task.Group["system"] == true
		case task.User != nil && task.User["name"] == "infra-verify":
			member = reflect.DeepEqual(task.User["groups"], []any{"infra-reconcile"}) && task.User["append"] == false
		case task.File != nil && reflect.DeepEqual(task.Loop, []any{"{{ reconciler_shared }}", "{{ reconciler_shared }}/requests"}):
			shared = task.File["owner"] == "root" && task.File["group"] == "infra-reconcile" && task.File["mode"] == "2770"
		}
	}
	if !group || !member || !shared {
		t.Errorf("group %v, membership %v, shared directories %v", group, member, shared)
	}
	service := string(roleFile(t, "templates/infra-reconcile-verify.service.j2"))
	if writable := regexp.MustCompile(`(?m)^ReadWritePaths=.*$`).FindAllString(service, -1); !slices.Equal(writable, []string{"ReadWritePaths={{ reconciler_shared }}"}) {
		t.Errorf("verify unit writes %q", writable)
	}
}
