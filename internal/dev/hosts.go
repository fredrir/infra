package dev

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"github.com/kdomanski/iso9660"
	"go.yaml.in/yaml/v3"
)

const (
	hostsSpecFile       = "dev/hosts/hosts.yaml"
	productionInventory = "ansible/inventory/production.yml"
	ansibleConfig       = "ansible/ansible.cfg"
	hostsUser           = "dev"
	hostsGateway        = "10.0.2.2"
)

type HostSpec struct {
	Name        string `yaml:"name" json:"name"`
	Role        string `yaml:"role" json:"role"`
	CPUs        int    `yaml:"cpus" json:"cpus"`
	MemoryMiB   int    `yaml:"memory_mib" json:"memory_mib"`
	DiskGiB     int    `yaml:"disk_gib" json:"disk_gib"`
	PrivateIP   string `yaml:"private_ip" json:"private_ip"`
	TailscaleIP string `yaml:"tailscale_ip" json:"tailscale_ip"`
	SSHPort     int    `yaml:"ssh_port" json:"ssh_port"`
}

type HostsSpec struct {
	Image struct {
		URL    string `yaml:"url"`
		SHA256 string `yaml:"sha256"`
	} `yaml:"image"`
	Network struct {
		Multicast string `yaml:"multicast"`
	} `yaml:"network"`
	Nodes []HostSpec `yaml:"nodes"`
}

type HostsOptions struct {
	State   State
	Nodes   []string
	Runner  ci.Runner
	Client  *http.Client
	Timeout time.Duration
	Signal  func(pid int, signal syscall.Signal) error
	Log     io.Writer
}

type HostStatus struct {
	HostSpec
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	SSH     string `json:"ssh"`
}

type HostsStatus struct {
	Inventory string       `json:"inventory"`
	Hosts     []HostStatus `json:"hosts"`
}

var hostNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func (s State) Hosts() string     { return filepath.Join(s.Cache, "hosts") }
func (s State) Inventory() string { return filepath.Join(s.Hosts(), "inventory.yml") }

func (opts HostsOptions) defaults() HostsOptions {
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 30 * time.Minute}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.Signal == nil {
		opts.Signal = syscall.Kill
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	return opts
}

func readHostsSpec(root string) (HostsSpec, error) {
	var spec HostsSpec
	data, err := os.ReadFile(filepath.Join(root, hostsSpecFile))
	if err != nil {
		return spec, err
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("%s: %w", hostsSpecFile, err)
	}
	if (!strings.HasPrefix(spec.Image.URL, "https://") && !strings.HasPrefix(spec.Image.URL, "http://127.0.0.1")) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(spec.Image.SHA256) {
		return spec, fmt.Errorf("%s requires an HTTPS image URL and SHA-256", hostsSpecFile)
	}
	if spec.Network.Multicast == "" {
		spec.Network.Multicast = "230.0.0.1:12160"
	}
	ports, names, servers := map[int]bool{}, map[string]bool{}, 0
	for _, node := range spec.Nodes {
		switch {
		case !hostNamePattern.MatchString(node.Name) || names[node.Name]:
			return spec, fmt.Errorf("%s: invalid or duplicate node name %q", hostsSpecFile, node.Name)
		case node.Role != "server" && node.Role != "agent" && node.Role != "reconciler":
			return spec, fmt.Errorf("%s: node %s role must be server, agent or reconciler", hostsSpecFile, node.Name)
		case node.CPUs < 1 || node.MemoryMiB < 512 || node.DiskGiB < 8:
			return spec, fmt.Errorf("%s: node %s needs at least 1 CPU, 512 MiB and 8 GiB", hostsSpecFile, node.Name)
		case node.PrivateIP == "" || node.TailscaleIP == "":
			return spec, fmt.Errorf("%s: node %s needs private_ip and tailscale_ip", hostsSpecFile, node.Name)
		case node.SSHPort <= 1024 || node.SSHPort > 65535 || ports[node.SSHPort]:
			return spec, fmt.Errorf("%s: node %s needs a unique unprivileged ssh_port", hostsSpecFile, node.Name)
		}
		names[node.Name], ports[node.SSHPort] = true, true
		if node.Role == "server" {
			servers++
		}
	}
	if servers == 0 {
		return spec, fmt.Errorf("%s declares no server node", hostsSpecFile)
	}
	return spec, nil
}

func (opts HostsOptions) dir(name string) string { return filepath.Join(opts.State.Hosts(), name) }

