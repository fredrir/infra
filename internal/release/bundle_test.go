package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archiveExecutable(t *testing.T, path, name string, program []byte, mode int64) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(program)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(program); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutableVerificationChecksFormatArchitectureAndMode(t *testing.T) {
	program := make([]byte, 64)
	copy(program, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(program[16:], 2)
	binary.LittleEndian.PutUint16(program[18:], 62)
	binary.LittleEndian.PutUint32(program[20:], 1)
	binary.LittleEndian.PutUint16(program[52:], 64)
	path := filepath.Join(t.TempDir(), "tool.tar.gz")
	archiveExecutable(t, path, "tool", program, 0755)
	if err := VerifyExecutable(path, "tool", "x86_64-unknown-linux-musl"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"aarch64-unknown-linux-musl", "x86_64-apple-darwin"} {
		if err := VerifyExecutable(path, "tool", target); err == nil {
			t.Errorf("accepted wrong architecture/format %s", target)
		}
	}
	archiveExecutable(t, path, "tool", program, 0644)
	if err := VerifyExecutable(path, "tool", "x86_64-unknown-linux-musl"); err == nil {
		t.Fatal("accepted non-executable file")
	}
	archiveExecutable(t, path, "../tool", program, 0755)
	if err := VerifyExecutable(path, "tool", "x86_64-unknown-linux-musl"); err == nil {
		t.Fatal("accepted archive traversal")
	}
}

func TestChecksumsRejectCorruptionAndEscape(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "artifact")
	if err := os.WriteFile(path, []byte("artifact"), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("%x  artifact\n", sha256.Sum256([]byte("artifact")))
	if err := os.WriteFile(filepath.Join(directory, "checksums.txt"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksums(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksums(directory); err == nil {
		t.Fatal("accepted corrupt artifact")
	}
	if err := os.WriteFile(filepath.Join(directory, "checksums.txt"), []byte(strings.ReplaceAll(manifest, "artifact", "../artifact")), 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksums(directory); err == nil {
		t.Fatal("accepted checksum traversal")
	}
}
