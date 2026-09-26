package reconcile

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const fakeK3sInstaller = `#!/bin/sh
set -eu
test -z "${HOME:-}"
service=k3s
if [ "$INSTALL_K3S_EXEC" != server ]; then service=k3s-$INSTALL_K3S_EXEC; fi
echo "$INSTALL_K3S_EXEC" >> /fixture/installs
rm -f "/etc/systemd/system/$service.service" "/etc/systemd/system/$service.service.env"
printf '[Service]\nExecStart=/usr/local/bin/k3s %s\n' "$INSTALL_K3S_EXEC" > "/etc/systemd/system/$service.service"
: > "/etc/systemd/system/$service.service.env"
printf '#!/bin/sh\n# %s\n' "$VERSION" > /usr/local/bin/k3s-killall.sh
printf '#!/bin/sh\n# %s\n' "$service" > "/usr/local/bin/$service-uninstall.sh"
`

var recapChanges = regexp.MustCompile(`(?m)^localhost\s+:\s+ok=\d+\s+changed=(\d+)\s+unreachable=0\s+failed=0\s`)

func TestK3sUpstreamServiceInstallsOnlyOnDifferencesWithLocalContainer(t *testing.T) {
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
	packages := localAnsiblePackages(t, root)
	fixture := t.TempDir()
	for path, data := range map[string]string{
		"ansible.cfg":   "[defaults]\nroles_path = /source/ansible/roles\nretry_files_enabled = False\n",
		"inventory.yml": "all:\n  hosts:\n    localhost:\n      ansible_connection: local\n      ansible_python_interpreter: /usr/bin/python3\n",
		"converge.yml":  "- hosts: all\n  gather_facts: false\n  tasks:\n  - ansible.builtin.import_role:\n      name: k3s\n      tasks_from: upstream-service.yml\n",
	} {
		if err := os.WriteFile(filepath.Join(fixture, path), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	name := fmt.Sprintf("infra-k3s-upstream-%d", time.Now().UnixNano())
	command("docker", "run", "-d", "--name", name, "--network=none", "-v", root+":/source:ro", "-v", fixture+":/fixture", "-v", packages+":/opt/ansible:ro", "-e", "PYTHONPATH=/opt/ansible", "-e", "ANSIBLE_CONFIG=/fixture/ansible.cfg", image, "sleep", "infinity")
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", name, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/fixture").Run()
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	installer := func(version string) string {
		t.Helper()
		script := strings.Replace(fakeK3sInstaller, "set -eu\n", "set -eu\nVERSION="+version+"\n", 1)
		if err := os.WriteFile(filepath.Join(fixture, "installer.sh"), []byte(script), 0644); err != nil {
			t.Fatal(err)
		}
		command("docker", "exec", name, "install", "-m", "0700", "/fixture/installer.sh", "/usr/local/sbin/k3s-install.sh")
		return fmt.Sprintf("%x", sha256.Sum256([]byte(script)))
	}
	command("docker", "exec", name, "mkdir", "-p", "-m", "0700", "/etc/rancher/k3s")
	checksum := installer("v1")
	converge := func(role string, extra ...string) int {
		t.Helper()
		args := append([]string{"exec", name, "python3", "-m", "ansible.cli.playbook", "-i", "/fixture/inventory.yml", "/fixture/converge.yml", "-e", "k3s_role=" + role, "-e", "k3s_version=v0.0.0", "-e", "k3s_install_sha256=" + checksum}, extra...)
		output := command(append([]string{"docker"}, args...)...)
		match := recapChanges.FindStringSubmatch(output)
		if match == nil {
			t.Fatalf("no recap:\n%s", output)
		}
		var changes int
		fmt.Sscan(match[1], &changes)
		return changes
	}
	installs := func() int {
		data, err := os.ReadFile(filepath.Join(fixture, "installs"))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Count(string(data), "\n")
	}
	expect := func(step string, changes, changed, installed int) {
		t.Helper()
		if changes != changed || installs() != installed {
			t.Fatalf("%s: %d changes and %d installs, want %d and %d", step, changes, installs(), changed, installed)
		}
	}
	if changes := converge("server"); changes == 0 || installs() != 1 {
		t.Fatalf("first convergence: %d changes and %d installs", changes, installs())
	}
	expect("unchanged service", converge("server"), 0, 1)
	expect("unchanged service in check mode", converge("server", "--check"), 0, 1)
	t.Log("an installed service is neither reinstalled nor reported as changed")
	command("docker", "exec", name, "sh", "-c", "echo drift >> /etc/systemd/system/k3s.service")
	expect("edited unit in check mode", converge("server", "--check"), 1, 1)
	if changes := converge("server"); changes == 0 || installs() != 2 {
		t.Fatalf("edited unit repair: %d changes and %d installs", changes, installs())
	}
	expect("repaired unit", converge("server"), 0, 2)
	command("docker", "exec", name, "rm", "/usr/local/bin/k3s-killall.sh")
	if converge("server"); installs() != 3 {
		t.Fatalf("missing helper not reinstalled: %d installs", installs())
	}
	t.Log("edited or missing upstream files are reported in check mode and reinstalled")
	if converge("agent"); installs() != 4 {
		t.Fatalf("role change not installed: %d installs", installs())
	}
	expect("installed agent", converge("agent"), 0, 4)
	checksum = installer("v2")
	if converge("agent"); installs() != 5 {
		t.Fatalf("installer upgrade not applied: %d installs", installs())
	}
	expect("upgraded installer", converge("agent"), 0, 5)
	t.Log("role changes and installer upgrades reinstall once")
}