func (opts HostsOptions) selected(node HostSpec) bool {
	if len(opts.Nodes) == 0 {
		return node.Role != "reconciler"
	}
	return slices.Contains(opts.Nodes, node.Name)
}

func (opts HostsOptions) pid(name string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(opts.dir(name), "qemu.pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, opts.Signal(pid, 0) == nil
}

func HostsUp(ctx context.Context, opts HostsOptions) (HostsStatus, error) {
	opts = opts.defaults()
	spec, err := readHostsSpec(opts.State.Root)
	if err != nil {
		return HostsStatus{}, err
	}
	if err := os.MkdirAll(opts.State.Hosts(), 0o755); err != nil {
		return HostsStatus{}, err
	}
	image, err := opts.ensureImage(ctx, spec)
	if err != nil {
		return HostsStatus{}, err
	}
	key, public, err := opts.ensureSSHKey(ctx)
	if err != nil {
		return HostsStatus{}, err
	}
	tokens, err := opts.ensureTokens()
	if err != nil {
		return HostsStatus{}, err
	}
	for _, name := range opts.Nodes {
		if !slices.ContainsFunc(spec.Nodes, func(node HostSpec) bool { return node.Name == name }) {
			return HostsStatus{}, fmt.Errorf("unknown node %q", name)
		}
	}
	for index, node := range spec.Nodes {
		if _, running := opts.pid(node.Name); running || !opts.selected(node) {
			continue
		}
		if err := opts.launch(ctx, spec, node, index, image, public, tokens); err != nil {
			return HostsStatus{}, fmt.Errorf("%s: %w", node.Name, err)
		}
		fmt.Fprintln(opts.Log, "Started:", node.Name, "ssh -p", node.SSHPort)
	}
	if err := opts.writeInventory(spec, key); err != nil {
		return HostsStatus{}, err
	}
	running := spec
	running.Nodes = slices.DeleteFunc(slices.Clone(spec.Nodes), func(node HostSpec) bool {
		_, alive := opts.pid(node.Name)
		return !alive
	})
	if err := opts.awaitSSH(ctx, running); err != nil {
		return HostsStatus{}, err
	}
	return opts.status(spec), nil
}

func (opts HostsOptions) ensureImage(ctx context.Context, spec HostsSpec) (string, error) {
	images := filepath.Join(opts.State.Hosts(), "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		return "", err
	}
	destination, err := filepath.Abs(filepath.Join(images, filepath.Base(spec.Image.URL)))
	if err != nil {
		return "", err
	}
	receipt := destination + ".sha256"
	if data, err := os.ReadFile(receipt); err == nil && strings.TrimSpace(string(data)) == spec.Image.SHA256 {
		if _, err := os.Stat(destination); err == nil {
			return destination, nil
		}
	}
	fmt.Fprintln(opts.Log, "Downloading:", spec.Image.URL)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.Image.URL, nil)
	if err != nil {
		return "", err
	}
	response, err := opts.Client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image server returned HTTP %d", response.StatusCode)
	}
	temporary, err := os.CreateTemp(images, ".download-")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name())
	digest := sha256.New()
	_, err = io.Copy(io.MultiWriter(temporary, digest), response.Body)
	if err := errors.Join(err, temporary.Close()); err != nil {
		return "", err
	}
	if hex.EncodeToString(digest.Sum(nil)) != spec.Image.SHA256 {
		return "", errors.New("image checksum mismatch")
	}
	if err := os.Rename(temporary.Name(), destination); err != nil {
		return "", err
	}
	return destination, os.WriteFile(receipt, []byte(spec.Image.SHA256+"\n"), 0o644)
}

func (opts HostsOptions) ensureSSHKey(ctx context.Context) (string, string, error) {
	key, err := filepath.Abs(filepath.Join(opts.State.Hosts(), "id_ed25519"))
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(key); os.IsNotExist(err) {
		if _, err := capture(ctx, opts.Runner, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "infra-dev-hosts", "-f", key); err != nil {
			return "", "", fmt.Errorf("generate ssh key: %w", err)
		}
	} else if err != nil {
		return "", "", err
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", "", err
	}
	return key, strings.TrimSpace(string(public)), nil
}

func (opts HostsOptions) ensureTokens() (map[string]string, error) {
	path := filepath.Join(opts.State.Hosts(), "k3s-tokens.json")
	tokens := make(map[string]string)
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &tokens); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	changed := false
	for _, name := range []string{"server", "agent"} {
		if tokens[name] != "" {
			continue
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		tokens[name], changed = hex.EncodeToString(secret), true
	}
	if !changed {
		return tokens, nil
	}
	data, err := json.Marshal(tokens)
	if err != nil {
		return nil, err
	}
	return tokens, os.WriteFile(path, data, 0o600)
}

