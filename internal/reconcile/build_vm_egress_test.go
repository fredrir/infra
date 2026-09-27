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
)

const egressProbe = `import os, pwd, socket, sys
mode, user, host, port = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
account = pwd.getpwnam(user)
os.setgid(account.pw_gid)
os.setuid(account.pw_uid)
if mode == "serve":
    listener = socket.create_server((host, port))
    open("/tmp/ready-%d" % port, "w").close()
    connection, _ = listener.accept()
    connection.sendall(b"ok")
    connection.close()
    sys.exit(0)
try:
    connection = socket.create_connection((host, port), timeout=2)
    print(connection.recv(2).decode() or "open")
except socket.timeout:
    print("dropped")
except OSError as error:
    print(type(error).__name__)
`

func TestBuildVmEgressFilterDropsOnlyConnectionsTheGuestOpens(t *testing.T) {
	image := os.Getenv("INFRA_NETWORK_TEST_IMAGE")
	if image == "" {
		t.Skip("INFRA_NETWORK_TEST_IMAGE names a local image with nft, python3 and adduser for a privileged network namespace")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return strings.TrimSpace(string(output))
	}
	source, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(source))
	fixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixture, "probe.py"), []byte(egressProbe), 0644); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	public, private := "infra-egress-public-"+suffix, "infra-egress-private-"+suffix
	run("network", "create", "--subnet", "198.51.100.0/24", public)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", public).Run() })
	run("network", "create", "--internal", "--subnet", "172.29.254.0/24", private)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", private).Run() })
	host, peer := "infra-egress-host-"+suffix, "infra-egress-peer-"+suffix
	for _, container := range []struct{ name, publicAddress, privateAddress string }{{host, "198.51.100.2", "172.29.254.2"}, {peer, "198.51.100.3", "172.29.254.3"}} {
		run("create", "--name", container.name, "--privileged", "--network", public, "--ip", container.publicAddress,
			"-v", filepath.Join(root, "ansible/roles/build_vm/files")+":/rules:ro", "-v", fixture+":/fixture:ro", "--entrypoint", "sleep", image, "infinity")
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container.name).Run() })
		run("network", "connect", "--ip", container.privateAddress, private, container.name)
		run("start", container.name)
		run("exec", container.name, "adduser", "-D", "infra-build-vm")
		run("exec", container.name, "adduser", "-D", "other")
	}
	run("exec", host, "sh", "-c", `ip link add nodelocaldns type dummy && ip address add 169.254.20.10/32 dev nodelocaldns && ip link set nodelocaldns up &&
nft -f - <<'EOF'
table ip raw {
  chain output {
    type filter hook output priority raw; policy accept;
    ip daddr 169.254.20.10 tcp dport 53 notrack
  }
}
EOF`)
	rules, err := os.ReadFile(filepath.Join(root, "ansible/roles/build_vm/files/infra-build-vm-egress.nft"))
	if err != nil {
		t.Fatal(err)
	}
	widened := strings.Replace(string(rules), "192.168.0.0/16 }", "192.168.0.0/16, 203.0.113.0/24 }", 1)
	if err := os.WriteFile(filepath.Join(fixture, "widened.nft"), []byte(widened), 0644); err != nil {
		t.Fatal(err)
	}
	run("exec", host, "nft", "-f", "/fixture/widened.nft")
	run("exec", host, "nft", "-f", "/rules/infra-build-vm-egress.nft")
	if set := run("exec", host, "nft", "list", "set", "inet", "infra_build_vm", "guest_denied_ipv4"); strings.Contains(set, "203.0.113.0/24") {
		t.Fatalf("reloading the filter kept an element removed from its definition:\n%s", set)
	}
	serve := func(container, user, address string, port int) {
		t.Helper()
		run("exec", "-d", container, "python3", "/fixture/probe.py", "serve", user, address, fmt.Sprint(port))
		for deadline := time.Now().Add(10 * time.Second); exec.CommandContext(ctx, "docker", "exec", container, "test", "-e", fmt.Sprintf("/tmp/ready-%d", port)).Run() != nil; {
			if time.Now().After(deadline) {
				t.Fatalf("%s listener on %s:%d did not start", container, address, port)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	connect := func(container, user, address string, port int) string {
		t.Helper()
		return run("exec", container, "python3", "/fixture/probe.py", "connect", user, address, fmt.Sprint(port))
	}
	for _, check := range []struct {
		listener, listenerUser, address string
		port                            int
		client, clientUser, want        string
	}{
		{host, "root", "127.0.0.1", 18080, host, "infra-build-vm", "dropped"},
		{host, "root", "198.51.100.2", 18081, host, "infra-build-vm", "dropped"},
		{host, "root", "172.29.254.2", 18082, host, "infra-build-vm", "dropped"},
		{peer, "root", "172.29.254.3", 18083, host, "infra-build-vm", "dropped"},
		{host, "root", "169.254.20.10", 53, host, "infra-build-vm", "dropped"},
		{host, "root", "127.0.0.53", 53, host, "infra-build-vm", "ok"},
		{peer, "root", "198.51.100.3", 18084, host, "infra-build-vm", "ok"},
		{host, "root", "127.0.0.1", 18085, host, "other", "ok"},
		{host, "infra-build-vm", "127.0.0.1", 2222, host, "root", "ok"},
		{host, "infra-build-vm", "172.29.254.2", 9101, peer, "root", "ok"},
	} {
		serve(check.listener, check.listenerUser, check.address, check.port)
		if got := connect(check.client, check.clientUser, check.address, check.port); got != check.want {
			t.Errorf("%s connecting to %s:%d served by %s got %s, want %s", check.clientUser, check.address, check.port, check.listenerUser, got, check.want)
		}
	}
}
