package kata

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

type PackageOptions struct{ RepoDir, WorkDir string }
type Manifest struct {
	Archive                     string               `json:"archive"`
	SHA256                      string               `json:"sha256"`
	Files                       map[string]string    `json:"files"`
	Components                  map[string]Component `json:"components"`
	NativeQualificationRequired bool                 `json:"nativeQualificationRequired"`
}
type archiveFile struct {
	path string
	mode int64
}

func Package(ctx context.Context, o PackageOptions) (Manifest, error) {
	p, e := LoadPins(o.RepoDir)
	if e != nil {
		return Manifest{}, e
	}
	if len(p.Files) == 0 {
		return Manifest{}, errors.New("runtime file closure required")
	}
	work, e := filepath.Abs(o.WorkDir)
	if e != nil {
		return Manifest{}, e
	}
	tmp, e := os.MkdirTemp(work, ".package-")
	if e != nil {
		return Manifest{}, e
	}
	defer os.RemoveAll(tmp)
	files := map[string]archiveFile{}
	add := func(name string, r io.Reader, mode int64) error {
		if _, ok := p.Files[name]; !ok || !validArchivePath(name) {
			return fmt.Errorf("unexpected runtime path %q", name)
		}
		if _, ok := files[name]; ok {
			return fmt.Errorf("duplicate runtime path %q", name)
		}
		f, e := os.CreateTemp(tmp, "member-")
		if e != nil {
			return e
		}
		_, e = io.Copy(f, &contextReader{ctx, r})
		if e = errors.Join(e, f.Close()); e != nil {
			return e
		}
		files[name] = archiveFile{f.Name(), mode}
		return nil
	}
	q, e := os.Open(filepath.Join(work, "qemu/output/kata-static-qemu.tar.gz"))
	if e != nil {
		return Manifest{}, e
	}
	defer q.Close()
	gz, e := gzip.NewReader(q)
	if e != nil {
		return Manifest{}, e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return Manifest{}, e
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return Manifest{}, errors.New("regular runtime files required")
		}
		if e = add(strings.TrimPrefix(h.Name, "./"), tr, h.Mode); e != nil {
			return Manifest{}, e
		}
	}
	release := filepath.Join(work, "guest/kata.tar.zst")
	if e = verify(release, p.Components["kata"].SHA256); e != nil {
		return Manifest{}, e
	}
	f, e := os.Open(release)
	if e != nil {
		return Manifest{}, e
	}
	defer f.Close()
	zr, e := zstd.NewReader(f)
	if e != nil {
		return Manifest{}, e
	}
	defer zr.Close()
	tr = tar.NewReader(zr)
	shim := "opt/kata/runtime-rs/bin/containerd-shim-kata-v2"
	found := false
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return Manifest{}, e
		}
		if strings.TrimPrefix(h.Name, "./") != shim {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return Manifest{}, errors.New("regular shim required")
		}
		if e = add(shim, tr, 0755); e != nil {
			return Manifest{}, e
		}
		found = true
	}
	if !found {
		return Manifest{}, errors.New("upstream shim missing")
	}
	if e = verify(files[shim].path, p.Files[shim]); e != nil {
		return Manifest{}, e
	}
	kernel := "opt/kata/share/kata-containers/vmlinux-" + p.Components["kernel"].Version + "-" + p.Components["kernel"].ConfigVersion
	for _, v := range []struct {
		name, source string
		mode         int64
	}{{"opt/kata/libexec/virtiofsd", "virtiofsd/output/virtiofsd", 0755}, {kernel, "kernel/output/" + kernel, 0644}, {"opt/kata/share/kata-containers/guest.initrd", "guest/output/kata-ubuntu-noble-updated.initrd", 0644}} {
		f, e := os.Open(filepath.Join(work, v.source))
		if e != nil {
			return Manifest{}, e
		}
		e = add(v.name, f, v.mode)
		e = errors.Join(e, f.Close())
		if e != nil {
			return Manifest{}, e
		}
	}
	f, e = os.Open(filepath.Join(o.RepoDir, "images/kata-runtime/configuration.toml"))
	if e != nil {
		return Manifest{}, e
	}
	e = add("opt/kata/share/defaults/kata-containers/runtime-rs/configuration.toml", f, 0644)
	e = errors.Join(e, f.Close())
	if e != nil {
		return Manifest{}, e
	}
	if len(files) != len(p.Files) {
		return Manifest{}, errors.New("incomplete runtime closure")
	}
	name := "kata-runtime-amd64.tar"
	dest := filepath.Join(work, name)
	if _, e = os.Lstat(dest); !errors.Is(e, os.ErrNotExist) {
		return Manifest{}, errors.New("package output already exists")
	}
	a, e := os.CreateTemp(work, ".archive-")
	if e != nil {
		return Manifest{}, e
	}
	defer os.Remove(a.Name())
	tw := tar.NewWriter(a)
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	m := Manifest{Archive: name, Files: map[string]string{}, Components: p.Components, NativeQualificationRequired: true}
	for _, k := range keys {
		v := files[k]
		f, e := os.Open(v.path)
		if e != nil {
			a.Close()
			return Manifest{}, e
		}
		stat, e := f.Stat()
		if e == nil {
			e = tw.WriteHeader(&tar.Header{Name: k, Size: stat.Size(), Mode: v.mode, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR})
		}
		if e == nil {
			_, e = io.Copy(tw, &contextReader{ctx, f})
		}
		e = errors.Join(e, f.Close())
		if e != nil {
			a.Close()
			return Manifest{}, e
		}
		m.Files[k], e = digestFile(v.path)
		if e != nil {
			a.Close()
			return Manifest{}, e
		}
	}
	if e = errors.Join(tw.Close(), a.Sync(), a.Close()); e != nil {
		return Manifest{}, e
	}
	m.SHA256, e = digestFile(a.Name())
	if e != nil {
		return Manifest{}, e
	}
	if e = os.Link(a.Name(), dest); e != nil {
		return Manifest{}, e
	}
	if e = writeJSON(strings.TrimSuffix(dest, ".tar")+".json", m); e != nil {
		os.Remove(dest)
		return Manifest{}, e
	}
	return m, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
func bytesDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func verifyArchive(ctx context.Context, path string, files map[string]string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	tr := tar.NewReader(f)
	seen := map[string]bool{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		expected, ok := files[h.Name]
		if !ok || seen[h.Name] || !validArchivePath(h.Name) || h.Typeflag != tar.TypeReg {
			return errors.New("invalid candidate archive member")
		}
		seen[h.Name] = true
		hash := sha256.New()
		if _, e = io.Copy(hash, &contextReader{ctx, tr}); e != nil {
			return e
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected {
			return fmt.Errorf("archive manifest mismatch: %s", h.Name)
		}
	}
	if len(seen) != len(files) {
		return errors.New("incomplete candidate archive")
	}
	return nil
}
