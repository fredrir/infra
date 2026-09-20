package kata

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

func PrepareBuilder(ctx context.Context, log io.Writer) error {
	if e := verify("/inputs/ca.deb", "641de77d8f142cfd62a1a6f964ba67b20754d3337c480efb529d086075a06c9a"); e != nil {
		return e
	}
	if e := run(ctx, "", nil, log, "dpkg-deb", "-x", "/inputs/ca.deb", "/tmp/bootstrap-ca"); e != nil {
		return e
	}
	paths, e := filepath.Glob("/tmp/bootstrap-ca/usr/share/ca-certificates/mozilla/*.crt")
	if e != nil {
		return e
	}
	sort.Strings(paths)
	var certs []byte
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		certs = append(certs, b...)
	}
	if bytesDigest(certs) != "6d84ab71cb726c0641b0af84303c316e3fa50db941dc8507d09045eb2fa5d238" {
		return errors.New("bootstrap CA checksum mismatch")
	}
	if e = os.MkdirAll("/etc/ssl/certs", 0755); e != nil {
		return e
	}
	if e = os.WriteFile("/etc/ssl/certs/ca-certificates.crt", certs, 0644); e != nil {
		return e
	}
	if e = os.WriteFile("/etc/apt/apt.conf.d/99-fail-closed", []byte("APT::Update::Error-Mode \"any\";\n"), 0644); e != nil {
		return e
	}
	env := map[string]string{"DEBIAN_FRONTEND": "noninteractive", "HOME": "/tmp/builder-home"}
	for _, args := range [][]string{{"apt-get", "update"}, {"apt-get", "upgrade", "-y"}, {"apt-get", "install", "-y", "--no-install-recommends", "binutils", "ca-certificates", "curl", "e2fsprogs", "file", "git", "gnupg", "jq", "make", "mmdebstrap", "sudo", "ubuntu-keyring", "xz-utils", "zstd"}} {
		if e = run(ctx, "", env, log, args...); e != nil {
			return e
		}
	}
	b, e := output(ctx, "", "dpkg-query", "-W", "-f=${binary:Package}\t${Version}\t${Architecture}\n")
	if e != nil {
		return e
	}
	return os.WriteFile("/builder-packages.tsv", b, 0644)
}

func Guest(ctx context.Context, log io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("guest builder requires container root")
	}
	p, e := LoadPins("/repo")
	if e != nil {
		return e
	}
	agent := p.Components["guest"].AgentSHA256
	if _, e = os.Stat("/build/rootfs"); !errors.Is(e, os.ErrNotExist) {
		return errors.New("rootfs already exists")
	}
	for _, d := range []string{"/build", "/output", "/inputs/bin", "/tmp/builder-home", "/tmp/builder-config"} {
		if e = os.MkdirAll(d, 0755); e != nil {
			return e
		}
	}
	if e = verify("/inputs/kata.tar.zst", p.Components["kata"].SHA256); e != nil {
		return e
	}
	f, e := os.Open("/inputs/kata.tar.zst")
	if e != nil {
		return e
	}
	zr, e := zstd.NewReader(f)
	if e != nil {
		f.Close()
		return e
	}
	tr := tar.NewReader(zr)
	found := false
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			zr.Close()
			f.Close()
			return e
		}
		if strings.TrimPrefix(h.Name, "./") != "opt/kata/share/kata-containers/kata-ubuntu-noble.image" {
			continue
		}
		if h.Typeflag != tar.TypeReg || found {
			zr.Close()
			f.Close()
			return errors.New("invalid upstream guest image")
		}
		o, e := os.OpenFile("/build/upstream.ext4", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			zr.Close()
			f.Close()
			return e
		}
		_, e = io.Copy(o, &contextReader{ctx, tr})
		e = errors.Join(e, o.Close())
		if e != nil {
			zr.Close()
			f.Close()
			return e
		}
		found = true
	}
	zr.Close()
	f.Close()
	if !found {
		return errors.New("upstream guest image missing")
	}
	if e = run(ctx, "", nil, log, "debugfs", "-R", "dump /usr/bin/kata-agent /build/kata-agent", "/build/upstream.ext4"); e != nil {
		return e
	}
	if e = verify("/build/kata-agent", agent); e != nil {
		return e
	}
	if e = os.Chmod("/build/kata-agent", 0755); e != nil {
		return e
	}
	if e = verify("/kata/src/kata-opa/allow-all.rego", "0bb24b3ac02a72f8f7a11cbeab9831d802393860479ef283d8e480d10dee6a19"); e != nil {
		return e
	}
	if e = copyFile("/infra", "/inputs/bin/MAKEDEV", 0755); e != nil {
		return e
	}
	env := map[string]string{"ARCH": "x86_64", "OS_VERSION": "noble", "AGENT_INIT": "no", "AGENT_POLICY": "yes", "SECCOMP": "yes", "AGENT_POLICY_FILE": "/kata/src/kata-opa/allow-all.rego", "AGENT_SOURCE_BIN": "/build/kata-agent", "AGENT_VERSION": p.Components["kata"].Version, "LIBC": "gnu", "ROOTFS_DIR": "/build/rootfs", "INSIDE_CONTAINER": "1", "REPO_URL": "/inputs/noble.sources", "HOME": "/tmp/builder-home", "XDG_CONFIG_HOME": "/tmp/builder-config", "PATH": "/inputs/bin:/usr/sbin:/usr/bin:/sbin:/bin", "ROOTFS_ONLY": "yes"}
	if e = run(ctx, "", env, log, "bash", "/kata/tools/osbuilder/rootfs-builder/rootfs.sh", "ubuntu"); e != nil {
		return e
	}
	b, e := output(ctx, "", "dpkg-query", "--admindir=/build/rootfs/var/lib/dpkg", "-W", "-f=${binary:Package}\t${Version}\t${Architecture}\n")
	if e != nil {
		return e
	}
	if len(b) == 0 {
		return errors.New("empty package inventory")
	}
	if e = os.WriteFile("/output/packages.tsv", b, 0644); e != nil {
		return e
	}
	for _, v := range [][2]string{{"/build/rootfs/var/lib/dpkg/status", "/output/audit-rootfs/var/lib/dpkg/status"}, {"/build/rootfs/usr/lib/os-release", "/output/audit-rootfs/usr/lib/os-release"}, {"/inputs/noble.sources", "/output/noble.sources"}, {"/builder-packages.tsv", "/output/builder-packages.tsv"}} {
		if e = copyFile(v[0], v[1], 0644); e != nil {
			return e
		}
	}
	if e = packageFiles("/build/rootfs/var/lib/dpkg/info", "/output/package-files.json"); e != nil {
		return e
	}
	env["ROOTFS_ONLY"] = ""
	if e = run(ctx, "", env, log, "bash", "/kata/tools/osbuilder/rootfs-builder/rootfs.sh", "-d"); e != nil {
		return e
	}
	if e = os.MkdirAll("/build/kata-service-source/src", 0755); e != nil {
		return e
	}
	if e = run(ctx, "", nil, log, "cp", "-a", "/kata/src/agent", "/build/kata-service-source/src/agent"); e != nil {
		return e
	}
	for _, name := range []string{"utils.mk", "VERSION"} {
		if e = copyFile("/kata/"+name, "/build/kata-service-source/"+name, 0644); e != nil {
			return e
		}
	}
	if e = run(ctx, "", env, log, "make", "-C", "/build/kata-service-source/src/agent", "install-services", "INIT=no", "LIBC=gnu", "DESTDIR=/build/rootfs", "VERSION="+p.Components["kata"].Version, "COMMIT="+p.Components["kata"].SourceRevision); e != nil {
		return e
	}
	if e = verify("/build/rootfs/usr/bin/kata-agent", agent); e != nil {
		return e
	}
	for _, name := range []string{"/build/rootfs/lib/systemd/systemd", "/build/rootfs/usr/bin/kata-agent"} {
		s, e := os.Stat(name)
		if e != nil {
			return e
		}
		if s.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("executable required: %s", name)
		}
	}
	if e = os.Symlink("/lib/systemd/systemd", "/build/rootfs/init"); e != nil {
		return e
	}
	o, e := os.OpenFile("/output/kata-ubuntu-noble-updated.initrd", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if e != nil {
		return e
	}
	e = WriteInitrd(ctx, "/build/rootfs", o)
	if e = errors.Join(e, o.Close()); e != nil {
		return e
	}
	if e = os.WriteFile("/output/final-agent.sha256", []byte(agent+"  /build/rootfs/usr/bin/kata-agent\n"), 0644); e != nil {
		return e
	}
	return checksums("/output/checksums.txt", []string{"/output/kata-ubuntu-noble-updated.initrd", "/output/packages.tsv", "/output/audit-rootfs/var/lib/dpkg/status"})
}

