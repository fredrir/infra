package dev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"github.com/kdomanski/iso9660"
	"go.yaml.in/yaml/v3"
)

const hostsFixture = `image:
  url: %s/ubuntu.img
  sha256: %s
nodes:
- name: dev-server-1
  role: server
  cpus: 2
  memory_mib: 3072
  disk_gib: 40
  private_ip: 10.60.0.11
  tailscale_ip: 100.64.0.11
  ssh_port: 2211
- name: dev-agent-1
  role: agent
  cpus: 1
  memory_mib: 2048
  disk_gib: 20
  private_ip: 10.60.0.21
  tailscale_ip: 100.64.0.21
  ssh_port: 2221
`

type hostsFake struct {
	t        *testing.T
	root     string
	alive    map[int]bool
	commands [][]string
	env      map[string][]string
	stdin    map[string]bool
}

func (f *hostsFake) runner() ci.Runner {
	return ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		if options.Dir != f.root {
			f.t.Errorf("%s ran outside the repository: %s", options.Name, options.Dir)
		}
		command := append([]string{filepath.Base(options.Name)}, options.Args...)
		f.commands = append(f.commands, command)
		f.env[strings.Join(command, " ")] = options.Env
		f.stdin[strings.Join(command, " ")] = options.Stdin != nil
		switch command[0] {
		case "ssh-keygen":
			key := command[slices.Index(command, "-f")+1]
			if err := os.WriteFile(key, []byte("PRIVATE\n"), 0o600); err != nil {
				f.t.Fatal(err)
			}
			if err := os.WriteFile(key+".pub", []byte("ssh-ed25519 AAAAC3dev infra-dev-hosts\n"), 0o644); err != nil {
				f.t.Fatal(err)
			}
		case "qemu-img":
			if err := os.WriteFile(command[len(command)-2], []byte("qcow2"), 0o644); err != nil {
				f.t.Fatal(err)
			}
		case "qemu-system-x86_64":
			pidfile := command[slices.Index(command, "-pidfile")+1]
			pid := 4000 + len(f.commands)
			f.alive[pid] = true
			if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
				f.t.Fatal(err)
			}
		case "ssh-keyscan":
			port := command[slices.Index(command, "-p")+1]
			return process.Result{Stdout: []byte("[127.0.0.1]:" + port + " ssh-ed25519 AAAAhost" + port + "\n")}, nil
		}
		return process.Result{}, nil
	}}
}

func (f *hostsFake) signal(pid int, signal syscall.Signal) error {
	if !f.alive[pid] {
		return syscall.ESRCH
	}
	if signal == syscall.SIGTERM {
		delete(f.alive, pid)
	}
	return nil
}

func (f *hostsFake) count(prefix string) int {
	total := 0
	for _, command := range f.commands {
		if strings.HasPrefix(strings.Join(command, " "), prefix) {
			total++
		}
	}
	return total
}

func (f *hostsFake) find(prefix string) []string {
	for _, command := range f.commands {
		if strings.HasPrefix(strings.Join(command, " "), prefix) {
			return command
		}
	}
	return nil
}

func hostsRoot(t *testing.T) (string, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	image := []byte("ubuntu-cloud-image")
	digest := sha256.Sum256(image)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(image) }))
	t.Cleanup(server.Close)
	writeFile(t, filepath.Join(root, hostsSpecFile), fmt.Sprintf(hostsFixture, server.URL, hex.EncodeToString(digest[:])))
	writeFile(t, filepath.Join(root, productionInventory), "all:\n  vars:\n    ubuntu_release: '26.04'\n")
	writeFile(t, filepath.Join(root, ansibleConfig), "[defaults]\nroles_path = roles\n")
	writeFile(t, filepath.Join(root, "ansible/site.yml"), "- hosts: ubuntu\n")
	return root, server
}

func newHostsFake(t *testing.T, root string) *hostsFake {
	t.Helper()
	return &hostsFake{t: t, root: root, alive: map[int]bool{}, env: map[string][]string{}, stdin: map[string]bool{}}
}

