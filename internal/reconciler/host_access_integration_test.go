package reconciler

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unavailable(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("INFRA_HOST_ACCESS_TEST") == "required" {
		t.Fatal(reason)
	}
	t.Skip(reason)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "ansible", "plugins", "strategy")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			unavailable(t, "requires the repository's Ansible configuration")
		}
		directory = parent
	}
}

func hopTools(t *testing.T, root string) (string, string) {
	t.Helper()
	sshd := ""
	for _, candidate := range []string{"/usr/sbin/sshd", "/usr/bin/sshd"} {
		if _, err := os.Stat(candidate); err == nil {
			sshd = candidate
		}
	}
	playbook := os.Getenv("INFRA_ANSIBLE_PLAYBOOK")
	if playbook == "" {
		playbook = filepath.Join(root, ".venv", "bin", "ansible-playbook")
	}
	_, keygen := exec.LookPath("ssh-keygen")
	if _, err := os.Stat(playbook); sshd == "" || err != nil || keygen != nil {
		unavailable(t, "requires sshd, ssh-keygen and the repository's Ansible environment")
	}
	return sshd, playbook
}

func TestHostAccessReachesHostsThroughARealSSHHopAndProxyJump(t *testing.T) {
	root := repositoryRoot(t)
	sshd, playbook := hopTools(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	work := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		if output, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %q: %v\n%s", name, args, err, output)
		}
	}
	run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(work, "host"))
	run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "infra-reconciler@test", "-f", filepath.Join(work, "identity"))
	authorized, err := os.ReadFile(filepath.Join(work, "identity.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "authorized_keys"), authorized, 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	config := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nListenAddress 127.0.0.2\nHostKey %s\nAuthorizedKeysFile %s\nPidFile none\nUsePAM no\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nAllowAgentForwarding no\n", port, filepath.Join(work, "host"), filepath.Join(work, "authorized_keys"))
	if err := os.WriteFile(filepath.Join(work, "sshd_config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(work, "sshd.log")
	daemon := exec.CommandContext(ctx, sshd, "-D", "-E", journal, "-f", filepath.Join(work, "sshd_config"))
	daemonLog := func() string {
		data, _ := os.ReadFile(journal)
		return string(data)
	}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
	})
	for _, address := range []string{"127.0.0.1", "127.0.0.2"} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			connection, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", address, port), time.Second)
			if err == nil {
				connection.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("sshd did not listen on %s: %v\n%s", address, err, daemonLog())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	hostKey, err := os.ReadFile(filepath.Join(work, "host.pub"))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Join(strings.Fields(string(hostKey))[:2], " ")
	knownHosts := filepath.Join(work, "known_hosts")
	if err := os.WriteFile(knownHosts, fmt.Appendf(nil, "fixture-target %s\n[127.0.0.2]:%d %s\n", key, port, key), 0o644); err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	inventory := fmt.Sprintf(`all:
  vars:
    ansible_user: %[1]s
    ansible_port: %[2]d
    ansible_python_interpreter: /usr/bin/python3
  hosts:
    direct:
      ansible_host: 127.0.0.1
      ansible_ssh_common_args: -o HostKeyAlias=fixture-target -o StrictHostKeyChecking=yes -o ConnectTimeout=10
    volatile:
      ansible_host: 127.0.0.1
      ansible_ssh_common_args: -o HostKeyAlias=fixture-target -o StrictHostKeyChecking=yes -o ConnectTimeout=10 -o ForwardAgent=no
    jumped:
      ansible_host: 127.0.0.1
      ansible_ssh_common_args: -o ProxyJump=%[1]s@127.0.0.2:%[2]d -o HostKeyAlias=fixture-target -o StrictHostKeyChecking=yes -o ConnectTimeout=10
`, account.Username, port)
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"inventory.yml": inventory, "play.yml": fmt.Sprintf("- hosts: all\n  gather_facts: false\n  tasks:\n  - ansible.builtin.command: %s\n    changed_when: false\n", truePath)} {
		if err := os.WriteFile(filepath.Join(work, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runtime := t.TempDir()
	if memoryBackedFilesystem("/dev/shm") == nil {
		shared, err := os.MkdirTemp("/dev/shm", "infra-host-access-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(shared) })
		runtime = shared
	}
	credentials := filepath.Join(runtime, "credentials.json")
	current, cleanup, err := openSession(filepath.Join(work, "runs"), credentials, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	access, err := current.hostAccess(filepath.Join(work, "identity"), knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	converge := func(extra []string) (string, error) {
		command := exec.CommandContext(ctx, playbook, "-i", filepath.Join(work, "inventory.yml"), filepath.Join(work, "play.yml"))
		command.Env = append([]string{"HOME=" + current.home, "PATH=" + filepath.Dir(playbook) + ":/usr/bin:/bin", "LANG=C.UTF-8", "ANSIBLE_CONFIG=" + filepath.Join(root, "ansible", "ansible.cfg"), "ANSIBLE_LOCAL_TEMP=" + filepath.Join(work, "ansible-local")}, extra...)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	legacy := filepath.Join(current.home, ".ssh")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	for source, target := range map[string]string{filepath.Join(work, "identity"): "id_ed25519", knownHosts: "known_hosts"} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacy, target), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := converge(nil); err == nil || !strings.Contains(output, "unreachable=1") {
		t.Fatalf("hosts were reachable through files in HOME, which OpenSSH ignores: %v\n%s", err, output)
	}
	if err := os.RemoveAll(legacy); err != nil {
		t.Fatal(err)
	}
	output, err := converge(access)
	if err != nil {
		t.Fatalf("hosts unreachable through the run's SSH configuration: %v\n%s\n%s", err, output, daemonLog())
	}
	for _, host := range []string{"direct", "volatile", "jumped"} {
		if !strings.Contains(output, host+" ") || !strings.Contains(output, "ok=1") {
			t.Errorf("%s did not converge:\n%s", host, output)
		}
	}
	if strings.Count(output, "unreachable=0") != 3 || strings.Count(output, "failed=0") != 3 {
		t.Errorf("recap:\n%s", output)
	}
	if strings.Count(daemonLog(), "Accepted publickey for "+account.Username) < 4 {
		t.Errorf("sshd accepted fewer than one key per connection and jump:\n%s", daemonLog())
	}
}
