package operations

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestSDKArchiveIsDeterministicAndPreservesLinks(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "usr/include/header.h", "header contents")
	if err := os.Link(filepath.Join(root, "usr/include/header.h"), filepath.Join(root, "usr/include/second.h")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/include", filepath.Join(root, "include")); err != nil {
		t.Fatal(err)
	}
	first, err := packageSDK(context.Background(), root, "15.4", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "usr/include/header.h"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	second, err := packageSDK(context.Background(), root, "15.4", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("SDK metadata made archive nondeterministic")
	}
	data, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if first.SHA256 != hex.EncodeToString(sum[:]) || first.Object != "MacOSX15.4.sdk.tar.zst" {
		t.Fatal(first)
	}
	file, err := os.Open(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	zr, err := zstd.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	headers := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		headers[h.Name] = h
		if h.Uid != 0 || h.Gid != 0 || h.ModTime.Unix() != 0 {
			t.Fatalf("nondeterministic ownership/time: %+v", h)
		}
		if h.Name == "MacOSX.sdk/usr/include/header.h" {
			b, err := io.ReadAll(tr)
			if err != nil || string(b) != "header contents" {
				t.Fatalf("payload: %s %v", b, err)
			}
		}
	}
	if h := headers["MacOSX.sdk/include"]; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "usr/include" {
		t.Fatal(h)
	}
	if h := headers["MacOSX.sdk/usr/include/second.h"]; h == nil || h.Typeflag != tar.TypeLink || h.Linkname != "MacOSX.sdk/usr/include/header.h" {
		t.Fatal(h)
	}
	if headers["MacOSX.sdk/"] == nil {
		t.Fatal("SDK root not normalized")
	}
}

func TestSDKRejectsUnsafeOutputAndCleansCancellation(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "header", "content")
	destination := t.TempDir()
	first, err := packageSDK(context.Background(), root, "15", destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := packageSDK(context.Background(), root, "15", destination); err == nil {
		t.Fatal("overwrote archive")
	}
	data, _ := os.ReadFile(first.Path)
	if len(data) == 0 {
		t.Fatal("removed existing archive")
	}
	for _, bad := range []string{"../15", "", "15 beta"} {
		if _, err := packageSDK(context.Background(), root, bad, t.TempDir()); err == nil {
			t.Fatal("accepted version", bad)
		}
	}
	if _, err := packageSDK(context.Background(), root, "15", root); err == nil {
		t.Fatal("accepted recursive archive")
	}
	output := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := packageSDK(ctx, root, "15", output); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial archive remains: %v %v", entries, err)
	}
}
