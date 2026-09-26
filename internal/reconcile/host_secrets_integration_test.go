package reconcile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	secretsStubInfra = `#!/bin/sh
set -eu
echo begin
digest() { printf '%s=%s\n' "$1" "$(printf '%s' "$2" | sha256sum | cut -d ' ' -f 1)"; }
hidden() { if test -r /etc/age/host.key; then echo key=readable; else echo key=hidden; fi; }
case "$1 $2" in
"platform control-backup")
  for name in RESTIC_REPOSITORY AWS_DEFAULT_REGION RESTIC_PASSWORD AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY BACKUP_HEARTBEAT_TOKEN; do
    eval "value=\${$name-}"
    digest "$name" "$value"
  done
  echo "credentials=$(ls "$CREDENTIALS_DIRECTORY")"
  hidden ;;
"reconcile request-verification")
  for argument; do
    case "$argument" in
    --private-key=*) key=${argument#*=} ;;
    --heartbeat-token=*) token=${argument#*=} ;;
    esac
  done
  echo "private_key=$(sha256sum < "$key" | cut -d ' ' -f 1)"
  echo "token=$(sha256sum < "$token" | cut -d ' ' -f 1)"
  echo "owner=$(stat -c '%u %a' "$key" "$token" | sort -u | paste -sd ,)"
  echo "user=$(id -u)"
  hidden ;;
esac
`
	secretsStubGatus = `#!/usr/bin/python3
import hashlib, http.server, os, re, sys
config = open(os.environ["GATUS_CONFIG_PATH"]).read()
print("begin")
for name in sorted(set(re.findall(r"\$\{([A-Z0-9_]+)\}", config))):
    print(f"{name}={hashlib.sha256(os.environ.get(name, '').encode()).hexdigest()}")
print("key=" + ("readable" if os.access("/etc/age/host.key", os.R_OK) else "hidden"))
print("credentials=" + ",".join(sorted(os.listdir(os.environ["CREDENTIALS_DIRECTORY"]))))
print("user=" + str(os.getuid()))
sys.stdout.flush()
web = re.search(r"^web:\n\s+address: (\S+)\n\s+port: (\d+)", config, re.M)
class Health(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"{}")
http.server.HTTPServer((web.group(1), int(web.group(2))), Health).serve_forever()
`
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
	var defaults struct {
		SOPS string `yaml:"host_secrets_sops_sha256"`
	}
	data, err := os.ReadFile(filepath.Join(root, "ansible/roles/host_secrets/defaults/main.yml"))
	if err == nil {
		err = yaml.Unmarshal(data, &defaults)
	}
	if err != nil {
		t.Fatal(err)
	}
	sopsBinary, err := exec.LookPath("sops")
	if err != nil {
		t.Fatal("requires the pinned sops on PATH; run infra dev setup")
	}
	if digest := strings.Fields(must("sha256sum", sopsBinary))[0]; digest != defaults.SOPS {
		t.Fatalf("%s is not the pinned SOPS release %s; run infra dev setup", sopsBinary, defaults.SOPS)
	}
	fixture := t.TempDir()
	write := func(path, data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fixture, path), []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.CopyFS(filepath.Join(fixture, "tree/ansible"), os.DirFS(filepath.Join(root, "ansible"))); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"build/cli-release.json", "platform/clusters/production/settings.yaml"} {
		if err := os.MkdirAll(filepath.Join(fixture, "tree", filepath.Dir(file)), 0o755); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join("tree", file), must("cat", filepath.Join(root, file)), 0o644)
	}
	grafana, err := Host(root)
	if err != nil {
		t.Fatal(err)
	}
	write("inventory.yml", `all:
  vars:
    ansible_connection: local
    ansible_user: root
    ansible_python_interpreter: /usr/bin/python3
  children:
    external:
      hosts:
        fredrir-06:
          tailscale_ip: 100.64.0.1
          architecture: amd64
          verification_trigger_app_id: 1
          verification_trigger_installation_id: 2
    control:
      hosts:
        fredrir-07: {}
`, 0o644)
	write("host.yml", "- hosts: fredrir-07\n  gather_facts: false\n  roles: [host_secrets]\n", 0o644)

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
	must("docker", "cp", sopsBinary, name+":/usr/local/bin/sops")
	var secrets []string
	play := func(success bool, playbook string, extra ...string) string {
		t.Helper()
		output, err := run(append([]string{"docker", "exec", "-e", "PYTHONPATH=/opt/ansible", "-e", "PYTHONUNBUFFERED=1", "-e", "ANSIBLE_CONFIG=/fixture/tree/ansible/ansible.cfg", name, "python3", "-m", "ansible.cli.playbook", "-i", "/fixture/inventory.yml", playbook}, extra...)...)
		if (err == nil) != success {
			t.Fatalf("%s success=%t: %v\n%s", playbook, success, err, output)
		}
		if strings.Contains(output, "AGE-SECRET-KEY-") || slices.ContainsFunc(secrets, func(secret string) bool { return strings.Contains(output, secret) }) {
			t.Fatalf("%s printed a secret", playbook)
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
	recordKey := regexp.MustCompile(`^[A-Za-z_]+$`)
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
	host := recipient()
	if host == enrolled {
		t.Fatal("a regenerated host key kept the lost recipient")
	}
	t.Log("check mode reports a missing host key as a difference and fails to read its recipient without creating one, and a lost key is replaced by a new recipient")

	random := func(size int) string {
		data := make([]byte, size)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(data)
	}
	digest := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	must("age-keygen", "-o", filepath.Join(fixture, "admin.key"))
	admin := strings.TrimSpace(must("age-keygen", "-y", filepath.Join(fixture, "admin.key")))
	encrypt := func(path string, values map[string]string, recipients ...string) {
		t.Helper()
		for _, value := range values {
			secrets = append(secrets, value)
		}
		data, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(ctx, "sops", "encrypt", "--age", strings.Join(recipients, ","), "--input-type", "json", "--output-type", "yaml", "/dev/stdin")
		command.Dir, command.Stdin = fixture, bytes.NewReader(data)
		output, err := command.Output()
		if err != nil {
			t.Fatalf("encrypt %s: %v", path, err)
		}
		write(path, string(output), 0o644)
	}
	smtpPassword := make([]byte, 30)
	if _, err := rand.Read(smtpPassword); err != nil {
		t.Fatal(err)
	}
	control := map[string]string{"RESTIC_PASSWORD": "p \"q\" $HOME \\ '" + random(16), "AWS_ACCESS_KEY_ID": "AKIA" + random(8), "AWS_SECRET_ACCESS_KEY": random(20), "BACKUP_HEARTBEAT_TOKEN": random(20)}
	gatus := map[string]string{"GATUS_SMTP_USERNAME": "AKIA" + random(8), "GATUS_SMTP_PASSWORD": base64.StdEncoding.EncodeToString(smtpPassword)}
	for _, endpoint := range []string{"BACKUPS_PARSER", "BACKUPS_Y", "BACKUPS_PORTFOLIO", "BACKUPS_ATTIC", "BACKUPS_CONTROL", "RECONCILIATION_VERIFICATION"} {
		gatus["GATUS_TOKEN_"+endpoint] = random(20)
	}
	credentials := map[string]string{"private_key": "-----BEGIN TEST KEY-----\n" + random(24) + "\n" + random(24) + "\n-----END TEST KEY-----\n", "token": random(20)}
	controlSecrets := "tree/ansible/roles/control_backup/files/control.sops.yaml"
	gatusSecrets := "tree/ansible/roles/gatus/files/secrets.sops.yaml"
	encrypt(controlSecrets, control, admin, host)
	encrypt(gatusSecrets, gatus, admin, host)
	encrypt("tree/ansible/roles/verification_trigger/files/credentials.sops.yaml", credentials, admin, host)
	encrypt("foreign.sops.yaml", control, admin)

	write("infra", secretsStubInfra, 0o755)
	write("gatus", secretsStubGatus, 0o755)
	var layer bytes.Buffer
	compressed := gzip.NewWriter(&layer)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "gatus", Mode: 0o755, Size: int64(len(secretsStubGatus))}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{func() error { _, err := archive.Write([]byte(secretsStubGatus)); return err }, archive.Close, compressed.Close} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	write("gatus.tar.gz", layer.String(), 0o644)
	if output, err := guest(`install -m 0755 /fixture/infra /usr/local/bin/infra
install -D -m 0600 /fixture/gatus.tar.gz /var/cache/gatus/binary.tar.gz
ip address add 100.64.0.1/32 dev lo
printf '[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/true\n' > /etc/systemd/system/tailscaled.service
install -d -m 0700 /etc/platform-backups /etc/infra-verification
echo plaintext > /etc/platform-backups/control.env
echo plaintext > /etc/infra-verification/github-app.pem
echo plaintext > /etc/infra-verification/gatus-token
systemctl daemon-reload`); err != nil {
		t.Fatalf("guest setup: %v\n%s", err, output)
	}
	stubHash := sha256.Sum256([]byte(secretsStubGatus))
	layerHash := sha256.Sum256(layer.Bytes())
	controlPlay := func(success bool, extra ...string) string {
		return play(success, "/fixture/tree/ansible/control-backup.yml", append([]string{"--skip-tags=infra_binary"}, extra...)...)
	}
	monitorPlay := func(extra ...string) string {
		return play(true, "/fixture/tree/ansible/external.yml", append([]string{"--tags=gatus,verification_trigger", "--skip-tags=infra_binary", "--extra-vars", fmt.Sprintf(`{"gatus_archive_sha256":"%x","gatus_binary_sha256":"%x"}`, layerHash, stubHash)}, extra...)...)
	}
	records := func(unit string, ready func(map[string]string) bool) map[string]string {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			journal := must("docker", "exec", name, "journalctl", "--unit", unit, "--output", "cat", "--no-pager")
			values := map[string]string{}
			if start := strings.LastIndex(journal, "begin\n"); start >= 0 {
				for line := range strings.SplitSeq(journal[start+len("begin\n"):], "\n") {
					if key, value, ok := strings.Cut(line, "="); ok && recordKey.MatchString(key) {
						values[key] = value
					}
				}
			}
			if ready(values) {
				return values
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s recorded %v", unit, values)
			}
			time.Sleep(time.Second)
		}
	}
	recorded := func(values map[string]string) bool { return values["key"] != "" }
	absent := func(paths ...string) {
		t.Helper()
		for _, path := range paths {
			if _, err := guest("test ! -e " + path); err != nil {
				t.Fatalf("%s remains", path)
			}
		}
	}

	controlPlay(true)
	for _, mode := range [][]string{nil, {"--check"}} {
		if output := controlPlay(true, mode...); len(changed(output)) != 0 {
			t.Fatalf("control backup %v is not idempotent: %q", mode, changed(output))
		}
	}
	absent("/etc/platform-backups/control.env")
	if installed, expected := must("docker", "exec", name, "sha256sum", "/etc/platform-backups/control.sops.yaml"), must("sha256sum", filepath.Join(fixture, controlSecrets)); strings.Fields(installed)[0] != strings.Fields(expected)[0] {
		t.Fatal("control backup host holds different ciphertext than declared")
	}
	must("docker", "exec", name, "systemctl", "start", "platform-control-backup.service")
	backup := records("platform-control-backup.service", recorded)
	want := map[string]string{"RESTIC_REPOSITORY": digest("s3:s3.eu-north-1.amazonaws.com/llunde-pyparser-bucket/restic/platform/control"), "AWS_DEFAULT_REGION": digest("eu-north-1"), "credentials": "age-key", "key": "hidden"}
	for key, value := range control {
		want[key] = digest(value)
	}
	if !maps.Equal(backup, want) {
		t.Fatalf("control backup started with %v, want %v", backup, want)
	}
	t.Log("the control backup ships only ciphertext, removes plaintext credentials and decrypts them into its environment at start with the credential-delivered host key")

	before := must("docker", "exec", name, "sha256sum", "/etc/platform-backups/control.sops.yaml", "/etc/systemd/system/platform-control-backup.service")
	declared := must("cat", filepath.Join(fixture, controlSecrets))
	must("cp", filepath.Join(fixture, "foreign.sops.yaml"), filepath.Join(fixture, controlSecrets))
	if output := controlPlay(false); !strings.Contains(output, "/fixture/"+controlSecrets+" is not encrypted to fredrir-07 "+host) {
		t.Fatalf("secrets not encrypted to the host did not fail clearly:\n%s", output)
	}
	if after := must("docker", "exec", name, "sha256sum", "/etc/platform-backups/control.sops.yaml", "/etc/systemd/system/platform-control-backup.service"); after != before {
		t.Fatal("secrets not encrypted to the host replaced the working ciphertext or unit")
	}
	must("rm", filepath.Join(fixture, controlSecrets))
	if output := controlPlay(false); !strings.Contains(output, "Missing SOPS-encrypted secrets /fixture/"+controlSecrets) {
		t.Fatalf("missing secrets did not fail clearly:\n%s", output)
	}
	write(controlSecrets, declared, 0o644)
	t.Log("secrets the host cannot decrypt fail before the working ciphertext or unit changes")

	must("docker", "exec", name, "sh", "-c", "echo drift >> /etc/platform-backups/control.sops.yaml")
	if output := controlPlay(true, "--check", "--diff"); !slices.Contains(changed(output), "host_secrets : Install the encrypted secrets") {
		t.Fatalf("edited ciphertext not reported: %q", changed(output))
	}
	controlPlay(true)
	t.Log("check mode compares the installed ciphertext without printing plaintext")

	monitorPlay()
	for _, mode := range [][]string{nil, {"--check"}} {
		if output := monitorPlay(mode...); len(changed(output)) != 0 {
			t.Fatalf("monitor %v is not idempotent: %q", mode, changed(output))
		}
	}
	absent("/etc/infra-verification/github-app.pem", "/etc/infra-verification/gatus-token", "/run/gatus/secrets.env")
	config := must("docker", "exec", name, "cat", "/etc/gatus/config.yaml")
	if strings.Contains(config, "AKIA") || !strings.Contains(config, "https://"+grafana+"/login") {
		t.Fatalf("monitor settings hold secrets or miss the Grafana endpoint:\n%s", config)
	}
	monitor := records("gatus.service", recorded)
	user := monitor["user"]
	delete(monitor, "user")
	want = map[string]string{"key": "hidden", "credentials": "config.yaml"}
	for key, value := range gatus {
		want[key] = digest(value)
	}
	if !maps.Equal(monitor, want) || user == "0" {
		t.Fatalf("Gatus started as %s with %v, want %v", user, monitor, want)
	}
	t.Log("Gatus merges its plain settings with secrets decrypted at start, without access to the host key or a plaintext file left behind")

	must("docker", "exec", name, "systemctl", "start", "infra-verification-request.service")
	verification := records("infra-verification-request.service", recorded)
	user = verification["user"]
	if verification["private_key"] != digest(credentials["private_key"]) || verification["token"] != digest(credentials["token"]) || verification["owner"] != user+" 600" || user == "0" || verification["key"] != "hidden" {
		t.Fatalf("verification request started with %v", verification)
	}
	absent("/run/infra-verification-request")
	t.Log("verification credentials are decrypted at start into a private runtime directory that is removed after the run")

	gatus["GATUS_TOKEN_BACKUPS_CONTROL"] = random(20)
	encrypt(gatusSecrets, gatus, admin, host)
	if output := monitorPlay(); !slices.Contains(changed(output), "gatus : Restart Gatus") {
		t.Fatalf("new monitor secrets did not restart Gatus: %q", changed(output))
	}
	records("gatus.service", func(values map[string]string) bool {
		return values["GATUS_TOKEN_BACKUPS_CONTROL"] == digest(gatus["GATUS_TOKEN_BACKUPS_CONTROL"])
	})
	must("docker", "exec", name, "sed", "-i", "s/OnCalendar=hourly/OnCalendar=daily/", "/etc/systemd/system/infra-verification-request.timer")
	if output := monitorPlay("--check"); !slices.Equal(changed(output), []string{"verification_trigger : Install the verification request units", "verification_trigger : Restart the verification request timer"}) {
		t.Fatalf("edited verification timer reported as %q", changed(output))
	}
	monitorPlay()
	t.Log("changed monitor secrets restart Gatus with the new values, and an edited timer is reported and repaired")
}
