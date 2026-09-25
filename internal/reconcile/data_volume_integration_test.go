package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const fakeMountSystemctl = `#!/bin/sh
state=/fixture/state
unit_file() { printf '/etc/systemd/system/%s' "$1"; }
value() { sed -n "s/^$2=//p" "$(unit_file "$1")"; }
active() {
  case "$1" in
  *.mount) findmnt --mountpoint "$(value "$1" Where)" > /dev/null 2>&1 ;;
  *) grep -qxF "$1" "$state/active" 2> /dev/null ;;
  esac
}
unit=$2
case "$1" in
show)
  if [ -f "$(unit_file "$unit")" ] || grep -qxF "$unit" "$state/services" 2> /dev/null; then echo LoadState=loaded; else echo LoadState=not-found; fi
  if active "$unit"; then echo ActiveState=active; else echo ActiveState=inactive; fi ;;
is-active)
  shift
  status=0
  for unit; do if active "$unit"; then echo active; else echo inactive; status=3; fi; done
  exit "$status" ;;
is-enabled)
  if grep -qxF "$unit" "$state/enabled" 2> /dev/null; then echo enabled; else echo disabled; exit 1; fi ;;
enable) echo "$unit" >> "$state/enabled" ;;
start)
  case "$unit" in
  *.mount)
    where=$(value "$unit" Where)
    mkdir -p "$where"
    if [ "$(value "$unit" Type)" = none ]; then
      mount -o "$(value "$unit" Options)" "$(value "$unit" What)" "$where"
    else
      mount -t "$(value "$unit" Type)" -o "$(value "$unit" Options)" "$(value "$unit" What)" "$where"
    fi ;;
  *) echo "$unit" >> "$state/active" ;;
  esac ;;
restart) echo "$unit" >> "$state/restarted" ;;
daemon-reload) ;;
*) exit 1 ;;
esac
`