func hostMAC(prefix string, index int) string { return fmt.Sprintf("%s:%02x", prefix, 0x10+index) }

func (opts HostsOptions) launch(ctx context.Context, spec HostsSpec, node HostSpec, index int, image, public string, tokens map[string]string) error {
	dir, err := filepath.Abs(opts.dir(node.Name))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	disk := filepath.Join(dir, "disk.qcow2")
	if _, err := os.Stat(disk); os.IsNotExist(err) {
		if _, err := capture(ctx, opts.Runner, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", image, disk, fmt.Sprintf("%dG", node.DiskGiB)); err != nil {
			return fmt.Errorf("create disk: %w", err)
		}
	} else if err != nil {
		return err
	}
	seed := filepath.Join(dir, "seed.iso")
	if err := writeSeed(seed, node, index, public, tokens); err != nil {
		return err
	}
	arguments := []string{"-name", node.Name, "-machine", "q35,accel=kvm", "-cpu", "host", "-smp", strconv.Itoa(node.CPUs), "-m", strconv.Itoa(node.MemoryMiB),
		"-display", "none", "-serial", "file:" + filepath.Join(dir, "serial.log"), "-monitor", "none",
		"-drive", "file=" + disk + ",if=virtio,format=qcow2,discard=unmap",
		"-drive", "file=" + seed + ",if=virtio,format=raw,readonly=on",
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", node.SSHPort), "-device", "virtio-net-pci,netdev=net0,mac=" + hostMAC("52:54:00:12:34", index),
		"-netdev", "socket,id=net1,mcast=" + spec.Network.Multicast, "-device", "virtio-net-pci,netdev=net1,mac=" + hostMAC("52:54:00:60:00", index),
		"-device", "virtio-rng-pci", "-pidfile", filepath.Join(dir, "qemu.pid"), "-daemonize"}
	if _, err := capture(ctx, opts.Runner, "qemu-system-x86_64", arguments...); err != nil {
		return fmt.Errorf("start guest: %w", err)
	}
	return nil
}

