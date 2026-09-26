package reconcile

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHostSecretsWithSystemdContainer(t *testing.T) {
	image := os.Getenv("INFRA_SYSTEMD_TEST_IMAGE")
	if image == "" {
		t.Skip("requires a local Ubuntu image that boots systemd from /sbin/init and provides python3, age and restic")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
	root := strings.TrimSpace(must("git", "rev-parse", "--show-toplevel"))
	packages := localAnsiblePackages(t, root)
	fixture := t.TempDir()
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fixture, path), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("inventory.yml", "all:\n  hosts:\n    localhost:\n      ansible_connection: local\n      ansible_user: root\n      ansible_python_interpreter: /usr/bin/python3\n")
	write("host.yml", "- hosts: all\n  gather_facts: false\n  roles: [host_secrets]\n")

	name := fmt.Sprintf("infra-host-secrets-%d", time.Now().UnixNano())
	must("docker", "run", "-d", "--name", name, "--privileged", "--cgroupns=private", "--network=none", "--tmpfs", "/run", "--tmpfs", "/run/lock", "-v", root+":/source:ro", "-v", fixture+":/fixture", "-v", packages+":/opt/ansible:ro", image, "/sbin/init")
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", name, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/fixture").Run()
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	guest := func(script string) (string, error) { return run("docker", "exec", name, "bash", "-euc", script) }
	deadline := time.Now().Add(time.Minute)
	for state, _ := guest("systemctl is-system-running"); !strings.HasPrefix(state, "running") && !strings.HasPrefix(state, "degraded"); state, _ = guest("systemctl is-system-running") {
		if time.Now().After(deadline) {
			t.Fatalf("systemd did not boot: %q", state)
		}
		time.Sleep(time.Second)
	}
	play := func(success bool, playbook string, extra ...string) string {
		t.Helper()
		output, err := run(append([]string{"docker", "exec", "-e", "PYTHONPATH=/opt/ansible", "-e", "PYTHONUNBUFFERED=1", "-e", "ANSIBLE_CONFIG=/source/ansible/ansible.cfg", name, "python3", "-m", "ansible.cli.playbook", "-i", "/fixture/inventory.yml", playbook}, extra...)...)
		if (err == nil) != success {
			t.Fatalf("%s success=%t: %v\n%s", playbook, success, err, output)
		}
		if strings.Contains(output, "AGE-SECRET-KEY-") {
			t.Fatalf("%s printed a private age key", playbook)
		}
		return output
	}
	changed := func(output string) []string {
		var task string
		var tasks []string
		for line := range strings.SplitSeq(output, "\n") {
			switch {
			case strings.HasPrefix(line, "TASK ["), strings.HasPrefix(line, "RUNNING HANDLER ["):
				task = line[strings.Index(line, "[")+1 : strings.LastIndex(line, "]")]
			case strings.HasPrefix(line, "changed: "):
				tasks = append(tasks, task)
			}
		}
		return slices.Compact(tasks)
	}
	recipientPattern := regexp.MustCompile(`^age1[02-9ac-hj-np-z]{58}$`)
	recipient := func() string {
		t.Helper()
		value := strings.TrimSpace(must("docker", "exec", name, "age-keygen", "-y", "/etc/age/host.key"))
		if !recipientPattern.MatchString(value) {
			t.Fatalf("host key yields no age recipient: %q", value)
		}
		return value
	}

	play(true, "/fixture/host.yml")
	if modes := must("docker", "exec", name, "stat", "-c", "%a %U %G", "/etc/age", "/etc/age/host.key"); modes != "700 root root\n600 root root\n" {
		t.Fatalf("host key is not root-only: %q", modes)
	}
	enrolled := recipient()
	for _, mode := range [][]string{nil, {"--check"}} {
		if output := play(true, "/fixture/host.yml", mode...); len(changed(output)) != 0 || recipient() != enrolled {
			t.Fatalf("host key %v is not stable: changed %q", mode, changed(output))
		}
	}
	t.Log("the host generates a root-only age key once and keeps its recipient across runs and check mode")

	must("docker", "exec", name, "rm", "/etc/age/host.key")
	if output := play(false, "/fixture/host.yml", "--check"); !slices.Equal(changed(output), []string{"host_secrets : Generate the host age key"}) || !strings.Contains(output, "TASK [host_secrets : Read the host age recipient]") {
		t.Fatalf("missing host key reported as %q", changed(output))
	}
	if _, err := guest("test ! -e /etc/age/host.key"); err != nil {
		t.Fatal("check mode generated a host key")
	}
	play(true, "/fixture/host.yml")
	if recipient() == enrolled {
		t.Fatal("a regenerated host key kept the lost recipient")
	}
	t.Log("check mode reports a missing host key as a difference and fails to read its recipient without creating one, and a lost key is replaced by a new recipient")
}