func MakeDevices(ctx context.Context, args []string, log io.Writer) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	pwd, e := os.Getwd()
	if e != nil {
		return e
	}
	if pwd != "/build/rootfs/dev" || strings.Join(args, " ") != "-v console tty ttyS null zero fd" {
		return errors.New("invalid MAKEDEV invocation")
	}
	entries, e := os.ReadDir(pwd)
	if e != nil {
		return e
	}
	removed := []string{}
	for _, d := range entries {
		removed = append(removed, d.Name())
		if e = os.RemoveAll(filepath.Join(pwd, d.Name())); e != nil {
			return e
		}
	}
	if e = writeJSON("/output/device-pruning.json", removed); e != nil {
		return e
	}
	for _, name := range []string{"console", "tty", "null", "zero", "ttyS0", "ttyS1", "ttyS2", "ttyS3"} {
		if e = os.WriteFile(filepath.Join(pwd, name), nil, 0600); e != nil {
			return e
		}
	}
	for _, v := range [][2]string{{"fd", "/proc/self/fd"}, {"stdin", "fd/0"}, {"stdout", "fd/1"}, {"stderr", "fd/2"}} {
		if e = os.Symlink(v[1], filepath.Join(pwd, v[0])); e != nil {
			return e
		}
	}
	_, e = fmt.Fprintln(log, "device metadata deferred to initrd writer")
	return e
}

func packageFiles(dir, dest string) error {
	paths, e := filepath.Glob(filepath.Join(dir, "*.list"))
	if e != nil {
		return e
	}
	m := map[string][]string{}
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		set := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
			if !strings.HasPrefix(line, "/") {
				return errors.New("invalid package file path")
			}
			for _, part := range strings.Split(line, "/") {
				if part == ".." {
					return errors.New("invalid package file path")
				}
			}
			name := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(line)), "/")
			if name != "" && name != "." {
				set[name] = true
			}
		}
		names := []string{}
		for name := range set {
			names = append(names, name)
		}
		sort.Strings(names)
		m[strings.TrimSuffix(filepath.Base(p), ".list")] = names
	}
	return writeJSON(dest, m)
}
func checksums(dest string, paths []string) error {
	var b strings.Builder
	for _, p := range paths {
		h, e := digestFile(p)
		if e != nil {
			return e
		}
		fmt.Fprintf(&b, "%s  %s\n", h, p)
	}
	return os.WriteFile(dest, []byte(b.String()), 0644)
}