func writeSeed(path string, node HostSpec, index int, public string, tokens map[string]string) error {
	writer, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer writer.Cleanup()
	userData := fmt.Sprintf("#cloud-config\nhostname: %s\nmanage_etc_hosts: true\nssh_pwauth: false\npackage_update: true\nusers:\n- name: %s\n  sudo: ALL=(ALL) NOPASSWD:ALL\n  shell: /bin/bash\n  lock_passwd: true\n  ssh_authorized_keys:\n  - %s\nwrite_files:\n", node.Name, hostsUser, public)
	roles := map[string][]string{"server": {"server", "agent"}, "agent": {"agent"}}[node.Role]
	for _, role := range roles {
		userData += fmt.Sprintf("- path: /etc/rancher/k3s/%s-token\n  owner: root:root\n  permissions: '0600'\n  content: %s\n", role, tokens[role])
	}
	userData += "- path: /etc/systemd/system/tailscaled.service\n  owner: root:root\n  permissions: '0644'\n  content: |\n    [Unit]\n    Description=Simulated Tailscale daemon\n    After=network-online.target\n    [Service]\n    Type=oneshot\n    RemainAfterExit=yes\n    ExecStart=/bin/true\n    [Install]\n    WantedBy=multi-user.target\n"
	userData += "runcmd:\n- [systemctl, daemon-reload]\n- [systemctl, enable, --now, tailscaled.service]\n"
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", node.Name, node.Name)
	networkConfig := fmt.Sprintf("version: 2\nethernets:\n  uplink:\n    match:\n      macaddress: %q\n    dhcp4: true\n  tailscale0:\n    match:\n      macaddress: %q\n    set-name: tailscale0\n    mtu: 1280\n    addresses:\n    - %s/24\n    - %s/24\n", hostMAC("52:54:00:12:34", index), hostMAC("52:54:00:60:00", index), node.TailscaleIP, node.PrivateIP)
	for name, content := range map[string]string{"user-data": userData, "meta-data": metaData, "network-config": networkConfig} {
		if err := writer.AddFile(strings.NewReader(content), name); err != nil {
			return err
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	err = writer.WriteTo(file, "cidata")
	return errors.Join(err, file.Close())
}

func (opts HostsOptions) writeInventory(spec HostsSpec, key string) error {
	knownHosts, err := filepath.Abs(filepath.Join(opts.State.Hosts(), "known_hosts"))
	if err != nil {
		return err
	}
	release, err := productionRelease(opts.State.Root)
	if err != nil {
		return err
	}
	groups := map[string]map[string]any{"server": {}, "agent": {}, "reconciler": {}}
	for _, node := range spec.Nodes {
		groups[node.Role][node.Name] = map[string]any{"ansible_host": "127.0.0.1", "ansible_port": node.SSHPort, "private_ip": node.PrivateIP, "tailscale_ip": node.TailscaleIP, "provider": "dev", "architecture": "amd64", "public_ipv4": node.PrivateIP}
	}
	inventory := map[string]any{"all": map[string]any{
		"vars": map[string]any{
			"ansible_user": hostsUser, "ansible_become": true, "ansible_python_interpreter": "/usr/bin/python3",
			"ansible_ssh_private_key_file": key, "ansible_ssh_common_args": "-o UserKnownHostsFile=" + knownHosts + " -o IdentitiesOnly=yes -o ConnectTimeout=10",
			"ubuntu_release": release, "firewall_admin_ipv4_cidrs": []string{"10.0.2.0/24", "10.60.0.0/24", "100.64.0.0/24"},
			"infra_reconcile_tailnet": false, "admin_machines": map[string]any{}, "planned_nodes": map[string]any{},
		},
		"children": map[string]any{
			"ubuntu": map[string]any{"children": map[string]any{"k3s_cluster": map[string]any{"children": map[string]any{
				"server": map[string]any{"hosts": groups["server"]}, "agent": map[string]any{"hosts": groups["agent"]},
			}}}},
			"reconcilers": map[string]any{"hosts": groups["reconciler"]},
		},
	}}
	data, err := yaml.Marshal(inventory)
	if err != nil {
		return err
	}
	return os.WriteFile(opts.State.Inventory(), data, 0o600)
}

func productionRelease(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, productionInventory))
	if err != nil {
		return "", err
	}
	var inventory struct {
		All struct {
			Vars struct {
				UbuntuRelease string `yaml:"ubuntu_release"`
			} `yaml:"vars"`
		} `yaml:"all"`
	}
	if err := yaml.Unmarshal(data, &inventory); err != nil {
		return "", fmt.Errorf("%s: %w", productionInventory, err)
	}
	if inventory.All.Vars.UbuntuRelease == "" {
		return "", fmt.Errorf("%s declares no ubuntu_release", productionInventory)
	}
	return inventory.All.Vars.UbuntuRelease, nil
}

