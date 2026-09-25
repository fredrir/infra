package reconcile

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestCgroupMetricsWithSystemdContainer(t *testing.T) {
	image := os.Getenv("INFRA_SYSTEMD_TEST_IMAGE")
	if image == "" {
		t.Skip("requires a local Ubuntu image that boots systemd from /sbin/init")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run := func(args ...string) (string, error) {
		output, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		return string(output), err
	}
	must := func(args ...string) string {
		t.Helper()
		output, err := run(args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return output
	}
	load := func(path string, value any) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err == nil {
			err = yaml.Unmarshal(data, value)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	root := strings.TrimSpace(must("git", "rev-parse", "--show-toplevel"))
	var state []map[string]any
	var defaults map[string]any
	load(filepath.Join(root, "ansible/roles/build_runner/tasks/state.yml"), &state)
	load(filepath.Join(root, "ansible/roles/build_runner/defaults/main.yml"), &defaults)
	var runnerSlice, collectorUnit string
	for _, task := range state {
		switch task["name"] {
		case "Install aggregate runner resource limits":
			runnerSlice = task["ansible.builtin.copy"].(map[string]any)["content"].(string)
		case "Install guest metric units":
			for _, item := range task["loop"].([]any) {
				if unit := item.(map[string]any); unit["name"] == "infra-cgroup-metrics" {
					collectorUnit = unit["content"].(string)
				}
			}
		}
	}
	runnerSlice = strings.NewReplacer("{{ build_runner_cpus_percent }}", fmt.Sprint(defaults["build_runner_cpus_percent"]), "{{ build_runner_memory }}", fmt.Sprint(defaults["build_runner_memory"])).Replace(runnerSlice)
	if collectorUnit == "" || runnerSlice == "" || strings.Contains(runnerSlice, "{{") {
		t.Fatalf("declared collector unit or runner slice not found:\n%s", runnerSlice)
	}

	name := fmt.Sprintf("infra-cgroup-metrics-%d", time.Now().UnixNano())
	must("docker", "run", "-d", "--name", name, "--privileged", "--cgroupns=private", "--tmpfs", "/run", "--tmpfs", "/run/lock", "-v", root+":/source:ro", image, "/sbin/init")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	guest := func(script string) (string, error) { return run("docker", "exec", name, "bash", "-euc", script) }
	deadline := time.Now().Add(time.Minute)
	for state, _ := guest("systemctl is-system-running"); !strings.HasPrefix(state, "running") && !strings.HasPrefix(state, "degraded"); state, _ = guest("systemctl is-system-running") {
		if time.Now().After(deadline) {
			t.Fatalf("systemd did not boot: %q", state)
		}
		time.Sleep(time.Second)
	}
	if output, err := guest(fmt.Sprintf(`cat > /etc/systemd/system/infra-runners.slice <<'EOF'
%sEOF
cat > /etc/systemd/system/infra-cgroup-metrics.service <<'EOF'
%sEOF
mkdir -p /var/lib/node_exporter/textfile_collector
install -D -m 0555 /source/ansible/roles/build_runner/files/infra-cgroup-metrics /usr/local/libexec/infra-cgroup-metrics
systemctl daemon-reload
systemd-run --quiet --unit=actions.runner.fixture --slice=infra-runners.slice sleep infinity
systemd-run --quiet --unit=infra-dagger-fixture --slice=infra-engine.slice -p MemoryMax=256M -p CPUQuota=100%% sleep infinity
systemctl start infra-cgroup-metrics`, runnerSlice, collectorUnit)); err != nil {
		t.Fatalf("guest setup: %v\n%s", err, output)
	}

	contains := func(want ...string) func(string) bool {
		return func(output string) bool {
			for _, line := range want {
				if !strings.Contains(output, line) {
					return false
				}
			}
			return true
		}
	}
	await := func(ready func(string) bool) string {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for {
			output, _ := guest("cat /var/lib/node_exporter/textfile_collector/infra-cgroups.prom")
			if ready(output) {
				return output
			}
			if time.Now().After(deadline) {
				journal, _ := guest("journalctl -u infra-cgroup-metrics --no-pager")
				t.Fatalf("collector output never became ready:\n%s\n%s", output, journal)
			}
			time.Sleep(time.Second)
		}
	}
	first := await(contains(
		`infra_cgroup_memory_max_bytes{cgroup="infra-engine.slice"} 268435456`,
		`infra_cgroup_memory_max_bytes{cgroup="infra-runners.slice"} 2147483648`,
		`infra_cgroup_cpu_max_cores{cgroup="infra-engine.slice"} 1`,
		`infra_cgroup_cpu_max_cores{cgroup="infra-runners.slice"} 4`,
		`infra_cgroup_memory_events_total{cgroup="infra-engine.slice",event="oom_kill"} 0`,
		"infra_cgroup_collector_read_errors 0",
		"\ninfra_cgroup_collector_last_success_timestamp_seconds "))
	t.Log("the declared collector reports engine and runner slice limits from real cgroups, including runner services without cpu.max")

	if output, err := guest(`systemctl stop infra-dagger-fixture
systemd-run --quiet --wait --unit=infra-dagger-oom --slice=infra-engine.slice -p MemoryMax=32M -p MemorySwapMax=0 tail /dev/zero || true`); err != nil {
		t.Fatalf("engine OOM fixture: %v\n%s", err, output)
	}
	stamp := first[strings.Index(first, "\ninfra_cgroup_collector_last_success_timestamp_seconds "):]
	stamp = stamp[:strings.Index(stamp[1:], "\n")+1]
	after := await(func(output string) bool {
		return !strings.Contains(output, stamp) && contains(
			`infra_cgroup_memory_events_total{cgroup="infra-engine.slice",event="oom_kill"} 1`,
			`infra_cgroup_memory_max_bytes{cgroup="infra-runners.slice"} 2147483648`,
			"infra_cgroup_collector_read_errors 0")(output)
	})
	if strings.Contains(after, `infra_cgroup_memory_max_bytes{cgroup="infra-engine.slice"}`) {
		t.Fatalf("stopped engine still reports a memory limit:\n%s", after)
	}
	if output, _ := guest("systemctl is-active infra-cgroup-metrics"); strings.TrimSpace(output) != "active" {
		t.Fatalf("collector stopped: %s", output)
	}
	t.Log("an engine OOM kill stays counted on its slice after the engine is gone, and the writer keeps running")
}