func TestHostsUpPreparesGuestsInventoryAndKnownHosts(t *testing.T) {
	root, server := hostsRoot(t)
	fake := newHostsFake(t, root)
	opts := HostsOptions{State: NewState(root), Runner: fake.runner(), Client: server.Client(), Signal: fake.signal, Log: io.Discard}
	status, err := HostsUp(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Hosts) != 2 || !status.Hosts[0].Running || !status.Hosts[1].Running || status.Hosts[0].PID == 0 || status.Hosts[0].SSH != "ssh -p 2211 dev@127.0.0.1" {
		t.Fatalf("unexpected status %+v", status)
	}
	hosts := filepath.Join(root, ".cache/dev/hosts")
	image, _ := filepath.Abs(filepath.Join(hosts, "images/ubuntu.img"))
	if data, err := os.ReadFile(image); err != nil || string(data) != "ubuntu-cloud-image" {
		t.Fatalf("image not downloaded: %v", err)
	}
	if _, err := os.Stat(image + ".sha256"); err != nil {
		t.Fatal("image receipt missing")
	}
	if fake.count("ssh -i") != 2 || !slices.Equal(fake.find("ssh -i"), []string{"ssh", "-i", filepath.Join(hosts, "id_ed25519"), "-p", "2211", "-o", "UserKnownHostsFile=" + filepath.Join(hosts, "known_hosts"), "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "dev@127.0.0.1", "cloud-init", "status", "--wait"}) {
		t.Fatalf("cloud-init completion not awaited: %v", fake.commands)
	}
	if fake.count("ssh-keygen") != 1 || fake.count("qemu-img create") != 2 || fake.count("qemu-system-x86_64") != 2 || fake.count("ssh-keyscan") != 2 {
		t.Fatalf("unexpected preparation commands: %v", fake.commands)
	}
	server1 := filepath.Join(hosts, "dev-server-1")
	disk := fake.find("qemu-img create")
	if !slices.Equal(disk, []string{"qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", image, filepath.Join(server1, "disk.qcow2"), "40G"}) {
		t.Fatalf("unexpected disk creation %v", disk)
	}
	qemu := fake.find("qemu-system-x86_64 -name dev-server-1")
	for _, expected := range []string{"q35,accel=kvm", "-smp", "2", "-m", "3072", "user,id=net0,hostfwd=tcp:127.0.0.1:2211-:22", "virtio-net-pci,netdev=net0,mac=52:54:00:12:34:10", "socket,id=net1,mcast=230.0.0.1:12160", "virtio-net-pci,netdev=net1,mac=52:54:00:60:00:10", "file=" + filepath.Join(server1, "seed.iso") + ",if=virtio,format=raw,readonly=on", "-daemonize", "-pidfile", filepath.Join(server1, "qemu.pid")} {
		if !slices.Contains(qemu, expected) {
			t.Errorf("guest launch lacks %q: %v", expected, qemu)
		}
	}
	if agent := fake.find("qemu-system-x86_64 -name dev-agent-1"); !slices.Contains(agent, "virtio-net-pci,netdev=net1,mac=52:54:00:60:00:11") || !slices.Contains(agent, "user,id=net0,hostfwd=tcp:127.0.0.1:2221-:22") {
		t.Errorf("second guest addressing wrong: %v", agent)
	}
	seed, err := os.Open(filepath.Join(server1, "seed.iso"))
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	iso, err := iso9660.OpenImage(seed)
	if err != nil {
		t.Fatal(err)
	}
	rootDir, err := iso.RootDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := rootDir.GetChildren()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		data, err := io.ReadAll(entry.Reader())
		if err != nil {
			t.Fatal(err)
		}
		files[strings.ToLower(entry.Name())] = string(data)
	}
	for name, fragments := range map[string][]string{
		"user-data":      {"#cloud-config", "hostname: dev-server-1", "package_update: true", "name: dev", "NOPASSWD:ALL", "ssh-ed25519 AAAAC3dev infra-dev-hosts", "path: /etc/rancher/k3s/server-token", "path: /etc/rancher/k3s/agent-token", "permissions: '0600'", "path: /etc/systemd/system/tailscaled.service", "- [systemctl, enable, --now, tailscaled.service]"},
		"meta-data":      {"instance-id: dev-server-1", "local-hostname: dev-server-1"},
		"network-config": {`macaddress: "52:54:00:12:34:10"`, "dhcp4: true", `macaddress: "52:54:00:60:00:10"`, "set-name: tailscale0", "mtu: 1280", "- 100.64.0.11/24", "- 10.60.0.11/24"},
	} {
		for _, fragment := range fragments {
			if !strings.Contains(files[name], fragment) {
				t.Errorf("%s lacks %q:\n%s", name, fragment, files[name])
			}
		}
	}
	tokensData, err := os.ReadFile(filepath.Join(hosts, "k3s-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tokens map[string]string
	if err := json.Unmarshal(tokensData, &tokens); err != nil || len(tokens["server"]) != 64 || len(tokens["agent"]) != 64 || tokens["server"] == tokens["agent"] {
		t.Fatalf("unexpected tokens %s: %v", tokensData, err)
	}
	if !strings.Contains(files["user-data"], "content: "+tokens["server"]) || !strings.Contains(files["user-data"], "content: "+tokens["agent"]) {
		t.Fatalf("server seed lacks the shared tokens:\n%s", files["user-data"])
	}
	agentSeed, err := os.Open(filepath.Join(hosts, "dev-agent-1", "seed.iso"))
	if err != nil {
		t.Fatal(err)
	}
	defer agentSeed.Close()
	agentISO, err := iso9660.OpenImage(agentSeed)
	if err != nil {
		t.Fatal(err)
	}
	agentRoot, err := agentISO.RootDir()
	if err != nil {
		t.Fatal(err)
	}
	agentEntries, err := agentRoot.GetChildren()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range agentEntries {
		if strings.ToLower(entry.Name()) != "user-data" {
			continue
		}
		data, err := io.ReadAll(entry.Reader())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "server-token") || !strings.Contains(string(data), "content: "+tokens["agent"]) {
			t.Fatalf("agent seed tokens wrong:\n%s", data)
		}
	}
	inventoryData, err := os.ReadFile(filepath.Join(hosts, "inventory.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory map[string]any
	if err := yaml.Unmarshal(inventoryData, &inventory); err != nil {
		t.Fatal(err)
	}
	all := inventory["all"].(map[string]any)
	vars := all["vars"].(map[string]any)
	if vars["ubuntu_release"] != "26.04" || vars["ansible_user"] != "dev" || vars["ansible_become"] != true || vars["infra_reconcile_tailnet"] != false {
		t.Fatalf("unexpected inventory vars %+v", vars)
	}
	if !strings.Contains(vars["ansible_ssh_common_args"].(string), "UserKnownHostsFile="+filepath.Join(hosts, "known_hosts")) || vars["ansible_ssh_private_key_file"] != filepath.Join(hosts, "id_ed25519") {
		t.Fatalf("ssh settings wrong %+v", vars)
	}
	cluster := all["children"].(map[string]any)["ubuntu"].(map[string]any)["children"].(map[string]any)["k3s_cluster"].(map[string]any)["children"].(map[string]any)
	server1Vars := cluster["server"].(map[string]any)["hosts"].(map[string]any)["dev-server-1"].(map[string]any)
	if server1Vars["ansible_port"] != 2211 || server1Vars["private_ip"] != "10.60.0.11" || server1Vars["tailscale_ip"] != "100.64.0.11" {
		t.Fatalf("server host vars wrong %+v", server1Vars)
	}
	if _, ok := cluster["agent"].(map[string]any)["hosts"].(map[string]any)["dev-agent-1"]; !ok {
		t.Fatal("agent missing from inventory")
	}
	known, err := os.ReadFile(filepath.Join(hosts, "known_hosts"))
	if err != nil || !strings.Contains(string(known), "[127.0.0.1]:2211 ssh-ed25519") || !strings.Contains(string(known), "[127.0.0.1]:2221 ssh-ed25519") {
		t.Fatalf("known_hosts incomplete: %v\n%s", err, known)
	}
	fake.commands = nil
	server.Close()
	if _, err := HostsUp(context.Background(), opts); err != nil || fake.count("qemu-system-x86_64") != 0 || fake.count("ssh-keygen") != 0 {
		t.Fatalf("running guests were relaunched or the image was fetched again: %v %v", err, fake.commands)
	}
	if err := HostsDown(context.Background(), opts, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.alive) != 0 {
		t.Fatal("guests still alive after down")
	}
	for _, path := range []string{"dev-server-1", "dev-agent-1", "inventory.yml", "known_hosts"} {
		if _, err := os.Stat(filepath.Join(hosts, path)); !os.IsNotExist(err) {
			t.Errorf("%s retained after purge", path)
		}
	}
	if _, err := os.Stat(image); err != nil {
		t.Fatal("base image removed by purge")
	}
}

func TestHostsPlayUsesDevInventoryAndAnsibleConfig(t *testing.T) {
	root, server := hostsRoot(t)
	fake := newHostsFake(t, root)
	hosts := HostsOptions{State: NewState(root), Runner: fake.runner(), Client: server.Client(), Signal: fake.signal}
	if err := HostsPlay(context.Background(), PlayOptions{Hosts: hosts, Playbook: "site.yml"}); err == nil || !strings.Contains(err.Error(), "hosts up") {
		t.Fatalf("play without inventory accepted: %v", err)
	}
	writeFile(t, filepath.Join(root, ".cache/dev/hosts/inventory.yml"), "all: {}\n")
	inventory := filepath.Join(root, ".cache/dev/hosts/inventory.yml")
	if err := HostsPlay(context.Background(), PlayOptions{Hosts: hosts, Playbook: "site.yml", Check: true, Args: []string{"--limit", "dev-server-1"}, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	play := fake.find("ansible-playbook")
	if !slices.Equal(play, []string{"ansible-playbook", "-i", inventory, "ansible/site.yml", "--check", "--diff", "--limit", "dev-server-1"}) {
		t.Fatalf("unexpected play %v", play)
	}
	if !slices.Contains(fake.env[strings.Join(play, " ")], "ANSIBLE_CONFIG="+filepath.Join(root, ansibleConfig)) {
		t.Fatalf("ansible config not set: %v", fake.env[strings.Join(play, " ")])
	}
	if err := HostsPlay(context.Background(), PlayOptions{Hosts: hosts, Playbook: "../outside.yml"}); err == nil {
		t.Fatal("playbook outside the repository accepted")
	}
	if err := HostsPlay(context.Background(), PlayOptions{Hosts: hosts, Playbook: "missing.yml"}); err == nil {
		t.Fatal("missing playbook accepted")
	}
	if err := HostsSSH(context.Background(), SSHOptions{Hosts: hosts, Node: "dev-agent-1", Args: []string{"uptime"}, Stdin: strings.NewReader("")}); err != nil {
		t.Fatal(err)
	}
	ssh := fake.find("ssh -i")
	if !slices.Equal(ssh, []string{"ssh", "-i", filepath.Join(root, ".cache/dev/hosts/id_ed25519"), "-p", "2221", "-o", "UserKnownHostsFile=" + filepath.Join(root, ".cache/dev/hosts/known_hosts"), "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "dev@127.0.0.1", "uptime"}) || !fake.stdin[strings.Join(ssh, " ")] {
		t.Fatalf("unexpected ssh %v", ssh)
	}
	if err := HostsSSH(context.Background(), SSHOptions{Hosts: hosts, Node: "dev-unknown"}); err == nil {
		t.Fatal("unknown node accepted")
	}
}

func TestHostsSpecValidation(t *testing.T) {
	root, server := hostsRoot(t)
	valid, err := os.ReadFile(filepath.Join(root, hostsSpecFile))
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"duplicate-port": strings.Replace(string(valid), "ssh_port: 2221", "ssh_port: 2211", 1),
		"bad-role":       strings.Replace(string(valid), "role: agent", "role: worker", 1),
		"no-server":      strings.Replace(string(valid), "role: server", "role: agent", 1),
		"bad-name":       strings.Replace(string(valid), "name: dev-agent-1", "name: Dev_Agent", 1),
		"tiny-memory":    strings.Replace(string(valid), "memory_mib: 2048", "memory_mib: 128", 1),
		"plain-http":     strings.Replace(string(valid), server.URL, "http://example.invalid", 1),
		"no-digest":      strings.Replace(string(valid), "sha256: ", "sha256: x", 1),
	} {
		writeFile(t, filepath.Join(root, hostsSpecFile), content)
		if _, err := readHostsSpec(root); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	writeFile(t, filepath.Join(root, hostsSpecFile), string(valid))
	spec, err := readHostsSpec(root)
	if err != nil || spec.Network.Multicast != "230.0.0.1:12160" || len(spec.Nodes) != 2 {
		t.Fatalf("valid spec rejected: %v %+v", err, spec)
	}
}

func TestHostsUpStartsSelectedNodesAndKeepsTheReconcilerOutsideTheCluster(t *testing.T) {
	root, server := hostsRoot(t)
	spec, err := os.ReadFile(filepath.Join(root, hostsSpecFile))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, hostsSpecFile), string(spec)+"- name: dev-reconciler-1\n  role: reconciler\n  cpus: 4\n  memory_mib: 4096\n  disk_gib: 30\n  private_ip: 10.60.0.31\n  tailscale_ip: 100.64.0.31\n  ssh_port: 2231\n")
	fake := newHostsFake(t, root)
	opts := HostsOptions{State: NewState(root), Runner: fake.runner(), Client: server.Client(), Signal: fake.signal, Log: io.Discard}
	opts.Nodes = []string{"dev-missing-1"}
	if _, err := HostsUp(context.Background(), opts); err == nil || !strings.Contains(err.Error(), `unknown node "dev-missing-1"`) {
		t.Fatalf("unknown node returned %v", err)
	}
	opts.Nodes = []string{"dev-reconciler-1"}
	if _, err := HostsUp(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if fake.count("qemu-system-x86_64") != 1 || fake.count("qemu-system-x86_64 -name dev-reconciler-1") != 1 || fake.count("ssh-keyscan") != 1 {
		t.Fatalf("selection launched %v", fake.commands)
	}
	seed, err := os.Open(filepath.Join(root, ".cache/dev/hosts/dev-reconciler-1/seed.iso"))
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	iso, err := iso9660.OpenImage(seed)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := iso.RootDir()
	if err != nil {
		t.Fatal(err)
	}
	children, err := directory.GetChildren()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range children {
		if strings.ToLower(entry.Name()) != "user-data" {
			continue
		}
		data, err := io.ReadAll(entry.Reader())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "/etc/rancher/k3s/") || !strings.Contains(string(data), "path: /etc/systemd/system/tailscaled.service") {
			t.Fatalf("reconciler seed:\n%s", data)
		}
	}
	data, err := os.ReadFile(opts.State.Inventory())
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		All struct {
			Children struct {
				Ubuntu      map[string]any `yaml:"ubuntu"`
				Reconcilers struct {
					Hosts map[string]map[string]any `yaml:"hosts"`
				} `yaml:"reconcilers"`
			} `yaml:"children"`
		} `yaml:"all"`
	}
	if err := yaml.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	if host := inventory.All.Children.Reconcilers.Hosts["dev-reconciler-1"]; host["ansible_port"] != 2231 || host["tailscale_ip"] != "100.64.0.31" || strings.Contains(fmt.Sprint(inventory.All.Children.Ubuntu), "dev-reconciler-1") {
		t.Fatalf("reconciler inventory:\n%s", data)
	}
	fake.commands = nil
	opts.Nodes = nil
	if _, err := HostsUp(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if fake.count("qemu-system-x86_64") != 2 || fake.count("qemu-system-x86_64 -name dev-reconciler-1") != 0 || fake.count("ssh-keyscan") != 3 {
		t.Fatalf("remaining guests launched with %v", fake.commands)
	}
}
