package reconcile

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
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

const checkModeSerial = "{{ '100%' if ansible_check_mode else 1 }}"

func references(text, name string) []string {
	return regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\b(\.[A-Za-z_]+)?`).FindAllString(text, -1)
}

func TestHostPlaysCompareProductionInCheckMode(t *testing.T) {
	root := filepath.Join("..", "..", "ansible")
	hostReads := []string{
		"roles/ubuntu/tasks/main.yml: Validate the complete SSH configuration",
		"roles/ubuntu/tasks/main.yml: Inspect effective SSH authentication",
		"roles/host_packages/tasks/main.yml: Check declared packages",
		"roles/firewall/tasks/main.yml: Require the enrolled management interface",
		"roles/firewall/tasks/main.yml: Confirm management access",
		"roles/k3s/tasks/main.yml: Verify the selected registration server API and datastore",
		"roles/k3s/tasks/main.yml: Wait for the node to become ready",
		"roles/k3s/tasks/main.yml: Wait for the kubelet to publish the CI job slots",
		"roles/ci_runtime/tasks/main.yml: Require the pinned runtime and sidecar binaries",
		"roles/build_vm/tasks/main.yml: Wait for guest SSH",
		"external.yml: Read the restored transport address",
		"roles/gatus/tasks/main.yml: Read the public upstream artifact token",
		"roles/gatus/tasks/main.yml: Wait for the monitor listener",
		"roles/gatus/tasks/main.yml: Verify monitor readiness",
	}
	driftProbes := []string{
		"roles/tailscale/tasks/tailscale-install.yml: Compare installed transport binaries with verified archive",
		"roles/tailscale/tasks/install.yml: Find unloaded tunnel support",
		"roles/k3s/tasks/main.yml: Find unloaded kernel modules",
		"roles/k3s/tasks/main.yml: Compare the upstream service with its declared command",
		"roles/ci_runtime/tasks/main.yml: Compare the extracted runtime and sidecar binaries",
		"external.yml: Read the embedded SSH server preference",
		"roles/gatus/tasks/main.yml: Compare the extracted binary with the pinned executable",
	}
	dryRuns := []string{
		"roles/k3s/tasks/main.yml: Assign worker capabilities through the administrator API",
		"roles/k3s/tasks/main.yml: Advertise CI job slots through the administrator API",
	}
	unverifiable := []string{
		"roles/k3s/tasks/main.yml: Install the upstream systemd service",
		"roles/ci_runtime/tasks/main.yml: Extract the runtime under opt",
	}
	seen := map[string]bool{}
	gatingResults := map[string]bool{}
	skippedResults := map[string]string{}
	var definitions []ansibleTask
	for _, playbook := range comparedPlaybooks {
		walkComparedPlays(t, root, playbook, func(play ansiblePlay) {
			if play.CheckMode {
				t.Errorf("%s: play %q never applies its declarations", playbook, play.Name)
			}
			if play.Serial != nil && play.Serial != checkModeSerial {
				t.Errorf("%s: play %q uses serial %v instead of lifting its batches in check mode with %s", playbook, play.Name, play.Serial, checkModeSerial)
			}
		}, func(task ansibleTask) {
			seen[task.Key] = true
			definitions = append(definitions, task)
			if !classifiedModule(t, task) {
				return
			}
			definition := fmt.Sprint(task.Definition)
			options, _ := task.Definition[task.Module].(map[string]any)
			_, creates := options["creates"]
			_, removes := options["removes"]
			command := task.Module == "ansible.builtin.command" || task.Module == "ansible.builtin.shell"
			compared := slices.Contains(checkModeModules, task.Module) || (command && (creates || removes))
			optedOut := task.CheckModeDeclared && !task.CheckMode
			changedWhen, reportsChanges := task.Definition["changed_when"]
			reportsChanges = reportsChanges && changedWhen != false
			register, _ := task.Definition["register"].(string)
			if strings.Contains(definition, "ansible_check_mode") && !slices.Contains(dryRuns, task.Key) {
				t.Errorf("%s runs differently in check mode", task.Key)
			}
			if task.CheckModeDeclared && task.CheckMode {
				t.Errorf("%s never applies its declaration", task.Key)
			}
			if task.IgnoreErrors {
				t.Errorf("%s ignores errors that reveal drift", task.Key)
			}
			comparesInCheckMode := optedOut || slices.Contains(readOnlyModules, task.Module)
			if comparesInCheckMode && reportsChanges != slices.Contains(driftProbes, task.Key) && !slices.Contains(dryRuns, task.Key) {
				t.Errorf("%s reports changes without being a declared drift probe, or a declared probe reports none", task.Key)
			}
			switch {
			case compared:
				gatingResults[register] = register != ""
				if optedOut {
					t.Errorf("%s opts its declaration out of check mode", task.Key)
				}
				for _, keyword := range []string{"changed_when", "failed_when"} {
					if _, ok := task.Definition[keyword]; ok {
						t.Errorf("%s overrides %s, which hides drift in check mode", task.Key, keyword)
					}
				}
			case slices.Contains(driftProbes, task.Key):
				gatingResults[register] = register != ""
				if register == "" || !comparesInCheckMode {
					t.Errorf("drift probe %s does not run and register its comparison in check mode", task.Key)
				}
			case slices.Contains(readOnlyModules, task.Module):
				if optedOut {
					t.Errorf("%s opts a read out of check mode", task.Key)
				}
			case slices.Contains(hostReads, task.Key):
				if !optedOut {
					t.Errorf("check mode skips host read %s", task.Key)
				}
				if command && changedWhen != false {
					t.Errorf("host read %s reports changes", task.Key)
				}
				if task.Module == "ansible.builtin.uri" && (!slices.Contains([]any{nil, "GET"}, options["method"]) || options["body"] != nil || options["src"] != nil) {
					t.Errorf("host read %s sends more than a GET request", task.Key)
				}
			case slices.Contains(dryRuns, task.Key):
				if !optedOut || !strings.Contains(fmt.Sprint(task.Definition[task.Module]), "ansible_check_mode") {
					t.Errorf("%s does not switch to a dry run in check mode", task.Key)
				}
			case optedOut:
				t.Errorf("%s runs outside check mode without being a declared host read, probe or dry run", task.Key)
			default:
				gates := slices.Concat(task.When, ansibleStrings(t, task.Definition["loop"]))
				gated := slices.ContainsFunc(slices.Collect(maps.Keys(gatingResults)), func(result string) bool {
					return gatingResults[result] && slices.ContainsFunc(gates, func(gate string) bool { return len(references(gate, result)) > 0 })
				})
				if gated == slices.Contains(unverifiable, task.Key) {
					t.Errorf("check mode skips %s, which must either follow a compared result or be declared unverifiable", task.Key)
				}
				if _, ok := task.Definition["until"]; ok {
					t.Errorf("%s retries on a result check mode skips", task.Key)
				}
				if register != "" {
					skippedResults[register] = task.Key
				}
			}
		})
	}
	for result, key := range skippedResults {
		for _, task := range definitions {
			if task.Key == key {
				continue
			}
			for _, reference := range references(fmt.Sprint(task.Definition), result) {
				if reference != result+".changed" {
					t.Errorf("%s reads %s from %s, which check mode skips", task.Key, reference, key)
				}
			}
		}
	}
	for _, key := range slices.Concat(hostReads, driftProbes, dryRuns, unverifiable) {
		if !seen[key] {
			t.Errorf("stale check-mode classification %s", key)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no host play tasks compared")
	}
}

func TestCheckModePlaysNeverReachDryRunSwitches(t *testing.T) {
	root := filepath.Join("..", "..", "ansible")
	playbooks, err := filepath.Glob(filepath.Join(root, "*.yml"))
	if err != nil || len(playbooks) == 0 {
		t.Fatalf("no playbooks: %v", err)
	}
	walked := 0
	for _, playbook := range playbooks {
		walkAnsiblePlays(t, root, filepath.Base(playbook), func(task ansibleTask) {
			walked++
			if (task.CheckMode || task.EnclosingCheckMode) && strings.Contains(fmt.Sprint(task.Definition), "ansible_check_mode") {
				t.Errorf("%s: %s switches on ansible_check_mode inside a check_mode: true play, where it would apply for real", filepath.Base(playbook), task.Key)
			}
		})
	}
	if walked == 0 {
		t.Fatal("no playbook tasks walked")
	}
}
