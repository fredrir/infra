package reconcile

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
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
	remove := func(path string) {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(fixture, path)); err != nil {
			t.Fatal(err)
		}
	}
	fleet, err := LoadRunnerFleet(root)
	if err != nil {
		t.Fatal(err)
	}
	toolchain := map[string]any{}
	if err := json.Unmarshal(read(filepath.Join(root, "build/toolchain.json")), &toolchain); err != nil {
		t.Fatal(err)
	}
	version := fleet.Version
	cli := "#!/bin/sh\necho fixture\n"
	engine := fmt.Sprintf("true %s\n", toolchain["engine_image"])
	runnerUnit := func(repository string) string {
		return "actions.runner." + fleet.Owner + "-" + repository + ".localhost-" + repository + "-1.service"
	}
	runnerRegistration := func(runner, repository string, id int) string {
		return fmt.Sprintf("\ufeff{\"agentId\": %d, \"agentName\": \"localhost-%s\", \"gitHubUrl\": \"https://github.com/%s/%s\"}", id, runner, fleet.Owner, repository)
	}
	write("infra", cli, true)
	write("vars.json", fmt.Sprintf(`{"build_runner_cli":{"sha256":"%x"}}`, sha256.Sum256([]byte(cli))), false)
	write("inventory.yml", "all:\n  children:\n    build_engines:\n      hosts:\n        localhost:\n          ansible_connection: local\n          ansible_user: root\n          ansible_python_interpreter: /usr/bin/python3\n          build_runner_repositories: {infra: 1, Y: 1}\n          build_runner_packages: [dpkg, tar]\n", false)
	for _, repository := range []string{"infra", "Y"} {
		write("runners/"+repository+"-1/.runner", "{}\n", false)
		write("runners/"+repository+"-1/.service", runnerUnit(repository)+"\n", false)
		write("runners/"+repository+"-1/bin/Runner.Listener", "#!/bin/sh\ncat /fixture/state/version-"+repository+"\n", true)
		write("state/version-"+repository, version, false)
	}
	write("bin/systemctl", `#!/bin/sh
state=/fixture/state
listed() { grep -qxF "$2" "$state/$1" 2>/dev/null; }
started() { python3 -c 'import datetime; print(datetime.datetime.now(datetime.UTC).strftime("%a %Y-%m-%d %H:%M:%S.%f UTC"))' > "$state/started-$1"; }
case "$1" in
show)
  eval "unit=\${$#}"
  case "$*" in
  *ActiveEnterTimestamp*) cat "$state/started-$unit" 2>/dev/null || true ;;
  *NeedDaemonReload*) if listed reload-units "$unit"; then echo yes; else echo no; fi ;;
  *) if listed inactive-units "$unit"; then echo ActiveState=inactive; else echo ActiveState=active; fi; echo LoadState=loaded ;;
  esac ;;
is-enabled) if listed disabled-units "$2"; then echo disabled; exit 1; fi; echo enabled ;;
start) started "$2" ;;
restart) echo "$2" >> "$state/restarted"; started "$2"; if [ "$2" = infra-dagger ]; then cp "$state/engine-pinned" "$state/engine"; fi ;;
stop) echo "$2" >> "$state/stopped"; if pgrep --full '/bin/Runner[.]Worker' > /dev/null; then echo "$2" >> "$state/stopped-mid-job"; fi ;;
disable) echo "$2" >> "$state/disabled" ;;
daemon-reload) if rm "$state/reload-failure" 2>/dev/null; then exit 1; fi; rm -f "$state/reload-units"; echo reload >> "$state/reloaded" ;;
esac
`, true)
	write("bin/docker", "#!/bin/sh\ncase \"$*\" in\n*State.Running*) cat /fixture/state/engine ;;\n*) cut -d ' ' -f 2 /fixture/state/engine ;;\nesac\n", true)
	write("state/engine", engine, false)
	write("state/engine-pinned", engine, false)
	write("converge.yml", `- hosts: build_engines
  gather_facts: false
  vars:
    build_runner_fleet: "{{ lookup('ansible.builtin.file', '/source/build/runners.json') | from_json }}"
    build_engine_toolchain: "{{ lookup('ansible.builtin.file', '/source/build/toolchain.json') | from_json }}"
  tasks:
  - ansible.builtin.import_role:
      name: build_runner
      tasks_from: state.yml
  - ansible.builtin.include_role:
      name: build_runner
      tasks_from: repository.yml
    loop: '{{ build_runner_instances }}'
    loop_control:
      loop_var: build_runner_instance
  - ansible.builtin.import_role:
      name: build_engine
      tasks_from: state.yml
`, false)
	name := fmt.Sprintf("infra-ansible-scope-%d", time.Now().UnixNano())
	command("docker", "run", "-d", "--name", name, "--network=none", "-v", root+":/source:ro", "-v", fixture+":/fixture", "-v", filepath.Join(fixture, "runners")+":/home/runner", "-v", packages+":/opt/ansible:ro", "-e", "PYTHONPATH=/opt/ansible", "-e", "ANSIBLE_CONFIG=/source/ansible/ansible.cfg", "-e", "PATH=/fixture/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", image, "sleep", "infinity")
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", name, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/fixture").Run()
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	command("docker", "exec", name, "useradd", "--non-unique", "--uid", fmt.Sprint(os.Getuid()), "--no-create-home", "runner")
	command("docker", "exec", name, "groupadd", "docker")
	command("docker", "exec", name, "mkdir", "-p", "/etc/tmpfiles.d")
	command("docker", "exec", name, "ln", "-s", "/fixture/infra", "/usr/local/bin/infra")
	playbookArgs := func(playbook string, extra ...string) []string {
		return append([]string{"exec", "-e", "PYTHONUNBUFFERED=1", name, "python3", "-m", "ansible.cli.playbook", "-i", "/fixture/inventory.yml", playbook, "--extra-vars", "@/fixture/vars.json"}, extra...)
	}
	run := func(playbook string, success bool, extra ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "docker", playbookArgs(playbook, extra...)...).CombinedOutput()
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
	listed := func(output string) map[string][]string {
		plays := map[string][]string{}
		var play string
		for line := range strings.SplitSeq(output, "\n") {
			switch {
			case strings.HasPrefix(line, "  play #"):
				play = strings.TrimSpace(line)
				plays[play] = []string{}
			case strings.HasPrefix(line, "      ") && play != "":
				plays[play] = append(plays[play], strings.TrimSpace(line))
			}
		}
		return plays
	}
	everything, withoutRunners := listed(full), listed(run("/source/ansible/reconcile.yml", true, "--list-tasks", "--skip-tags=runners"))
	runnerPlays := 0
	for play, tasks := range everything {
		skipped, ok := withoutRunners[play]
		switch {
		case !ok:
			t.Fatalf("skipping runners lost play %s", play)
		case strings.Contains(play, "Register trusted dedicated build runners"):
			runnerPlays++
			if len(tasks) == 0 || len(skipped) != 0 {
				t.Fatalf("skipping runners kept runner play tasks %q of %q", skipped, tasks)
			}
		case !slices.Equal(skipped, tasks):
			t.Fatalf("skipping runners changed %s: %q, want %q", play, skipped, tasks)
		}
	}
	if runnerPlays != 1 || len(withoutRunners) != len(everything) {
		t.Fatalf("skipping runners changed the plays: %d runner plays, %d of %d plays", runnerPlays, len(withoutRunners), len(everything))
	}
	var binary []string
	for _, tasks := range listed(run("/source/ansible/build-runners.yml", true, "--list-tasks", "--tags=infra_binary")) {
		binary = append(binary, tasks...)
	}
	if len(binary) < 2 || !strings.HasPrefix(binary[0], "Gather runner host facts\t") || slices.ContainsFunc(binary[1:], func(task string) bool { return !strings.HasPrefix(task, "infra_binary : ") }) {
		t.Fatalf("binary-only runner convergence selects more than facts and the binary: %q", binary)
	}
	t.Log("skipping runners removes only the runner play and the binary tag selects only facts and the binary")
	record := func(name string) string {
		data, err := os.ReadFile(filepath.Join(fixture, "state", name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(data)
	}
	restarted := func() string { return record("restarted") }
	converge := func() { run("/fixture/converge.yml", true) }
	reloadsOnly := func() {
		restarts, reloads := restarted(), record("reloaded")
		converge()
		if restarted() != restarts || record("reloaded") == reloads || record("reload-units") != "" {
			t.Fatalf("slice repair restarted %q or left the slice unreloaded", strings.TrimPrefix(restarted(), restarts))
		}
	}
	repairs := func(want string) func() {
		return func() {
			before := restarted()
			converge()
			if got := strings.TrimPrefix(restarted(), before); got != want+"\n" {
				t.Fatalf("repair restarted %q, want only %s", got, want)
			}
		}
	}
	converge()
	steady := restarted()
	if output := run("/fixture/converge.yml", true); !strings.Contains(output, "changed=0") || restarted() != steady {
		t.Fatalf("declared runner state does not converge without restarts:\n%s", output)
	}
	t.Log("declared runner state converges idempotently without restarting services")
	if output := run("/fixture/converge.yml", true, "--extra-vars", `{"build_runner_restart":["Y-1"]}`); restarted() != steady+runnerUnit("Y")+"\n" {
		t.Fatalf("requested runner restart restarted %q, want only %s:\n%s", strings.TrimPrefix(restarted(), steady), runnerUnit("Y"), output)
	}
	t.Log("runners reported offline restart without restarting the rest of the fleet")
	identity := []string{".runner", ".credentials", ".credentials_rsaparams"}
	reregisterY := `{"build_runner_reregister":["Y-1"]}`
	write("bin/gh", `#!/bin/sh
echo "$*" >> /fixture/state/gh
case "$*" in
*registration-token*) if [ -e /fixture/state/token-failure ]; then exit 1; fi; echo fixture-token ;;
*"--method DELETE"*) if [ -e /fixture/state/withdrawal-failure ]; then exit 1; fi ;;
*"--method PUT"*) cat >> /fixture/state/gh ;;
*"--jq .busy"*) if [ -e /fixture/state/github-busy ]; then echo true; else echo false; fi ;;
*) echo 7 ;;
esac
`, true)
	write("runners/Y-1/config.sh", "#!/bin/sh\nset -eu\necho \"$*\" >> /fixture/state/registered\nfor file in .runner .credentials .credentials_rsaparams; do echo registered > \"$file\"; done\n", true)
	for _, file := range identity {
		write("runners/Y-1/"+file, "stale\n", false)
	}
	write("state/token-failure", "", false)
	run("/fixture/converge.yml", false, "--extra-vars", reregisterY)
	for _, file := range identity {
		if kept := string(read(filepath.Join(fixture, "runners/Y-1", file))); kept != "stale\n" || record("registered") != "" {
			t.Fatalf("failed registration token replaced %s with %q", file, kept)
		}
	}
	remove("state/token-failure")
	t.Log("a registration token failure keeps the existing runner identity")
	before := restarted()
	reregistered := run("/fixture/converge.yml", true, "--extra-vars", reregisterY)
	registration := string(read(filepath.Join(fixture, "state/registered")))
	if strings.Count(registration, "\n") != 1 || !strings.Contains(registration, "--replace") || !strings.Contains(registration, "--token fixture-token") || !strings.Contains(registration, "--name localhost-Y-1 ") {
		t.Fatalf("missing registration did not re-register only Y with --replace: %q\n%s", registration, reregistered)
	}
	for _, file := range identity {
		if current := string(read(filepath.Join(fixture, "runners/Y-1", file))); current != "registered\n" {
			t.Fatalf("re-registration kept stale %s %q", file, current)
		}
	}
	if got := strings.TrimPrefix(restarted(), before); got != runnerUnit("Y")+"\n" {
		t.Fatalf("re-registration restarted %q, want only %s", got, runnerUnit("Y"))
	}
	if output := run("/fixture/converge.yml", true); !strings.Contains(output, "changed=0") {
		t.Fatalf("re-registered runner does not converge idempotently:\n%s", output)
	}
	t.Log("missing registrations re-register with fresh credentials and restart only their runner")
	outcome := func(output string) (changed, failed []string) {
		var task string
		for line := range strings.SplitSeq(output, "\n") {
			switch {
			case strings.HasPrefix(line, "TASK ["), strings.HasPrefix(line, "RUNNING HANDLER ["):
				task = line[strings.Index(line, "[")+1 : strings.LastIndex(line, "]")]
			case strings.HasPrefix(line, "changed: "):
				changed = append(changed, task)
			case strings.HasPrefix(line, "fatal: "), strings.HasPrefix(line, "failed: "):
				failed = append(failed, task)
			}
		}
		return slices.Compact(changed), slices.Compact(failed)
	}
	drift := []string{"Reject declared runner state drift"}
	observed := []string{"Verify observed runner host"}
	for _, scenario := range []struct {
		name            string
		introduce       func()
		restore         func()
		extra           []string
		changed, failed []string
	}{
		{name: "valid"},
		{
			name:      "stopped engine",
			introduce: func() { write("state/inactive-units", "infra-dagger\n", false) },
			restore:   func() { remove("state/inactive-units") },
			changed:   []string{"build_engine : Start build engine"},
			failed:    drift,
		},
		{
			name:      "disabled runner",
			introduce: func() { write("state/disabled-units", runnerUnit("Y")+"\n", false) },
			restore:   func() { remove("state/disabled-units") },
			changed:   []string{"build_runner : Start registered runner"},
			failed:    drift,
		},
		{
			name:      "edited hook",
			introduce: func() { command("docker", "exec", name, "sh", "-c", "echo >> /usr/local/libexec/infra-runner-hook.sh") },
			restore:   converge,
			changed:   []string{"build_runner : Install immutable trusted-job hook adapter"},
			failed:    drift,
		},
		{
			name:      "edited engine configuration",
			introduce: func() { command("docker", "exec", name, "sh", "-c", "echo >> /etc/infra-dagger.toml") },
			restore:   converge,
			changed:   []string{"build_engine : Install bounded engine cache retention", "build_engine : Restart stale build engine"},
			failed:    drift,
		},
		{
			name:      "stale engine",
			introduce: func() { command("docker", "exec", name, "touch", "/etc/infra-dagger.toml") },
			restore:   repairs("infra-dagger"),
			changed:   []string{"build_engine : Restart stale build engine"},
			failed:    drift,
		},
		{
			name:      "runner awaiting daemon reload",
			introduce: func() { write("state/reload-units", runnerUnit("Y")+"\n", false) },
			restore:   repairs(runnerUnit("Y")),
			changed:   []string{"build_runner : Restart stale runner service"},
			failed:    drift,
		},
		{
			name:   "missing package",
			extra:  []string{"--extra-vars", `{"build_runner_packages":["dpkg","infra-fixture-absent"]}`},
			failed: []string{"host_packages : Install missing declared packages"},
		},
		{
			name:   "overridden engine pin",
			extra:  []string{"--extra-vars", `{"build_engine_image":"registry.dagger.io/engine:v0.0.1@sha256:` + strings.Repeat("0", 64) + `"}`},
			failed: []string{"build_engine : Validate engine pin"},
		},
		{
			name:      "engine running an unpinned image",
			introduce: func() { write("state/engine", "true registry.dagger.io/engine:v0.0.1\n", false) },
			restore:   repairs("infra-dagger"),
			failed:    observed,
		},
		{
			name: "edited runner slice",
			introduce: func() {
				command("docker", "exec", name, "sh", "-c", "echo >> /etc/systemd/system/infra-runners.slice")
				write("state/reload-units", "infra-runners.slice\n", false)
			},
			restore: reloadsOnly,
			failed:  observed,
		},
		{
			name:      "wrong CLI",
			introduce: func() { write("infra", "wrong", true) },
			restore:   func() { write("infra", cli, true) },
			failed:    observed,
		},
		{
			name: "undeclared runner",
			introduce: func() {
				write("runners/Z/.runner", "{}\n", false)
				write("runners/Z/.service", runnerUnit("Z")+"\n", false)
			},
			restore: func() { remove("runners/Z") },
			failed:  observed,
		},
		{
			name:      "pending runner replacement",
			introduce: func() { write("runners/Y-1/.infra-runner-pending", version+"\n", false) },
			restore:   func() { remove("runners/Y-1/.infra-runner-pending") },
			failed:    observed,
		},
		{
			name:      "foreign runner service",
			introduce: func() { write("runners/Y-1/.service", runnerUnit("infra")+"\n", false) },
			restore:   func() { write("runners/Y-1/.service", runnerUnit("Y")+"\n", false) },
			failed:    []string{"Verify registered runner services"},
		},
		{
			name:      "foreign owner runner service",
			introduce: func() { write("runners/Y-1/.service", "actions.runner.someone-Y.localhost-Y-1.service\n", false) },
			restore:   func() { write("runners/Y-1/.service", runnerUnit("Y")+"\n", false) },
			failed:    []string{"Verify registered runner services"},
		},
		{
			name: "no registered runners",
			introduce: func() {
				remove("runners/infra-1/.runner")
				remove("runners/Y-1/.runner")
			},
			restore: func() {
				write("runners/infra-1/.runner", "{}\n", false)
				write("runners/Y-1/.runner", "{}\n", false)
			},
			failed: observed,
		},
		{name: "repaired"},
	} {
		if scenario.introduce != nil {
			scenario.introduce()
		}
		before := restarted()
		output := run("/source/ansible/verify-runners.yml", scenario.failed == nil, scenario.extra...)
		changed, failed := outcome(output)
		if !slices.Equal(changed, scenario.changed) || !slices.Equal(failed, scenario.failed) {
			t.Fatalf("%s: changed %q and failed %q, want %q and %q:\n%s", scenario.name, changed, failed, scenario.changed, scenario.failed, output)
		}
		if restarted() != before {
			t.Fatalf("%s: verification restarted a service:\n%s", scenario.name, output)
		}
		if scenario.restore != nil {
			scenario.restore()
		}
		t.Logf("verification of %s fails %q", scenario.name, failed)
	}
	archive := func() {
		t.Helper()
		file, err := os.Create(filepath.Join(fixture, "runner.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		compressed := gzip.NewWriter(file)
		writer := tar.NewWriter(compressed)
		data := "#!/bin/sh\necho " + version + "\n"
		for _, header := range []*tar.Header{{Name: "./", Typeflag: tar.TypeDir, Mode: 0755}, {Name: "./bin/", Typeflag: tar.TypeDir, Mode: 0755}, {Name: "./bin/Runner.Listener", Mode: 0755, Size: int64(len(data))}} {
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
	withdrawal := "api --method DELETE --silent repos/" + fleet.Owner + "/infra/actions/runners/7/labels\n"
	restoration := "api --method PUT --silent repos/" + fleet.Owner + "/infra/actions/runners/7/labels --input -\n" + fmt.Sprintf(`{"labels": ["%s"]}`, strings.Join(fleet.Labels, `", "`)) + "\n"
	github := func(since string) string { return strings.TrimPrefix(record("gh"), since) }
	returned := func(calls string) bool {
		withdrawn := strings.LastIndex(calls, withdrawal)
		return withdrawn >= 0 && strings.HasSuffix(calls, restoration) && strings.LastIndex(calls, restoration) > withdrawn
	}
	untouched := func(scenario, since string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(fixture, "runners/infra-1/.infra-runner-pending")); record("stopped") != "" || !os.IsNotExist(err) {
			t.Fatalf("%s interrupted the runner: stopped %q, pending marker %v", scenario, record("stopped"), err)
		}
		if calls := github(since); !returned(calls) {
			t.Fatalf("%s did not return the runner to its jobs:\n%s", scenario, calls)
		}
	}
	calls := record("gh")
	write("state/withdrawal-failure", "", false)
	run("/fixture/converge.yml", false)
	untouched("failed withdrawal", calls)
	remove("state/withdrawal-failure")
	t.Log("a runner that cannot be withdrawn from new jobs is not replaced")
	write("state/github-busy", "", false)
	calls = record("gh")
	run("/fixture/converge.yml", false, "--extra-vars", `{"build_runner_drain_minutes":0,"build_runner_drain_interval":1}`)
	untouched("job accepted before withdrawal", calls)
	remove("state/github-busy")
	t.Log("a job GitHub already assigned keeps its runner before its worker starts")
	write("runners/infra-1/bin/Runner.Worker", "#!/bin/sh\nwhile [ -e /fixture/state/job-infra ]; do sleep 0.1; done\n", true)
	write("state/job-infra", "", false)
	command("docker", "exec", "-d", name, "/home/runner/infra-1/bin/Runner.Worker", "spawnclient", "1", "2")
	calls = record("gh")
	run("/fixture/converge.yml", false, "--extra-vars", `{"build_runner_drain_minutes":0,"build_runner_drain_interval":1}`)
	untouched("drain deadline", calls)
	t.Log("a runner still busy at the drain deadline keeps its job and its binaries and returns to new jobs")
	calls = record("gh")
	upgrade := exec.CommandContext(ctx, "docker", playbookArgs("/fixture/converge.yml", "--extra-vars", `{"build_runner_drain_interval":1}`)...)
	progress, err := upgrade.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	upgrade.Stderr = upgrade.Stdout
	if err := upgrade.Start(); err != nil {
		t.Fatal(err)
	}
	var upgradeOutput strings.Builder
	lines := bufio.NewScanner(progress)
	for lines.Scan() && !(strings.HasPrefix(lines.Text(), "FAILED - RETRYING: ") && strings.Contains(lines.Text(), "Wait for the runner to finish its job")) {
		upgradeOutput.WriteString(lines.Text() + "\n")
	}
	if record("stopped") != "" {
		t.Fatalf("upgrade stopped the runner during its job:\n%s", upgradeOutput.String())
	}
	if withdrawn := github(calls); !strings.Contains(withdrawn, withdrawal) || strings.Contains(withdrawn[strings.LastIndex(withdrawn, withdrawal):], "--method PUT") {
		t.Fatalf("draining runner still takes new jobs:\n%s", withdrawn)
	}
	remove("state/job-infra")
	rest, _ := io.ReadAll(progress)
	if err := upgrade.Wait(); err != nil {
		t.Fatalf("upgrade after the job finished: %v\n%s%s", err, upgradeOutput.String(), rest)
	}
	stopped := string(read(filepath.Join(fixture, "state/stopped")))
	if stopped != runnerUnit("infra")+"\n" {
		t.Fatalf("wrong service stopped: %q", stopped)
	}
	if busy := record("stopped-mid-job"); busy != "" {
		t.Fatalf("upgrade stopped %q while a job was running", busy)
	}
	if replaced := github(calls); !returned(replaced) {
		t.Fatalf("upgraded runner did not return to new jobs:\n%s", replaced)
	}
	t.Log("runner upgrade takes the runner out of new jobs, waits for its running job and returns it after the replacement")
	if output := run("/source/ansible/verify-runners.yml", true); !strings.Contains(output, "changed=0") {
		t.Fatalf("verification after runner upgrade reports drift:\n%s", output)
	}
	output := run("/fixture/converge.yml", true)
	if !strings.Contains(output, "changed=0") || string(read(filepath.Join(fixture, "state/stopped"))) != stopped {
		t.Fatalf("runner replacement is not idempotent:\n%s", output)
	}
	t.Log("runner upgrade stops only its own service and converges idempotently")
	write("runners/infra-1/bin/Runner.Listener", "#!/bin/sh\necho 0.0.0\n", true)
	write("runner.tar.gz", "invalid archive", false)
	calls = record("gh")
	run("/fixture/converge.yml", false)
	pending := filepath.Join(fixture, "runners/infra-1/.infra-runner-pending")
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("partial replacement lost its recovery marker", err)
	}
	if failed := github(calls); !returned(failed) {
		t.Fatalf("failed replacement did not return the runner to new jobs:\n%s", failed)
	}
	archive()
	converge()
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
	removal, err := yaml.Marshal([]any{map[string]any{"hosts": "build_engines", "gather_facts": false, "vars": map[string]any{"build_runner_fleet": "{{ lookup('ansible.builtin.file', '/source/build/runners.json') | from_json }}", "build_runner_drain_interval": 1}, "vars_files": []string{"/source/ansible/roles/build_runner/defaults/main.yml"}, "tasks": removalTasks}})
	if err != nil {
		t.Fatal(err)
	}
	write("removal.yml", string(removal), false)
	declaredUnit := "/etc/systemd/system/" + runnerUnit("infra")
	retiredService := "actions.runner." + fleet.Owner + "-retired.localhost-retired.service"
	retiredUnit := "/etc/systemd/system/" + retiredService
	for _, unit := range []string{declaredUnit, retiredUnit} {
		command("docker", "exec", name, "mkdir", "-p", unit+".d")
		command("docker", "exec", name, "touch", unit, unit+".d/resources.conf")
	}
	exists := func(path string) bool {
		return exec.CommandContext(ctx, "docker", "exec", name, "test", "-e", path).Run() == nil
	}
	remove("state/stopped")
	remove("state/reloaded")
	write("runners/infra.bak/.runner", "{}", false)
	write("runners/infra.bak/.service", string(read(filepath.Join(fixture, "runners/infra-1/.service"))), false)
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
	write("runners/retired/.runner", runnerRegistration("retired", "retired", 11), false)
	write("runners/retired/.service", "actions.runner.someone-retired.localhost-retired.service", false)
	if output := run("/fixture/removal.yml", false); !strings.Contains(output, "Assertion failed") {
		t.Fatalf("runner root naming another owner's service did not fail its identity check:\n%s", output)
	}
	if record("stopped") != "" || record("disabled") != "" || !exists(retiredUnit) {
		t.Fatal("runner root naming another owner's service changed a service")
	}
	t.Log("runner root naming another owner's service fails closed")
	outside := slices.DeleteFunc(fleet.RepositoryNames(), func(repository string) bool { return repository == "infra" || repository == "Y" })
	if len(outside) == 0 {
		t.Fatal("fleet declares no repository outside the host subset")
	}
	kept := []string{"runners/.runner", "runners/.hidden/.runner", "runners/infra-1/_work/x/.runner", "runners/unregistered/config.sh", "runners/" + outside[0] + "-1/.runner", "runners/infra-1/.runner", "runners/Y-1/.runner"}
	for _, path := range kept {
		write(path, "{}", false)
	}
	write("runners/retired/.runner", runnerRegistration("retired", "retired", 11), false)
	write("runners/retired/.service", retiredService, false)
	write("runners/infra/.runner", runnerRegistration("infra", "infra", 12), false)
	calls = record("gh")
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
	for _, retired := range []string{"retired", "infra"} {
		if _, err := os.Stat(filepath.Join(fixture, "runners", retired)); !os.IsNotExist(err) {
			t.Fatal("retired runner root remains", err)
		}
	}
	for _, call := range []string{"api --method DELETE --silent repos/" + fleet.Owner + "/retired/actions/runners/11/labels", "api repos/" + fleet.Owner + "/retired/actions/runners/11 --jq .busy", "api --method DELETE --silent repos/" + fleet.Owner + "/retired/actions/runners/11\n", "api --method DELETE --silent repos/" + fleet.Owner + "/infra/actions/runners/12\n"} {
		if !strings.Contains(github(calls), call) {
			t.Fatalf("retired runners were not drained and deregistered at GitHub (%q):\n%s", call, github(calls))
		}
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
	write("runners/Y/.runner", runnerRegistration("Y", "Y", 13), false)
	write("runners/Y/bin/Runner.Worker", "#!/bin/sh\nwhile [ -e /fixture/state/job-Y ]; do sleep 0.1; done\n", true)
	write("state/job-Y", "", false)
	command("docker", "exec", "-d", name, "/home/runner/Y/bin/Runner.Worker", "spawnclient", "1", "2")
	calls = record("gh")
	run("/fixture/removal.yml", false, "--extra-vars", `{"build_runner_drain_minutes":0}`)
	if _, err := os.Stat(filepath.Join(fixture, "runners/Y/.runner")); err != nil || strings.Contains(github(calls), "repos/"+fleet.Owner+"/Y/actions/runners/13\n") || !strings.Contains(github(calls), "repos/"+fleet.Owner+"/Y/actions/runners/13/labels") {
		t.Fatalf("retired runner busy at the drain deadline lost its job or kept taking new jobs: %v\n%s", err, github(calls))
	}
	remove("state/job-Y")
	run("/fixture/removal.yml", true)
	if _, err := os.Stat(filepath.Join(fixture, "runners/Y")); !os.IsNotExist(err) || !strings.Contains(github(calls), "api --method DELETE --silent repos/"+fleet.Owner+"/Y/actions/runners/13\n") {
		t.Fatalf("retired runner was not removed after its job: %v\n%s", err, github(calls))
	}
	t.Log("a retired runner stops taking jobs, keeps its running job and is deregistered after it finishes")
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
	write("bin/sops", "#!/bin/sh\nprintf 'private_key: declared-key\\ntoken: declared-token\\n'\n", true)
	write("app-key.sops.yaml", "", false)
	write("heartbeat.sops.yaml", "", false)
	write("trigger.yml", `- hosts: build_engines
  gather_facts: false
  vars:
    verification_trigger_app_id: 1
    verification_trigger_installation_id: 2
    verification_trigger_credentials:
    - {source: /fixture/app-key.sops.yaml, field: private_key, dest: /etc/infra-verification/github-app.pem}
    - {source: /fixture/heartbeat.sops.yaml, field: token, dest: /etc/infra-verification/gatus-token}
  roles:
  - verification_trigger
`, false)
	trigger := func(success bool, extra ...string) string {
		return run("/fixture/trigger.yml", success, append([]string{"--skip-tags=infra_binary"}, extra...)...)
	}
	if output := trigger(true, "--diff"); strings.Contains(output, "declared-key") || strings.Contains(output, "declared-token") {
		t.Fatalf("verification trigger printed a credential:\n%s", output)
	}
	for credential, size := range map[string]int{"github-app.pem": 12, "gatus-token": 14} {
		if installed := command("docker", "exec", name, "stat", "-c", "%a %U %s", "/etc/infra-verification/"+credential); string(installed) != fmt.Sprintf("600 root %d\n", size) {
			t.Fatalf("%s installed as %q", credential, installed)
		}
	}
	for _, mode := range [][]string{nil, {"--check"}} {
		if output := trigger(true, mode...); !strings.Contains(output, "changed=0") {
			t.Fatalf("verification trigger %v is not idempotent:\n%s", mode, output)
		}
	}
	command("docker", "exec", name, "sed", "-i", "s/OnCalendar=hourly/OnCalendar=daily/", "/etc/systemd/system/infra-verification-request.timer")
	if changed, _ := outcome(trigger(true, "--check")); !slices.Equal(changed, []string{"verification_trigger : Install the verification request units", "verification_trigger : Restart the verification request timer"}) {
		t.Fatalf("edited verification timer reported as %q", changed)
	}
	before = restarted()
	trigger(true)
	if got := strings.TrimPrefix(restarted(), before); got != "infra-verification-request.timer\n" {
		t.Fatalf("timer repair restarted %q", got)
	}
	command("docker", "exec", name, "rm", "/fixture/heartbeat.sops.yaml")
	if output := trigger(false, "--check"); !strings.Contains(output, "Missing SOPS-encrypted credential /fixture/heartbeat.sops.yaml") {
		t.Fatalf("missing encrypted credential did not fail clearly:\n%s", output)
	}
	t.Log("verification trigger installs its credentials privately, converges idempotently, restarts an edited timer and requires its encrypted credentials")
}

func TestHostComparisonWithLocalContainer(t *testing.T) {
	image := os.Getenv("INFRA_ANSIBLE_TEST_IMAGE")
	if image == "" {
		t.Skip("requires a local Linux container image with Python matching the Ansible environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return string(output)
	}
	root := strings.TrimSpace(command("git", "rev-parse", "--show-toplevel"))
	packages := os.Getenv("INFRA_ANSIBLE_SITE_PACKAGES")
	if packages == "" {
		matches, err := filepath.Glob(filepath.Join(root, ".venv/lib/python*/site-packages"))
		if err != nil || len(matches) != 1 {
			t.Fatal("set INFRA_ANSIBLE_SITE_PACKAGES to the local Ansible Python package directory")
		}
		packages = matches[0]
	}
	fixture := t.TempDir()
	for path, data := range map[string]string{
		"ansible/ansible.cfg":              "[defaults]\nroles_path = /source/ansible/roles\nstrategy_plugins = /source/ansible/plugins/strategy\nstrategy = mitogen_linear\nretry_files_enabled = False\n",
		"ansible/inventory/production.yml": "all:\n  hosts:\n    localhost:\n      ansible_connection: local\n      ansible_user: root\n      ansible_python_interpreter: /usr/bin/python3\n",
		"ansible/reconcile.yml":            "- name: Declare patching\n  hosts: all\n  gather_facts: false\n  roles: [host_patching]\n- name: Declare host enrollment\n  hosts: all\n  gather_facts: false\n  tasks:\n  - name: Inspect the enrolled host\n    ansible.builtin.stat:\n      path: /etc/infra-host.enrolled\n    register: host_enrollment\n  - name: Require the enrolled host\n    ansible.builtin.assert:\n      that: host_enrollment.stat.exists\n- name: Register runners\n  hosts: all\n  gather_facts: false\n  tags: [runners]\n  tasks:\n  - name: Reject runner comparison\n    ansible.builtin.fail:\n      msg: runner play compared\n",
		"ansible/external.yml":             "- name: Declare monitor\n  hosts: all\n  gather_facts: false\n  tasks:\n  - name: Inspect the enrolled monitor\n    ansible.builtin.stat:\n      path: /etc/infra-monitor.enrolled\n    register: monitor_enrollment\n  - name: Require the enrolled monitor\n    ansible.builtin.assert:\n      that: monitor_enrollment.stat.exists\n  - name: Install monitor settings\n    ansible.builtin.copy:\n      dest: /etc/infra-monitor.conf\n      content: \"declared\\n\"\n",
	} {
		if err := os.MkdirAll(filepath.Join(fixture, filepath.Dir(path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture, path), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	name := fmt.Sprintf("infra-host-comparison-%d", time.Now().UnixNano())
	command("docker", "run", "-d", "--name", name, "--network=none", "-v", root+":/source:ro", "-v", fixture+":"+fixture, "-v", packages+":/opt/ansible:ro", "-e", "PYTHONPATH=/opt/ansible", image, "sleep", "infinity")
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", name, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), fixture).Run()
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	var mu sync.Mutex
	var output strings.Builder
	commands := Commands{Work: fixture, Runner: ci.Runner{Dir: fixture, Execute: func(ctx context.Context, options process.Options) (process.Result, error) {
		args := []string{"exec", "-w", options.Dir}
		for _, entry := range options.Env {
			if strings.HasPrefix(entry, "ANSIBLE_") || strings.HasPrefix(entry, "JUNIT_") {
				args = append(args, "-e", entry)
			}
		}
		result, err := exec.CommandContext(ctx, "docker", append(append(args, name, "python3", "-m", "ansible.cli.playbook"), options.Args...)...).CombinedOutput()
		mu.Lock()
		output.Write(result)
		mu.Unlock()
		if options.Stdout != nil {
			_, _ = options.Stdout.Write(result)
		}
		return process.Result{Stdout: result}, err
	}}}
	converge := func() {
		t.Helper()
		command("docker", "exec", "-w", filepath.Join(fixture, "ansible"), "-e", "ANSIBLE_CONFIG="+filepath.Join(fixture, "ansible/ansible.cfg"), name, "python3", "-m", "ansible.cli.playbook", "-i", "inventory/production.yml", "reconcile.yml", "external.yml", "--skip-tags=runners")
	}
	compare := func(differences []Difference, errors []string) {
		t.Helper()
		outcome := VerificationOutcome("", true, commands.compareHosts(ctx))
		if !reflect.DeepEqual(outcome.Differences, append([]Difference{}, differences...)) || !reflect.DeepEqual(outcome.Errors, append([]string{}, errors...)) {
			t.Fatalf("comparison reported %+v, want differences %+v and errors %q\n%s", outcome, differences, errors, output.String())
		}
		output.Reset()
	}
	command("docker", "exec", name, "touch", "/etc/infra-monitor.enrolled", "/etc/infra-host.enrolled")
	converge()
	compare(nil, nil)
	t.Log("converged host matches its declaration in check mode")
	command("docker", "exec", name, "sh", "-c", `echo 'APT::Periodic::Unattended-Upgrade "0";' >> /etc/apt/apt.conf.d/20auto-upgrades && echo drift > /etc/infra-monitor.conf`)
	compare([]Difference{
		{System: "hosts", Host: "localhost", Item: "Declare patching: host_patching : Enable unattended security updates"},
		{System: "hosts", Host: "localhost", Item: "Declare monitor: Install monitor settings"},
	}, nil)
	if monitor := command("docker", "exec", name, "cat", "/etc/infra-monitor.conf"); monitor != "drift\n" {
		t.Fatalf("check mode repaired the monitor settings: %q", monitor)
	}
	t.Log("edited managed files are reported once as differences without being repaired")
	command("docker", "exec", name, "rm", "/etc/infra-monitor.enrolled")
	compare([]Difference{{System: "hosts", Host: "localhost", Item: "Declare patching: host_patching : Enable unattended security updates"}}, []string{"external.yml comparison incomplete: host tasks failed: [localhost] Declare monitor: Require the enrolled monitor"})
	t.Log("failed requirements are reported as errors, not differences")
	command("docker", "exec", name, "sh", "-c", "touch /etc/infra-monitor.enrolled && rm /etc/infra-host.enrolled")
	compare([]Difference{
		{System: "hosts", Host: "localhost", Item: "Declare patching: host_patching : Enable unattended security updates"},
		{System: "hosts", Host: "localhost", Item: "Declare monitor: Install monitor settings"},
	}, []string{"reconcile.yml comparison incomplete: host tasks failed: [localhost] Declare host enrollment: Require the enrolled host"})
	t.Log("a failing reconcile.yml host leaves the monitor playbook compared")
	command("docker", "exec", name, "touch", "/etc/infra-host.enrolled")
	converge()
	compare(nil, nil)
	t.Log("repair converges the host back to a matching comparison")
}