func (opts HostsOptions) sshArguments(node HostSpec) ([]string, error) {
	key, err := filepath.Abs(filepath.Join(opts.State.Hosts(), "id_ed25519"))
	if err != nil {
		return nil, err
	}
	knownHosts, err := filepath.Abs(filepath.Join(opts.State.Hosts(), "known_hosts"))
	if err != nil {
		return nil, err
	}
	return []string{"-i", key, "-p", strconv.Itoa(node.SSHPort), "-o", "UserKnownHostsFile=" + knownHosts, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", hostsUser + "@127.0.0.1"}, nil
}

func (opts HostsOptions) awaitSSH(ctx context.Context, spec HostsSpec) error {
	deadline := time.Now().Add(opts.Timeout)
	knownHosts := filepath.Join(opts.State.Hosts(), "known_hosts")
	var entries strings.Builder
	for _, node := range spec.Nodes {
		for {
			output, err := capture(ctx, opts.Runner, "ssh-keyscan", "-T", "5", "-p", strconv.Itoa(node.SSHPort), "127.0.0.1")
			if err == nil && strings.Contains(output, "ssh-") {
				entries.WriteString(strings.TrimSpace(output) + "\n")
				fmt.Fprintln(opts.Log, "Reachable:", node.Name)
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: SSH not reachable on 127.0.0.1:%d within %s; see %s", node.Name, node.SSHPort, opts.Timeout, filepath.Join(opts.dir(node.Name), "serial.log"))
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		if err := os.WriteFile(knownHosts, []byte(entries.String()), 0o600); err != nil {
			return err
		}
		arguments, err := opts.sshArguments(node)
		if err != nil {
			return err
		}
		waitContext, cancel := context.WithDeadline(ctx, deadline)
		_, err = capture(waitContext, opts.Runner, "ssh", append(arguments, "cloud-init", "status", "--wait")...)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: cloud-init did not finish cleanly: %w", node.Name, err)
		}
		fmt.Fprintln(opts.Log, "Initialized:", node.Name)
	}
	return nil
}

func (opts HostsOptions) status(spec HostsSpec) HostsStatus {
	status := HostsStatus{Inventory: opts.State.Inventory()}
	for _, node := range spec.Nodes {
		pid, running := opts.pid(node.Name)
		status.Hosts = append(status.Hosts, HostStatus{HostSpec: node, Running: running, PID: pid, SSH: fmt.Sprintf("ssh -p %d %s@127.0.0.1", node.SSHPort, hostsUser)})
	}
	return status
}

func InspectHosts(ctx context.Context, opts HostsOptions) (HostsStatus, error) {
	opts = opts.defaults()
	spec, err := readHostsSpec(opts.State.Root)
	if err != nil {
		return HostsStatus{}, err
	}
	return opts.status(spec), nil
}

func HostsDown(ctx context.Context, opts HostsOptions, purge bool) error {
	opts = opts.defaults()
	spec, err := readHostsSpec(opts.State.Root)
	if err != nil {
		return err
	}
	for _, node := range spec.Nodes {
		if pid, running := opts.pid(node.Name); running {
			if err := opts.Signal(pid, syscall.SIGTERM); err != nil {
				return fmt.Errorf("stop %s: %w", node.Name, err)
			}
			for attempts := 0; attempts < 60 && opts.Signal(pid, 0) == nil; attempts++ {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
			}
			fmt.Fprintln(opts.Log, "Stopped:", node.Name)
		}
		if err := os.Remove(filepath.Join(opts.dir(node.Name), "qemu.pid")); err != nil && !os.IsNotExist(err) {
			return err
		}
		if purge {
			if err := os.RemoveAll(opts.dir(node.Name)); err != nil {
				return err
			}
		}
	}
	if purge {
		for _, name := range []string{"inventory.yml", "known_hosts"} {
			if err := os.Remove(filepath.Join(opts.State.Hosts(), name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

type PlayOptions struct {
	Hosts    HostsOptions
	Playbook string
	Check    bool
	Args     []string
	Stdout   io.Writer
	Stderr   io.Writer
}

func HostsPlay(ctx context.Context, opts PlayOptions) error {
	hosts := opts.Hosts.defaults()
	inventory, err := filepath.Abs(hosts.State.Inventory())
	if err != nil {
		return err
	}
	if _, err := os.Stat(inventory); err != nil {
		return fmt.Errorf("dev inventory missing; run infra dev hosts up")
	}
	playbook := opts.Playbook
	if !strings.Contains(playbook, "/") {
		playbook = filepath.Join("ansible", playbook)
	}
	if !filepath.IsLocal(playbook) {
		return fmt.Errorf("playbook must be inside the repository")
	}
	if _, err := os.Stat(filepath.Join(hosts.State.Root, playbook)); err != nil {
		return fmt.Errorf("playbook %s: %w", playbook, err)
	}
	config, err := filepath.Abs(filepath.Join(hosts.State.Root, ansibleConfig))
	if err != nil {
		return err
	}
	arguments := []string{"-i", inventory, playbook}
	if opts.Check {
		arguments = append(arguments, "--check", "--diff")
	}
	arguments = append(arguments, opts.Args...)
	_, err = execute(ctx, hosts.Runner, process.Options{Name: filepath.Join(hosts.State.Venv(), "bin", "ansible-playbook"), Args: arguments, Env: []string{"ANSIBLE_CONFIG=" + config}, Stdout: opts.Stdout, Stderr: opts.Stderr})
	return err
}

type SSHOptions struct {
	Hosts  HostsOptions
	Node   string
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func HostsSSH(ctx context.Context, opts SSHOptions) error {
	hosts := opts.Hosts.defaults()
	spec, err := readHostsSpec(hosts.State.Root)
	if err != nil {
		return err
	}
	for _, node := range spec.Nodes {
		if node.Name != opts.Node {
			continue
		}
		arguments, err := hosts.sshArguments(node)
		if err != nil {
			return err
		}
		_, err = execute(ctx, hosts.Runner, process.Options{Name: "ssh", Args: append(arguments, opts.Args...), Stdin: opts.Stdin, Stdout: opts.Stdout, Stderr: opts.Stderr})
		return err
	}
	return fmt.Errorf("unknown node %q", opts.Node)
}
