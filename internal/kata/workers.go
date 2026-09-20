package kata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/fredrir/infra/internal/process"
)

type DoctorReport struct {
	OS           string  `json:"os"`
	Architecture string  `json:"architecture"`
	CPUs         float64 `json:"cpuLimit"`
	MemoryBytes  int64   `json:"memoryLimit"`
	PIDs         int64   `json:"pidLimit"`
}

func Doctor(l Limits) (DoctorReport, error) {
	r := DoctorReport{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	if e := l.Validate(); e != nil {
		return r, e
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return r, errors.New("native Linux amd64 required")
	}
	return doctorCgroup("/sys/fs/cgroup", l, r)
}
func doctorCgroup(root string, l Limits, r DoctorReport) (DoctorReport, error) {
	b, e := os.ReadFile(filepath.Join(root, "cpu.max"))
	if e != nil {
		return r, e
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 || parts[0] == "max" {
		return r, errors.New("bounded cgroup CPU quota required")
	}
	q, e := strconv.ParseFloat(parts[0], 64)
	if e != nil {
		return r, e
	}
	period, e := strconv.ParseFloat(parts[1], 64)
	if e != nil || period <= 0 {
		return r, errors.New("invalid CPU quota")
	}
	r.CPUs = q / period
	if r.CPUs <= 0 || r.CPUs > float64(l.CPUs) {
		return r, errors.New("CPU quota exceeds build limit")
	}
	for _, v := range []struct {
		name  string
		limit int64
		dest  *int64
	}{{"memory.max", l.MemoryBytes, &r.MemoryBytes}, {"pids.max", int64(l.PIDs), &r.PIDs}} {
		b, e := os.ReadFile(filepath.Join(root, v.name))
		if e != nil {
			return r, e
		}
		n, e := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if e != nil || n <= 0 || n > v.limit {
			return r, fmt.Errorf("%s must be bounded at %d", v.name, v.limit)
		}
		*v.dest = n
	}
	b, e = os.ReadFile(filepath.Join(root, "memory.swap.max"))
	if e != nil {
		return r, e
	}
	if strings.TrimSpace(string(b)) != "0" {
		return r, errors.New("swap must be disabled")
	}
	return r, nil
}

func Worker(ctx context.Context, args []string, l Limits, engineBounded bool, log io.Writer) error {
	if len(args) == 0 {
		return errors.New("worker command required")
	}
	if e := l.Validate(); e != nil {
		return e
	}
	if !engineBounded {
		if _, e := Doctor(l); e != nil {
			return e
		}
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return errors.New("native Linux amd64 worker required")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if e := boundAffinity(l.CPUs); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	env := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[key] = value
		}
	}
	for key, value := range map[string]string{"HOME": "/tmp/build-home", "XDG_CONFIG_HOME": "/tmp/build-config", "OMP_NUM_THREADS": strconv.Itoa(l.CPUs), "OMP_THREAD_LIMIT": strconv.Itoa(l.CPUs), "MAKEFLAGS": "-j" + strconv.Itoa(l.CPUs), "CARGO_BUILD_JOBS": strconv.Itoa(l.CPUs)} {
		env[key] = value
	}
	return run(ctx, "", env, log, args...)
}

func Kernel(ctx context.Context, log io.Writer) error {
	p, e := LoadPins("/repo")
	if e != nil {
		return e
	}
	k := p.Components["kernel"]
	tarball := "/build/linux-" + k.Version + ".tar.xz"
	if e = verify(tarball, k.SHA256); e != nil {
		return e
	}
	if e = os.Mkdir("/build/patch-check", 0755); e != nil {
		return e
	}
	args := []string{"tar", "-xJf", tarball, "-C", "/build/patch-check", "--strip-components=1"}
	seen := map[string]bool{}
	for _, patch := range k.PackagingPatches {
		if !filepath.IsLocal(patch.SourcePath) || !filepath.IsLocal(patch.Path) {
			return errors.New("invalid patch path")
		}
		if !seen[patch.SourcePath] {
			args = append(args, "linux-"+k.Version+"/"+patch.SourcePath)
			seen[patch.SourcePath] = true
		}
	}
	if e = run(ctx, "", nil, log, args...); e != nil {
		return e
	}
	for _, patch := range k.PackagingPatches {
		file := filepath.Join("/kata/tools/packaging/kernel/patches/6.18.x", patch.Path)
		if e = verify(file, patch.SHA256); e != nil {
			return e
		}
		direction := "--forward"
		switch patch.Action {
		case "retain":
		case "omit-already-applied":
			direction = "--reverse"
		default:
			return errors.New("invalid patch action")
		}
		if e = run(ctx, "", nil, log, "patch", "--dry-run", "--batch", direction, "-p1", "-d", "/build/patch-check", "-i", file); e != nil {
			return e
		}
		if patch.Action == "omit-already-applied" {
			if e = os.Remove(file); e != nil {
				return e
			}
		}
	}
	env := map[string]string{"DESTDIR": "/output", "PREFIX": "/opt/kata", "KERNEL_DEBUG_ENABLED": "no", "HOME": "/tmp/build-home", "XDG_CONFIG_HOME": "/tmp/build-config"}
	for _, phase := range []string{"setup", "build", "install"} {
		if e = run(ctx, "/build", env, log, "bash", "/kata/tools/packaging/kernel/build-kernel.sh", "-v", k.Version, "-a", "x86_64", "-t", "qemu", phase); e != nil {
			return e
		}
	}
	return nil
}

func Qemu(ctx context.Context, log io.Writer) error {
	p, e := LoadPins("/repo")
	if e != nil {
		return e
	}
	c := p.Components["qemu"]
	if !filepath.IsLocal(c.Tag) || strings.Contains(c.Tag, "/") {
		return errors.New("invalid QEMU tag")
	}
	if e = run(ctx, "/qemu", nil, log, "git", "tag", "--force", c.Tag, c.SourceRevision); e != nil {
		return e
	}
	marker := filepath.Join("/kata/tools/packaging/qemu/patches/tag_patches", c.Tag, "no_patches.txt")
	if e = os.MkdirAll(filepath.Dir(marker), 0755); e != nil {
		return e
	}
	if e = os.WriteFile(marker, nil, 0644); e != nil {
		return e
	}
	env := map[string]string{"HOME": "/tmp/build-home", "XDG_CONFIG_HOME": "/tmp/build-config", "QEMU_REPO": "file:///qemu", "QEMU_VERSION_NUM": c.Tag, "HYPERVISOR_NAME": "kata-qemu", "PKGVERSION": "kata-static", "PREFIX": "/opt/kata", "QEMU_DESTDIR": "/tmp/qemu-static", "QEMU_TARBALL": "kata-static-qemu.tar.gz", "ARCH": "x86_64"}
	return run(ctx, "", env, log, "bash", "/kata/tools/packaging/static-build/qemu/build-qemu.sh")
}

func Virtiofsd(ctx context.Context, log io.Writer) error {
	p, e := LoadPins("/repo")
	if e != nil {
		return e
	}
	if e = verify("/source/Cargo.lock", p.Components["virtiofsd"].CargoLockSHA256); e != nil {
		return e
	}
	for _, d := range []string{"/tmp/build-home", "/tmp/build-config", "/build/cargo-home", "/output"} {
		if e = os.MkdirAll(d, 0755); e != nil {
			return e
		}
	}
	env := map[string]string{"HOME": "/tmp/build-home", "XDG_CONFIG_HOME": "/tmp/build-config", "CARGO_HOME": "/build/cargo-home", "CARGO_TARGET_DIR": "/build/target", "CARGO_BUILD_JOBS": "2", "CARGO_INCREMENTAL": "0", "RUSTFLAGS": "-C target-feature=+crt-static -C link-self-contained=yes", "LIBSECCOMP_LINK_TYPE": "static", "LIBSECCOMP_LIB_PATH": "/usr/lib", "LIBCAPNG_LINK_TYPE": "static", "LIBCAPNG_LIB_PATH": "/usr/lib"}
	if e = run(ctx, "", env, log, "apk", "add", "--no-cache", "libcap-ng-static=0.8.5-r2", "libseccomp-static=2.6.0-r2", "musl-dev=1.2.6-r2", "gcc=15.2.0-r5", "binutils=2.45.1-r1", "pkgconf=2.5.1-r0"); e != nil {
		return e
	}
	if e = copyFile("/lib/apk/db/installed", "/output/apk-installed", 0644); e != nil {
		return e
	}
	for _, v := range []struct {
		name string
		args []string
	}{{"rustc.txt", []string{"rustc", "-vV"}}, {"cargo.txt", []string{"cargo", "-vV"}}} {
		b, e := output(ctx, "", v.args...)
		if e != nil {
			return e
		}
		if e = os.WriteFile("/output/"+v.name, b, 0644); e != nil {
			return e
		}
	}
	b, e := output(ctx, "", "rustc", "--version")
	if e != nil {
		return e
	}
	if !strings.HasPrefix(string(b), "rustc 1.98.1 ") {
		return errors.New("unexpected Rust toolchain")
	}
	if e = run(ctx, "/source", env, log, "cargo", "rustc", "--locked", "--release", "--target", "x86_64-unknown-linux-musl", "--bin", "virtiofsd", "--", "-C", "link-arg=-Wl,-Map,/output/link.map"); e != nil {
		return e
	}
	if e = copyFile("/build/target/x86_64-unknown-linux-musl/release/virtiofsd", "/output/virtiofsd", 0755); e != nil {
		return e
	}
	if e = copyFile("/source/Cargo.lock", "/output/Cargo.lock", 0644); e != nil {
		return e
	}
	for _, v := range []struct {
		name string
		args []string
	}{{"cargo-metadata.json", []string{"cargo", "metadata", "--locked", "--format-version", "1"}}, {"cargo-features.txt", []string{"cargo", "tree", "--locked", "--target", "x86_64-unknown-linux-musl", "-e", "features"}}, {"elf-program-headers.txt", []string{"readelf", "-lW", "/output/virtiofsd"}}, {"elf-dynamic.txt", []string{"readelf", "-d", "/output/virtiofsd"}}} {
		f, e := os.Create("/output/" + v.name)
		if e != nil {
			return e
		}
		_, e = process.Run(ctx, process.Options{Name: v.args[0], Args: v.args[1:], Dir: "/source", Env: cleanEnv(env), Stdout: f, Stderr: log})
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
		if strings.HasPrefix(v.name, "elf-") {
			b, e := os.ReadFile("/output/" + v.name)
			if e != nil {
				return e
			}
			if strings.Contains(string(b), "INTERP") || strings.Contains(string(b), "NEEDED") {
				return errors.New("virtiofsd must be static")
			}
		}
	}
	b, e = output(ctx, "", "rustc", "--print", "sysroot")
	if e != nil {
		return e
	}
	libs := []string{"/usr/lib/libseccomp.a", "/usr/lib/libcap-ng.a"}
	e = filepath.WalkDir(filepath.Join(strings.TrimSpace(string(b)), "lib/rustlib/x86_64-unknown-linux-musl/lib/self-contained"), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type().IsRegular() {
			libs = append(libs, p)
		}
		return nil
	})
	if e != nil {
		return e
	}
	if e = checksums("/output/native-library-hashes.txt", libs); e != nil {
		return e
	}
	return checksums("/output/checksums.txt", []string{"/output/virtiofsd", "/output/Cargo.lock", "/output/apk-installed", "/output/link.map"})
}
