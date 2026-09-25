package reconcile

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func walkComparedPlays(t *testing.T, root, file string, visitPlay func(ansiblePlay), visit func(ansibleTask)) {
	t.Helper()
	for _, play := range loadAnsible[[]ansiblePlay](t, root, file) {
		if play.ImportPlaybook != "" {
			walkComparedPlays(t, root, play.ImportPlaybook, visitPlay, visit)
			continue
		}
		if slices.Contains(ansibleStrings(t, play.Tags), "runners") {
			continue
		}
		visitPlay(play)
		roles := map[string]bool{}
		walkAnsiblePlay(t, root, file, play, func(task ansibleTask) {
			if task.Role != "" {
				roles[task.Role] = true
			}
			visit(task)
		})
		requireAnsibleTaskNames(t, file, play.Handlers, map[string]bool{})
		walkAnsibleTasks(t, root, file, play.Handlers, ansibleTask{}, visit)
		for _, role := range slices.Sorted(maps.Keys(roles)) {
			handlers := filepath.Join("roles", role, "handlers", "main.yml")
			if _, err := os.Stat(filepath.Join(root, handlers)); err == nil {
				walkAnsibleFile(t, root, handlers, ansibleTask{Role: role}, visit)
			}
		}
	}
}

func TestHostPlaysCompareProductionInCheckMode(t *testing.T) {
	root := filepath.Join("..", "..", "ansible")
	hostReads := []string{
		"roles/ubuntu/tasks/main.yml: Validate the complete SSH configuration",
		"roles/ubuntu/tasks/main.yml: Inspect effective SSH authentication",
		"roles/host_packages/tasks/main.yml: Check declared packages",
		"roles/tailscale/tasks/tailscale-install.yml: Compare installed transport binaries with verified archive",
		"roles/firewall/tasks/main.yml: Require the enrolled management interface",
		"roles/firewall/tasks/main.yml: Confirm management access",
		"roles/k3s/tasks/main.yml: Verify the selected registration server API and datastore",
		"roles/k3s/tasks/main.yml: Wait for the node to become ready",
		"roles/k3s/tasks/main.yml: Wait for the kubelet to publish the CI job slots",
		"roles/build_vm/tasks/main.yml: Wait for guest SSH",
		"external.yml: Read the restored transport address",
		"roles/gatus/tasks/main.yml: Read the public upstream artifact token",
		"roles/gatus/tasks/main.yml: Wait for the monitor listener",
		"roles/gatus/tasks/main.yml: Verify monitor readiness",
	}
	dryRuns := []string{
		"roles/k3s/tasks/main.yml: Assign worker capabilities through the administrator API",
		"roles/k3s/tasks/main.yml: Advertise CI job slots through the administrator API",
	}
	unverifiable := []string{
		"roles/tailscale/tasks/tailscale-install.yml: Extract Tailscale binaries",
		"roles/tailscale/tasks/install.yml: Load tunnel support",
		"external.yml: Disable the embedded SSH server",
		"roles/k3s/tasks/main.yml: Load kernel modules",
		"roles/k3s/tasks/main.yml: Install the upstream systemd service",
		"roles/ci_runtime/tasks/main.yml: Extract the runtime and sidecar binaries",
		"roles/ci_runtime/tasks/main.yml: Extract the runtime under opt",
		"roles/gatus/tasks/main.yml: Extract the upstream binary",
	}
	seen := map[string]bool{}
	comparedResults := map[string]bool{}
	for _, playbook := range comparedPlaybooks {
		walkComparedPlays(t, root, playbook, func(play ansiblePlay) {
			if play.CheckMode {
				t.Errorf("%s: play %q never applies its declarations", playbook, play.Name)
			}
			if serial := fmt.Sprint(play.Serial); play.Serial != nil && !strings.Contains(serial, "ansible_check_mode") {
				t.Errorf("%s: play %q keeps serial batches %s while comparing in check mode", playbook, play.Name, serial)
			}
		}, func(task ansibleTask) {
			seen[task.Key] = true
			if !classifiedModule(t, task) {
				return
			}
			arguments := fmt.Sprint(task.Definition[task.Module])
			options, _ := task.Definition[task.Module].(map[string]any)
			_, creates := options["creates"]
			_, removes := options["removes"]
			command := task.Module == "ansible.builtin.command" || task.Module == "ansible.builtin.shell"
			compared := slices.Contains(checkModeModules, task.Module) || (command && (creates || removes))
			optedOut := task.CheckModeDeclared && !task.CheckMode
			if slices.ContainsFunc(task.When, func(condition string) bool { return strings.Contains(condition, "ansible_check_mode") }) {
				t.Errorf("%s runs differently in check mode", task.Key)
			}
			if task.CheckModeDeclared && task.CheckMode {
				t.Errorf("%s never applies its declaration", task.Key)
			}
			if task.IgnoreErrors {
				t.Errorf("%s ignores errors that reveal drift", task.Key)
			}
			switch {
			case compared:
				if register, ok := task.Definition["register"].(string); ok {
					comparedResults[register] = true
				}
				if optedOut {
					t.Errorf("%s opts its declaration out of check mode", task.Key)
				}
				for _, keyword := range []string{"changed_when", "failed_when"} {
					if _, ok := task.Definition[keyword]; ok {
						t.Errorf("%s overrides %s, which hides drift in check mode", task.Key, keyword)
					}
				}
			case slices.Contains(readOnlyModules, task.Module):
				if optedOut {
					t.Errorf("%s opts a read out of check mode", task.Key)
				}
			case slices.Contains(hostReads, task.Key):
				if !optedOut {
					t.Errorf("check mode skips host read %s", task.Key)
				}
				if command && task.Definition["changed_when"] != false {
					t.Errorf("host read %s reports changes", task.Key)
				}
			case slices.Contains(dryRuns, task.Key):
				if !optedOut || !strings.Contains(arguments, "ansible_check_mode") {
					t.Errorf("%s does not switch to a dry run in check mode", task.Key)
				}
			case optedOut:
				t.Errorf("%s runs outside check mode without being a declared host read or dry run", task.Key)
			case slices.ContainsFunc(task.When, func(condition string) bool {
				return comparedResults[strings.TrimSuffix(condition, ".changed")] && strings.HasSuffix(condition, ".changed")
			}):
			case !slices.Contains(unverifiable, task.Key):
				t.Errorf("check mode skips %s without a declared reason", task.Key)
			}
			if !compared && !optedOut && !slices.Contains(readOnlyModules, task.Module) {
				for _, keyword := range []string{"register", "until"} {
					if _, ok := task.Definition[keyword]; ok {
						t.Errorf("%s uses %s on a result check mode skips", task.Key, keyword)
					}
				}
			}
		})
	}
	for _, key := range slices.Concat(hostReads, dryRuns, unverifiable) {
		if !seen[key] {
			t.Errorf("stale check-mode classification %s", key)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no host play tasks compared")
	}
}
