package ci

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestCacheArchiveRoundTripPreservesExecutableAndRelativeLink(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "target"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "target/tool"), []byte("compiled artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tool", filepath.Join(root, "target/link")); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := WriteZstd(&encoded, root, "target"); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := ExtractZstd(&encoded, destination, "target", 1024); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "target/link"))
	if err != nil || string(data) != "compiled artifact" {
		t.Fatalf("restored data %q: %v", data, err)
	}
	info, err := os.Stat(filepath.Join(destination, "target/tool"))
	if err != nil || info.Mode()&0111 == 0 {
		t.Fatalf("lost executable mode: %v", err)
	}
}

func TestCacheArchiveRejectsEscapesAndSizeLimits(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
		{Name: "target/link", Typeflag: tar.TypeSymlink, Linkname: "../../escape"},
		{Name: "target/link", Typeflag: tar.TypeSymlink, Linkname: "/escape"},
		{Name: "other/file", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
		{Name: "target/large", Typeflag: tar.TypeReg, Mode: 0644, Size: 2},
	} {
		t.Run(header.Name+header.Linkname, func(t *testing.T) {
			var encoded bytes.Buffer
			compressor, err := zstd.NewWriter(&encoded)
			if err != nil {
				t.Fatal(err)
			}
			archive := tar.NewWriter(compressor)
			if err := archive.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if header.Size > 0 {
				archive.Write(bytes.Repeat([]byte("a"), int(header.Size)))
			}
			archive.Close()
			compressor.Close()
			if err := ExtractZstd(&encoded, t.TempDir(), "target", 1); err == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
}

func TestMaximumRustVersionUsesNumericOrdering(t *testing.T) {
	version, err := MaximumRustVersion([]string{"1.9", "1.81", "1.81.3", ""})
	if err != nil || version != "1.81.3" {
		t.Fatalf("version %q: %v", version, err)
	}
	if _, err := MaximumRustVersion([]string{"nightly"}); err == nil {
		t.Fatal("accepted unstable MSRV")
	}
}
