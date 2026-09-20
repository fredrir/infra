package kata

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	work := t.TempDir()
	files := map[string][]byte{"opt/kata/bin/qemu-system-x86_64": []byte("qemu"), "opt/kata/runtime-rs/bin/containerd-shim-kata-v2": []byte("shim"), "opt/kata/libexec/virtiofsd": []byte("virtiofsd"), "opt/kata/share/kata-containers/vmlinux-1-2": []byte("kernel"), "opt/kata/share/kata-containers/guest.initrd": []byte("initrd"), "opt/kata/share/defaults/kata-containers/runtime-rs/configuration.toml": []byte("configuration")}
	p := Pins{Architecture: "amd64", Files: map[string]string{}, Components: map[string]Component{"kernel": {Version: "1", ConfigVersion: "2"}, "kata": {Version: "1"}}}
	for name, b := range files {
		p.Files[name] = bytesDigest(b)
	}
	write := func(name string, b []byte) {
		t.Helper()
		if e := os.MkdirAll(filepath.Dir(name), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(name, b, 0644); e != nil {
			t.Fatal(e)
		}
	}
	var q bytes.Buffer
	gz := gzip.NewWriter(&q)
	tw := tar.NewWriter(gz)
	name := "opt/kata/bin/qemu-system-x86_64"
	if e := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(files[name]))}); e != nil {
		t.Fatal(e)
	}
	tw.Write(files[name])
	tw.Close()
	gz.Close()
	write(filepath.Join(work, "qemu/output/kata-static-qemu.tar.gz"), q.Bytes())
	var release bytes.Buffer
	zr, e := zstd.NewWriter(&release)
	if e != nil {
		t.Fatal(e)
	}
	tw = tar.NewWriter(zr)
	name = "opt/kata/runtime-rs/bin/containerd-shim-kata-v2"
	if e = tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0755, Size: int64(len(files[name]))}); e != nil {
		t.Fatal(e)
	}
	tw.Write(files[name])
	tw.Close()
	zr.Close()
	c := p.Components["kata"]
	c.SHA256 = bytesDigest(release.Bytes())
	p.Components["kata"] = c
	write(filepath.Join(work, "guest/kata.tar.zst"), release.Bytes())
	for _, pair := range [][2]string{{"virtiofsd/output/virtiofsd", "opt/kata/libexec/virtiofsd"}, {"kernel/output/opt/kata/share/kata-containers/vmlinux-1-2", "opt/kata/share/kata-containers/vmlinux-1-2"}, {"guest/output/kata-ubuntu-noble-updated.initrd", "opt/kata/share/kata-containers/guest.initrd"}} {
		write(filepath.Join(work, pair[0]), files[pair[1]])
	}
	write(filepath.Join(root, "images/kata-runtime/configuration.toml"), files["opt/kata/share/defaults/kata-containers/runtime-rs/configuration.toml"])
	b, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	write(filepath.Join(root, "ansible/roles/ci_runtime/files/kata-runtime.json"), b)
	return root, work
}

func TestPackageProducesDeterministicCompleteClosure(t *testing.T) {
	var digest string
	for i := 0; i < 2; i++ {
		root, work := fixture(t)
		m, e := Package(context.Background(), PackageOptions{root, work})
		if e != nil {
			t.Fatal(e)
		}
		if !m.NativeQualificationRequired || len(m.Files) != 6 {
			t.Fatalf("invalid manifest: %+v", m)
		}
		if e = verifyArchive(context.Background(), filepath.Join(work, m.Archive), m.Files); e != nil {
			t.Fatal(e)
		}
		if i > 0 && digest != m.SHA256 {
			t.Fatal("equivalent inputs produced different archives")
		}
		digest = m.SHA256
		f, e := os.Open(filepath.Join(work, m.Archive))
		if e != nil {
			t.Fatal(e)
		}
		tr := tar.NewReader(f)
		previous := ""
		count := 0
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			if h.Name <= previous || h.Uid != 0 || h.Gid != 0 || h.ModTime.Unix() != 0 || h.Typeflag != tar.TypeReg {
				t.Fatalf("invalid archive metadata: %+v", h)
			}
			b, e := io.ReadAll(tr)
			if e != nil {
				t.Fatal(e)
			}
			if bytesDigest(b) != m.Files[h.Name] {
				t.Fatal("manifest differs from archive")
			}
			previous = h.Name
			count++
		}
		f.Close()
		if count != 6 {
			t.Fatal(count)
		}
		if _, e = Package(context.Background(), PackageOptions{root, work}); e == nil {
			t.Fatal("existing package overwritten")
		}
	}
}

func TestPackageRejectsUnverifiedUpstream(t *testing.T) {
	root, work := fixture(t)
	if e := os.WriteFile(filepath.Join(work, "guest/kata.tar.zst"), []byte("corrupt"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := Package(context.Background(), PackageOptions{root, work}); e == nil || !strings.Contains(e.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum failure, got %v", e)
	}
	if _, e := os.Stat(filepath.Join(work, "kata-runtime-amd64.tar")); !os.IsNotExist(e) {
		t.Fatal("failed package published")
	}
}

func TestPackageRejectsUnexpectedAndNonregularMembers(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  byte
	}{{"../escape", tar.TypeReg}, {"opt/kata/bin/qemu-system-x86_64", tar.TypeSymlink}} {
		t.Run(tc.name, func(t *testing.T) {
			root, work := fixture(t)
			f, e := os.Create(filepath.Join(work, "qemu/output/kata-static-qemu.tar.gz"))
			if e != nil {
				t.Fatal(e)
			}
			gz := gzip.NewWriter(f)
			tw := tar.NewWriter(gz)
			if e = tw.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.typ, Linkname: "/etc/passwd", Mode: 0755}); e != nil {
				t.Fatal(e)
			}
			tw.Close()
			gz.Close()
			f.Close()
			if _, e = Package(context.Background(), PackageOptions{root, work}); e == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
}

func TestPackageCancellationDoesNotPublish(t *testing.T) {
	root, work := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := Package(ctx, PackageOptions{root, work}); e == nil {
		t.Fatal("cancelled package succeeded")
	}
	if _, e := os.Stat(filepath.Join(work, "kata-runtime-amd64.tar")); !os.IsNotExist(e) {
		t.Fatal("cancelled package published")
	}
}
