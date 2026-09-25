package reconcile

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestAnsibleScopeWithLocalContainers(t *testing.T) {
	image := os.Getenv("INFRA_ANSIBLE_TEST_IMAGE")
	if image == "" {
		t.Skip("requires a local Linux container image with Python matching the Ansible environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := func(args ...string) []byte {
		t.Helper()
		output, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", args[0], err, output)
		}
		return output
	}
	root := strings.TrimSpace(string(command("git", "rev-parse", "--show-toplevel")))
	packages := os.Getenv("INFRA_ANSIBLE_SITE_PACKAGES")
	if packages == "" {
		matches, err := filepath.Glob(filepath.Join(root, ".venv/lib/python*/site-packages"))
		if err != nil || len(matches) != 1 {
			t.Fatal("set INFRA_ANSIBLE_SITE_PACKAGES to the local Ansible Python package directory")
		}
		packages = matches[0]
	}
	fixture := t.TempDir()
	write := func(path, data string, executable bool) {
		t.Helper()
		path = filepath.Join(fixture, path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0644)
		if executable {
			mode = 0755
		}
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	fleet, err := LoadRunnerFleet(root)
	if err != nil {
		t.Fatal(err)
	}
	engineDefaults := map[string]any{}
	if err := yaml.Unmarshal(read(filepath.Join(root, "ansible/roles/build_engine/defaults/main.yml")), &engineDefaults); err != nil {
		t.Fatal(err)
	}
	toolchain := map[string]any{}
	if err := json.Unmarshal(read(filepath.Join(root, "build/toolchain.json")), &toolchain); err != nil {
		t.Fatal(err)
	}
	version := fleet.Version
	cli := "#!/bin/sh\necho fixture\n"
	write("infra", cli, true)
	write("vars.json", fmt.Sprintf(`{"build_runner_cli":{"sha256":"%x"}}`, sha256.Sum256([]byte(cli))), false)
	write("inventory.yml", "all:\n  children:\n    build_engines:\n      hosts:\n        localhost:\n          ansible_connection: local\n          ansible_user: root\n          ansible_python_interpreter: /usr/bin/python3\n          build_runner_repositories: [infra, Y]\n", false)
	for _, repository := range []string{"infra", "Y"} {
		write("runners/"+repository+"/.runner", "{}", false)
		write("runners/"+repository+"/.service", "actions.runner.fixture."+repository+".service", false)
		write("runners/"+repository+"/bin/Runner.Listener", "#!/bin/sh\ncat /fixture/state/version-"+repository+"\n", true)
		write("state/version-"+repository, version, false)
	}
	write("bin/systemctl", `#!/bin/sh
case "$1" in
stop) echo "$2" >> /fixture/state/stopped; exit 0 ;;
disable) echo "$2" >> /fixture/state/disabled; exit 0 ;;
show) printf 'LoadState=loaded\nActiveState=active\n'; exit 0 ;;
is-enabled) echo enabled; exit 0 ;;
daemon-reload) if rm /fixture/state/reload-failure 2>/dev/null; then exit 1; fi; echo reload >> /fixture/state/reloaded; exit 0 ;;
esac
if [ -e /fixture/state/service-failure ] && [ "$2" = actions.runner.fixture.Y.service ]; then exit 1; fi
echo active
`, true)
	write("bin/docker", `#!/bin/sh
if [ -e /fixture/state/wrong-image ]; then printf '[{"State":{"Running":true},"Config":{"Image":"wrong"}}]'; exit 0; fi
cat "/fixture/state/$2.json"
`, true)
	for name, image := range map[string]any{"infra-dagger": toolchain["engine_image"], "infra-bazel-cache": engineDefaults["build_engine_bazel_image"]} {
		encoded, err := json.Marshal([]any{map[string]any{"State": map[string]any{"Running": true}, "Config": map[string]any{"Image": image}}})
		if err != nil {
			t.Fatal(err)
		}
		write("state/"+name+".json", string(encoded), false)
	}
	write("health/status", "{}", false)
	name := fmt.Sprintf("infra-ansible-scope-%d", time.Now().UnixNano())
	command("docker", "run", "-d", "--name", name, "--network=none", "-v", root+":/source:ro", "-v", fixture+":/fixture", "-v", filepath.Join(fixture, "runners")+":/home/runner", "-v", packages+":/opt/ansible:ro", "-e", "PYTHONPATH=/opt/ansible", "-e", "ANSIBLE_CONFIG=/source/ansible/ansible.cfg", "-e", "PATH=/fixture/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", image, "sleep", "infinity")
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", name, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/fixture").Run()
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	command("docker", "exec", name, "useradd", "--non-unique", "--uid", fmt.Sprint(os.Getuid()), "--no-create-home", "runner")
	command("docker", "exec", name, "ln", "-s", "/fixture/infra", "/usr/local/bin/infra")
	command("docker", "exec", "-d", name, "python3", "-m", "http.server", "9092", "--bind", "127.0.0.1", "--directory", "/fixture/health")
	run := func(playbook string, success bool, extra ...string) string {
		t.Helper()
		args := []string{"exec", name, "python3", "-m", "ansible.cli.playbook", "-i", "/fixture/inventory.yml", playbook, "--extra-vars", "@/fixture/vars.json"}
		args = append(args, extra...)
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if (err == nil) != success {
			t.Fatalf("Ansible success=%t: %v\n%s", success, err, output)
		}
		return string(output)
	}
	full := run("/source/ansible/reconcile.yml", true, "--list-tasks")
	runners := run("/source/ansible/build-runners.yml", true, "--list-tasks")
	taskStart := strings.Index(runners, "      infra_binary :")
	if taskStart < 0 || !strings.Contains(full, strings.TrimSpace(runners[taskStart:])) {
		t.Fatalf("runner-only path lost full-path prerequisites:\n%s", runners)
	}
	t.Log("full and runner-only paths resolve the same ordered runner tasks")
	for _, scenario := range []string{"valid", "service-failure", "wrong-image", "wrong-version", "wrong-cli", "cache-failure"} {
		t.Run(scenario, func(t *testing.T) {
			switch scenario {
			case "service-failure", "wrong-image":
				write("state/"+scenario, "", false)
			case "wrong-version":
				write("state/version-Y", "0.0.0", false)
			case "wrong-cli":
				write("infra", "wrong", true)
			case "cache-failure":
				if err := os.Remove(filepath.Join(fixture, "health/status")); err != nil {
					t.Fatal(err)
				}
			}
			output := run("/source/ansible/verify-runners.yml", scenario == "valid")
			if scenario == "valid" && !strings.Contains(output, "changed=0") {
				t.Fatalf("verification changed the host:\n%s", output)
			}
			if scenario == "service-failure" || scenario == "wrong-image" {
				if err := os.Remove(filepath.Join(fixture, "state/"+scenario)); err != nil {
					t.Fatal(err)
				}
			}
			write("state/version-Y", version, false)
			write("infra", cli, true)
		})
	}
	var tasks []map[string]any
	if err := yaml.Unmarshal(read(filepath.Join(root, "ansible/roles/build_runner/tasks/repository.yml")), &tasks); err != nil {
		t.Fatal(err)
	}
	for index, task := range tasks {
		if task["name"] == "Obtain single-use runner registration token" {
			tasks = tasks[:index]
			break
		}
	}
	upgrade, err := yaml.Marshal([]any{map[string]any{"hosts": "build_engines", "gather_facts": false, "vars": map[string]any{"build_runner_repository": "infra", "build_runner_version": version}, "tasks": tasks}})
	if err != nil {
		t.Fatal(err)
	}
	write("upgrade.yml", string(upgrade), false)
	archive := func() {
		t.Helper()
		file, err := os.Create(filepath.Join(fixture, "runner.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		compressed := gzip.NewWriter(file)
		writer := tar.NewWriter(compressed)
		data := "#!/bin/sh\necho " + version + "\n"
		for _, header := range []*tar.Header{{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0755}, {Name: "bin/Runner.Listener", Mode: 0755, Size: int64(len(data))}} {
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if header.Size > 0 {
				if _, err := writer.Write([]byte(data)); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, close := range []func() error{writer.Close, compressed.Close, file.Close} {
			if err := close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	archive()
	command("docker", "exec", name, "ln", "-s", "/fixture/runner.tar.gz", "/var/cache/actions-runner-"+version+".tar.gz")
	write("state/version-infra", "0.0.0", false)
	run("/fixture/upgrade.yml", true)
	stopped := string(read(filepath.Join(fixture, "state/stopped")))
	if stopped != "actions.runner.fixture.infra.service\n" {
		t.Fatalf("wrong service stopped: %q", stopped)
	}
	output := run("/fixture/upgrade.yml", true)
	if !strings.Contains(output, "changed=0") || string(read(filepath.Join(fixture, "state/stopped"))) != stopped {
		t.Fatalf("runner replacement is not idempotent:\n%s", output)
	}
	t.Log("runner upgrade stops only its own service and converges idempotently")
	write("runners/infra/bin/Runner.Listener", "#!/bin/sh\necho 0.0.0\n", true)
	write("runner.tar.gz", "invalid archive", false)
	run("/fixture/upgrade.yml", false)
	pending := filepath.Join(fixture, "runners/infra/.infra-runner-pending")
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("partial replacement lost its recovery marker", err)
	}
	archive()
	run("/fixture/upgrade.yml", true)
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatal("successful recovery left its pending marker", err)
	}
	t.Log("partial extraction failure retains recovery proof and retries successfully")
	var roleTasks []map[string]any
	if err := yaml.Unmarshal(read(filepath.Join(root, "ansible/roles/build_runner/tasks/main.yml")), &roleTasks); err != nil {
		t.Fatal(err)
	}
	configure := slices.IndexFunc(roleTasks, func(task map[string]any) bool { return task["name"] == "Configure repository runners" })
	if configure < 0 {
		t.Fatal("repository runner loop not found")
	}
	removalTasks := roleTasks[configure+1:]
	for _, task := range removalTasks {
		if path, ok := task["ansible.builtin.include_tasks"].(string); ok {
			task["ansible.builtin.include_tasks"] = "/source/ansible/roles/build_runner/tasks/" + path
		}
	}
	removal, err := yaml.Marshal([]any{map[string]any{"hosts": "build_engines", "gather_facts": false, "vars": map[string]any{"build_runner_fleet": "{{ lookup('ansible.builtin.file', '/source/build/runners.json') | from_json }}"}, "tasks": removalTasks}})
	if err != nil {
		t.Fatal(err)
	}
	write("removal.yml", string(removal), false)
	declaredUnit := "/etc/systemd/system/actions.runner.fixture.infra.service"
	retiredService := "actions.runner.fixture.localhost-retired.service"
	retiredUnit := "/etc/systemd/system/" + retiredService
	for _, unit := range []string{declaredUnit, retiredUnit} {
		command("docker", "exec", name, "mkdir", "-p", unit+".d")
		command("docker", "exec", name, "touch", unit, unit+".d/resources.conf")
	}
	exists := func(path string) bool {
		return exec.CommandContext(ctx, "docker", "exec", name, "test", "-e", path).Run() == nil
	}
	if err := os.Remove(filepath.Join(fixture, "state/stopped")); err != nil {
		t.Fatal(err)
	}
	write("runners/infra.bak/.runner", "{}", false)
	write("runners/infra.bak/.service", string(read(filepath.Join(fixture, "runners/infra/.service"))), false)
	if output := run("/fixture/removal.yml", false); !strings.Contains(output, "Assertion failed") {
		t.Fatalf("copied runner root did not fail its service identity check:\n%s", output)
	}
	for _, record := range []string{"state/stopped", "state/disabled"} {
		if _, err := os.Stat(filepath.Join(fixture, record)); !os.IsNotExist(err) {
			t.Fatal("copied runner root changed a declared service", err)
		}
	}
	if !exists(declaredUnit) || !exists(declaredUnit+".d/resources.conf") {
		t.Fatal("copied runner root removed the declared unit")
	}
	if _, err := os.Stat(filepath.Join(fixture, "runners/infra.bak/.service")); err != nil {
		t.Fatal("copied runner root was removed", err)
	}
	t.Log("copied runner root naming a declared service fails closed")
	if err := os.RemoveAll(filepath.Join(fixture, "runners/infra.bak")); err != nil {
		t.Fatal(err)
	}
	outside := slices.DeleteFunc(slices.Clone(fleet.Repositories), func(repository string) bool { return repository == "infra" || repository == "Y" })
	if len(outside) == 0 {
		t.Fatal("fleet declares no repository outside the host subset")
	}
	kept := []string{"runners/.runner", "runners/.hidden/.runner", "runners/infra/_work/x/.runner", "runners/unregistered/config.sh", "runners/" + outside[0] + "/.runner", "runners/infra/.runner", "runners/Y/.runner"}
	for _, path := range kept {
		write(path, "{}", false)
	}
	write("runners/retired/.runner", "{}", false)
	write("runners/retired/.service", retiredService, false)
	write("state/reload-failure", "", false)
	run("/fixture/removal.yml", false)
	if _, err := os.Stat(filepath.Join(fixture, "runners/retired/.service")); err != nil {
		t.Fatal("interrupted removal lost the retired service name", err)
	}
	run("/fixture/removal.yml", true)
	for record, want := range map[string]string{"stopped": retiredService + "\n", "disabled": retiredService + "\n", "reloaded": "reload\n"} {
		if got := string(read(filepath.Join(fixture, "state", record))); got != want {
			t.Fatalf("systemctl %s %q, want %q", record, got, want)
		}
	}
	if exists(retiredUnit) || exists(retiredUnit+".d") {
		t.Fatal("retired runner unit remains")
	}
	if _, err := os.Stat(filepath.Join(fixture, "runners/retired")); !os.IsNotExist(err) {
		t.Fatal("retired runner root remains", err)
	}
	if !exists(declaredUnit) || !exists(declaredUnit+".d/resources.conf") {
		t.Fatal("runner removal removed the declared unit")
	}
	for _, path := range kept {
		if _, err := os.Stat(filepath.Join(fixture, path)); err != nil {
			t.Fatal("runner removal touched a kept path", err)
		}
	}
	if output := run("/fixture/removal.yml", true); !strings.Contains(output, "changed=0") {
		t.Fatalf("runner removal is not idempotent:\n%s", output)
	}
	t.Log("undeclared runner removal resumes after interruption and keeps declared and unregistered paths")
	write("packages.yml", "- hosts: build_engines\n  gather_facts: false\n  module_defaults:\n    ansible.builtin.apt:\n      update_cache_retries: 1\n  roles:\n  - role: host_packages\n    vars:\n      host_packages_required: '{{ packages }}'\n", false)
	installed := `{"packages":["dpkg","tar"]}`
	if output := run("/fixture/packages.yml", true, "--extra-vars", installed); !strings.Contains(output, "changed=0") {
		t.Fatalf("installed packages refreshed the index:\n%s", output)
	}
	command("docker", "exec", name, "apt-mark", "hold", "tar")
	if output := run("/fixture/packages.yml", true, "--extra-vars", installed); !strings.Contains(output, "changed=0") {
		t.Fatalf("held package refreshed the index:\n%s", output)
	}
	command("docker", "exec", name, "apt-mark", "unhold", "tar")
	if output := run("/fixture/packages.yml", false, "--extra-vars", `{"packages":["dpkg","infra-fixture-absent"]}`); !strings.Contains(output, "Install missing declared packages") || strings.Contains(output, "skipping: [localhost]") {
		t.Fatalf("missing package did not reach installation:\n%s", output)
	}
	t.Log("package index refreshes only when a declared package is missing")
	write("patching.yml", "- hosts: build_engines\n  gather_facts: false\n  roles:\n  - host_patching\n", false)
	run("/fixture/patching.yml", true)
	if output := run("/fixture/patching.yml", true); !strings.Contains(output, "changed=0") {
		t.Fatalf("declared patching configuration is not idempotent:\n%s", output)
	}
	if output := string(command("docker", "exec", name, "apt-config", "dump", "Unattended-Upgrade")); !strings.Contains(output, `-security"`) || !strings.Contains(output, `Automatic-Reboot "false"`) {
		t.Fatalf("unattended upgrade policy not applied:\n%s", output)
	}
	t.Log("declared patching configuration validates and converges idempotently")
}
