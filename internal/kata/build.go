package kata

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Build(ctx context.Context, o BuildOptions, x Executor) error {
	if o.Component == "package" {
		_, e := Package(ctx, PackageOptions{o.RepoDir, o.WorkDir})
		return e
	}
	if o.Limits == (Limits{}) {
		o.Limits = DefaultLimits()
	}
	if e := o.Limits.Validate(); e != nil {
		return e
	}
	if x == nil {
		return errors.New("container executor required")
	}
	switch o.Component {
	case "qemu", "kernel", "guest", "virtiofsd":
	default:
		return errors.New("unknown Kata component")
	}
	if o.Component == "guest" && !o.AllowPrivilegedGuest {
		return errors.New("guest build requires an isolated engine and --allow-privileged-guest")
	}
	if e := validateBinary(o.Binary); e != nil {
		return e
	}
	p, e := LoadPins(o.RepoDir)
	if e != nil {
		return e
	}
	c := p.Components[o.Component]
	if !strings.Contains(c.Builder, "@sha256:") {
		return errors.New("digest-pinned builder required")
	}
	work, e := filepath.Abs(o.WorkDir)
	if e != nil {
		return e
	}
	if work == string(filepath.Separator) {
		return errors.New("work directory cannot be root")
	}
	if e = os.MkdirAll(work, 0755); e != nil {
		return e
	}
	dest := filepath.Join(work, o.Component)
	if _, e = os.Lstat(dest); !errors.Is(e, os.ErrNotExist) {
		return errors.New("component output already exists")
	}
	part, e := os.MkdirTemp(work, "."+o.Component+"-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(part)
	ctx, cancel := context.WithTimeout(ctx, o.Limits.Timeout)
	defer cancel()
	for _, d := range []string{"build", "output", "inputs", "repo/ansible/roles/ci_runtime/files"} {
		if e = os.MkdirAll(filepath.Join(part, d), 0755); e != nil {
			return e
		}
	}
	stagePins := Pins{Architecture: p.Architecture, Components: map[string]Component{o.Component: c}}
	if o.Component == "guest" {
		stagePins.Components["kata"] = p.Components["kata"]
	}
	if e = writeJSON(filepath.Join(part, "repo/ansible/roles/ci_runtime/files/kata-runtime.json"), stagePins); e != nil {
		return e
	}
	r := ContainerRequest{Image: c.Builder, Limits: o.Limits, OutputDir: filepath.Join(part, "output"), OutputPath: "/output", Env: map[string]string{"HOME": "/tmp/build-home", "XDG_CONFIG_HOME": "/tmp/build-config", "OMP_NUM_THREADS": "2", "OMP_THREAD_LIMIT": "2"}, Mounts: []Mount{{filepath.Join(part, "repo"), "/repo"}, {filepath.Join(part, "build"), "/build"}, {o.Binary, "/infra"}}}
	if o.Component != "virtiofsd" {
		r.Sources = append(r.Sources, GitSource{"https://github.com/kata-containers/kata-containers.git", p.Components["kata"].SourceRevision, "/kata"})
	}
	switch o.Component {
	case "qemu":
		r.Sources = append(r.Sources, GitSource{c.Repository, c.SourceRevision, "/qemu"})
		r.OutputPath = "/share"
		r.Args = []string{"/infra", "kata", "qemu-worker"}
	case "kernel":
		tarball := filepath.Join(part, "build/linux-"+c.Version+".tar.xz")
		if e = fetch(ctx, c.URL, tarball, c.SHA256); e != nil {
			return e
		}
		if e = os.WriteFile(tarball+".sha256", []byte(c.SHA256+"  linux-"+c.Version+".tar.xz\n"), 0644); e != nil {
			return e
		}
		r.Args = []string{"/infra", "kata", "kernel-worker"}
	case "virtiofsd":
		r.Sources = append(r.Sources, GitSource{c.Repository, c.SourceRevision, "/source"})
		r.Args = []string{"/infra", "kata", "virtiofsd-worker"}
	case "guest":
		contextDir := filepath.Join(part, "context")
		if e = os.Mkdir(contextDir, 0755); e != nil {
			return e
		}
		if e = fetch(ctx, "https://snapshot.ubuntu.com/ubuntu/20260912T000000Z/pool/main/c/ca-certificates/ca-certificates_20240203_all.deb", filepath.Join(contextDir, "ca.deb"), "641de77d8f142cfd62a1a6f964ba67b20754d3337c480efb529d086075a06c9a"); e != nil {
			return e
		}
		if e = fetch(ctx, p.Components["kata"].URL, filepath.Join(part, "kata.tar.zst"), p.Components["kata"].SHA256); e != nil {
			return e
		}
		for _, name := range []string{"guest-builder.sources"} {
			if e = copyFile(filepath.Join(o.RepoDir, "images/kata-runtime", name), filepath.Join(contextDir, name), 0644); e != nil {
				return e
			}
		}
		if e = copyFile(o.Binary, filepath.Join(contextDir, "infra"), 0755); e != nil {
			return e
		}
		if e = copyFile(filepath.Join(o.RepoDir, "images/kata-runtime/noble.sources"), filepath.Join(part, "inputs/noble.sources"), 0644); e != nil {
			return e
		}
		r.BuildContext = contextDir
		r.Mounts = append(r.Mounts, Mount{filepath.Join(part, "inputs"), "/inputs"}, Mount{filepath.Join(part, "kata.tar.zst"), "/inputs/kata.tar.zst"})
		r.Privileged = true
		r.Args = []string{"/infra", "kata", "guest-worker"}
	}
	if e = x.Execute(ctx, r); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	return os.Rename(part, dest)
}

func validateBinary(path string) error {
	f, e := elf.Open(path)
	if e != nil {
		return fmt.Errorf("prebuilt Linux infra binary: %w", e)
	}
	defer f.Close()
	if f.Machine != elf.EM_X86_64 || f.Class != elf.ELFCLASS64 {
		return errors.New("Linux amd64 infra binary required")
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return errors.New("statically linked infra binary required")
		}
	}
	return nil
}
func fetch(ctx context.Context, url, dest, digest string) error {
	if !strings.HasPrefix(url, "https://") || !validDigest(digest) {
		return errors.New("HTTPS URL and SHA256 required")
	}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if e != nil {
		return e
	}
	client := &http.Client{Timeout: 180 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 10 || req.URL.Scheme != "https" {
			return errors.New("invalid download redirect")
		}
		return nil
	}}
	res, e := client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", res.StatusCode)
	}
	f, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(f, res.Body)
	e = errors.Join(e, f.Close())
	if e != nil {
		os.Remove(dest)
		return e
	}
	if e = verify(dest, digest); e != nil {
		os.Remove(dest)
		return e
	}
	return nil
}