func TestDataVolumeWithLoopDevices(t *testing.T) {
	image := os.Getenv("INFRA_ANSIBLE_TEST_IMAGE")
	if image == "" || os.Getenv("INFRA_ANSIBLE_LOOP_DEVICES") != "1" {
		t.Skip("requires INFRA_ANSIBLE_TEST_IMAGE and INFRA_ANSIBLE_LOOP_DEVICES=1 for a privileged container that attaches host loop devices")
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
	packages := os.Getenv("INFRA_ANSIBLE_SITE_PACKAGES")
	if packages == "" {
		matches, err := filepath.Glob(filepath.Join(root, ".venv/lib/python*/site-packages"))
		if err != nil || len(matches) != 1 {
			t.Fatal("set INFRA_ANSIBLE_SITE_PACKAGES to the local Ansible Python package directory")
		}
		packages = matches[0]
	}
	fixture := t.TempDir()
	write := func(path, data string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(fixture, path)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture, path), []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("bin/systemctl", fakeMountSystemctl, 0755)
	for _, tool := range []string{"parted", "mkfs.ext4"} {
		write("bin/"+tool, "#!/bin/sh\necho \""+tool+" $*\" >> /fixture/state/commands\nexec /usr/sbin/"+tool+" \"$@\"\n", 0755)
	}
	write("state/services", "k3s-agent.service\ndocker.service\n", 0644)
	write("state/active", "docker.service\n", 0644)
	write("inventory.yml", "all:\n  hosts:\n    localhost:\n      ansible_connection: local\n      ansible_python_interpreter: /usr/bin/python3\n", 0644)
	write("volume.yml", "- hosts: all\n  gather_facts: false\n  roles:\n  - data_volume\n", 0644)
	disks := []string{"empty", "formatted", "unpartitioned", "stale"}
	for _, disk := range disks {
		write(disk+".img", "", 0644)
		if err := os.Truncate(filepath.Join(fixture, disk+".img"), 64<<20); err != nil {
			t.Fatal(err)
		}
	}
	name := fmt.Sprintf("infra-data-volume-%d", time.Now().UnixNano())
	must("docker", "run", "-d", "--privileged", "--name", name, "-v", root+":/source:ro", "-v", fixture+":/fixture", "-v", packages+":/opt/ansible:ro", "-e", "PYTHONPATH=/opt/ansible", "-e", "ANSIBLE_CONFIG=/source/ansible/ansible.cfg", "-e", "PATH=/fixture/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", image, "sleep", "infinity")
	loops := map[string]string{}
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", name, "umount", "--lazy", "/var/lib/rancher", "/var/lib/infra-build-vm", "/srv/data", "/srv/formatted", "/srv/stale").Run()
		for _, loop := range loops {
			_ = exec.Command("docker", "exec", name, "losetup", "--detach", "/hostdev/"+loop).Run()
		}
		_ = exec.Command("docker", "exec", name, "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/fixture").Run()
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	inside := func(args ...string) string {
		t.Helper()
		return must(append([]string{"docker", "exec", name}, args...)...)
	}
	inside("sh", "-c", "apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq parted > /dev/null")
	inside("sh", "-c", "mkdir -p /hostdev /dev/disk/by-id && mount -t devtmpfs devtmpfs /hostdev")
	for _, disk := range disks {
		loop := filepath.Base(strings.Fields(inside("losetup", "--find"))[0])
		inside("losetup", "--partscan", "/hostdev/"+loop, "/fixture/"+disk+".img")
		loops[disk] = loop
		inside("ln", "-s", "/hostdev/"+loop, "/dev/disk/by-id/virtio-"+disk)
		inside("ln", "-s", "/hostdev/"+loop+"p1", "/dev/disk/by-id/virtio-"+disk+"-part1")
	}
	signatures := func(device string) []string {
		t.Helper()
		return strings.Fields(inside("wipefs", "--no-act", "--noheadings", "--parsable", "--output=TYPE", device))
	}
	commands := func() string {
		data, _ := os.ReadFile(filepath.Join(fixture, "state/commands"))
		_ = os.Remove(filepath.Join(fixture, "state/commands"))
		return string(data)
	}
	converge := func(disk, mount string, success bool, extra ...string) string {
		t.Helper()
		binds := []string{}
		if mount == "/srv/data" {
			binds = []string{"/var/lib/rancher", "/var/lib/infra-build-vm"}
		}
		vars, _ := json.Marshal(map[string]any{
			"data_volume_device":    "/dev/disk/by-id/virtio-" + disk,
			"data_volume_mount":     mount,
			"data_volume_binds":     binds,
			"data_volume_consumers": []string{"k3s-agent.service", "docker.service"},
		})
		write(disk+".json", string(vars), 0644)
		output, err := run(append([]string{"docker", "exec", "-e", "PYTHONUNBUFFERED=1", name, "python3", "-m", "ansible.cli.playbook", "-i", "/fixture/inventory.yml", "/fixture/volume.yml", "--extra-vars", "@/fixture/" + disk + ".json"}, extra...)...)
		if (err == nil) != success {
			t.Fatalf("data volume success=%t: %v\n%s", success, err, output)
		}
		return output
	}
	device := "/hostdev/" + loops["empty"]
	output := converge("empty", "/srv/data", false, "--check")
	if !strings.Contains(output, "changed: [localhost]") || len(signatures(device)) != 0 || commands() != "" {
		t.Fatalf("check mode touched the empty volume:\n%s", output)
	}
	t.Log("check mode reports an empty volume without partitioning it")
	converge("empty", "/srv/data", true)
	if calls := commands(); strings.Count(calls, "parted ") != 1 || strings.Count(calls, "mkfs.ext4 ") != 1 {
		t.Fatalf("empty volume not partitioned and formatted exactly once: %q", calls)
	}
	if table := signatures(device); !slices.Contains(table, "gpt") || !slices.Equal(signatures(device+"p1"), []string{"ext4"}) {
		t.Fatalf("empty volume holds %q", table)
	}
	uuid := inside("blkid", "--match-tag", "UUID", "--output", "value", device+"p1")
	inside("sh", "-c", "echo kept > /var/lib/rancher/marker")
	if source := strings.TrimSpace(inside("findmnt", "--noheadings", "--output", "SOURCE", "--mountpoint", "/srv/data")); source != device+"p1" {
		t.Fatalf("data volume mounted from %q", source)
	}
	if inside("cat", "/srv/data/rancher/marker") != "kept\n" {
		t.Fatal("layout bind mount does not reach the data volume")
	}
	escaped := `/etc/systemd/system/var-lib-infra\x2dbuild\x2dvm.mount`
	for path, want := range map[string]string{
		"/etc/systemd/system/srv-data.mount": "What=/dev/disk/by-id/virtio-empty-part1\nWhere=/srv/data\nType=ext4\nOptions=nofail\n\n[Install]\nWantedBy=local-fs.target\n",
		escaped:                              "RequiresMountsFor=/srv/data\n\n[Mount]\nWhat=/srv/data/infra-build-vm\nWhere=/var/lib/infra-build-vm\nType=none\nOptions=bind,nofail\n",
		"/etc/systemd/system/k3s-agent.service.d/data-volume.conf": "[Unit]\nRequiresMountsFor=/var/lib/rancher /var/lib/infra-build-vm\n",
		"/etc/systemd/system/docker.service.d/data-volume.conf":    "[Unit]\nRequiresMountsFor=/var/lib/rancher /var/lib/infra-build-vm\n",
	} {
		if unit := inside("cat", path); !strings.Contains(unit, want) {
			t.Fatalf("%s lacks %q:\n%s", path, want, unit)
		}
	}
	if restarted, _ := os.ReadFile(filepath.Join(fixture, "state/restarted")); string(restarted) != "docker.service\n" {
		t.Fatalf("mounting restarted %q, want only the running consumer", restarted)
	}
	t.Log("empty volume is partitioned, formatted, mounted with nofail, bound into its layout and required by its consumers")
	for _, extra := range [][]string{nil, {"--check"}} {
		if output := converge("empty", "/srv/data", true, extra...); !strings.Contains(output, "changed=0") || commands() != "" {
			t.Fatalf("rerun %q changed the converged volume:\n%s", extra, output)
		}
	}
	if inside("blkid", "--match-tag", "UUID", "--output", "value", device+"p1") != uuid || inside("cat", "/var/lib/rancher/marker") != "kept\n" {
		t.Fatal("rerun replaced the data volume filesystem")
	}
	t.Log("reruns and check mode keep the converged volume unchanged")
	formatted := "/hostdev/" + loops["formatted"]
	inside("parted", "--script", formatted, "mklabel", "gpt", "mkpart", "data", "ext4", "1MiB", "100%")
	inside("mkfs.ext4", "-q", "-L", "existing", formatted+"p1")
	inside("sh", "-c", "mkdir -p /mnt/formatted && mount "+formatted+"p1 /mnt/formatted && echo kept > /mnt/formatted/marker && umount /mnt/formatted")
	commands()
	converge("formatted", "/srv/formatted", true)
	if calls := commands(); calls != "" || inside("cat", "/srv/formatted/marker") != "kept\n" || strings.TrimSpace(inside("blkid", "--match-tag", "LABEL", "--output", "value", formatted+"p1")) != "existing" {
		t.Fatalf("existing filesystem was not adopted untouched: %q", calls)
	}
	t.Log("an existing filesystem is mounted without partitioning or formatting")
	unpartitioned := "/hostdev/" + loops["unpartitioned"]
	inside("mkfs.ext4", "-q", unpartitioned)
	commands()
	if output := converge("unpartitioned", "/srv/unpartitioned", false); !strings.Contains(output, "Require an empty or GPT-partitioned data volume") || commands() != "" || !slices.Equal(signatures(unpartitioned), []string{"ext4"}) {
		t.Fatalf("whole-disk filesystem was not refused untouched:\n%s", output)
	}
	t.Log("a whole-disk filesystem fails closed")
	stale := "/hostdev/" + loops["stale"]
	inside("parted", "--script", stale, "mklabel", "gpt", "mkpart", "data", "ext4", "1MiB", "100%")
	inside("mkfs.ext4", "-q", "-L", "stale", stale+"p1")
	inside("wipefs", "--all", "--quiet", stale)
	commands()
	converge("stale", "/srv/stale", true)
	if calls := commands(); strings.Count(calls, "parted ") != 1 || strings.Contains(calls, "mkfs") || strings.TrimSpace(inside("blkid", "--match-tag", "LABEL", "--output", "value", stale+"p1")) != "stale" {
		t.Fatalf("filesystem behind a wiped partition table was reformatted: %q", calls)
	}
	t.Log("a filesystem behind a wiped partition table is repartitioned but never reformatted")
}
