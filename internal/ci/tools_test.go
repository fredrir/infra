package ci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolChecksumFailurePreservesInstalledBinary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "new binary") }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}
	asset := ToolAsset{URL: server.URL, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("other")))}
	if err := InstallTool(context.Background(), server.Client(), asset, path, ""); err == nil {
		t.Fatal("accepted wrong checksum")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "old binary" {
		t.Fatalf("previous binary changed: %q %v", data, err)
	}
	asset.Digest = fmt.Sprintf("%x", sha256.Sum256([]byte("new binary")))
	if err := InstallTool(context.Background(), server.Client(), asset, path, ""); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "new binary" {
		t.Fatalf("binary was not replaced: %q %v", data, err)
	}
}

func TestVerifiedToolCacheAvoidsNetworkAndRepairsCorruption(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++; io.WriteString(w, "binary") }))
	defer server.Close()
	asset := ToolAsset{URL: server.URL, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("binary")))}
	path := filepath.Join(t.TempDir(), "tool")
	for range 2 {
		if err := InstallTool(context.Background(), server.Client(), asset, path, ""); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 1 {
		t.Fatalf("warm requests: %d", requests)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := InstallTool(context.Background(), server.Client(), asset, path, ""); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("corruption was not repaired: %d", requests)
	}
}

func toolArchive(t *testing.T, member string, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(archive.Close(), compressed.Close()); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestToolDownloadsAreVerifiedOnEveryInstall(t *testing.T) {
	for _, test := range []struct {
		name, member string
		asset        []byte
	}{
		{name: "archive", member: "release/tool", asset: toolArchive(t, "release/tool", []byte("binary"))},
		{name: "binary", asset: []byte("binary")},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++; w.Write(test.asset) }))
			defer server.Close()
			asset := ToolAsset{URL: server.URL, Digest: fmt.Sprintf("%x", sha256.Sum256(test.asset)), Member: test.member}
			downloads := t.TempDir()
			install := func() {
				t.Helper()
				path := filepath.Join(t.TempDir(), "tool")
				if err := InstallTool(context.Background(), server.Client(), asset, path, downloads); err != nil {
					t.Fatal(err)
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != "binary" {
					t.Fatalf("installed %q: %v", data, err)
				}
			}
			install()
			install()
			if requests != 1 {
				t.Fatalf("verified download fetched %d times", requests)
			}
			if err := os.WriteFile(filepath.Join(downloads, asset.Digest), []byte("poison"), 0o600); err != nil {
				t.Fatal(err)
			}
			install()
			if cached, err := os.ReadFile(filepath.Join(downloads, asset.Digest)); requests != 2 || err != nil || !bytes.Equal(cached, test.asset) {
				t.Fatalf("poisoned download was not replaced: %d requests, %v", requests, err)
			}
		})
	}
}

func TestToolDownloadsKeepOnlyPinnedAssets(t *testing.T) {
	downloads := t.TempDir()
	pinned := checkToolAssets["tofu"].Digest
	for _, name := range []string{pinned, strings.Repeat("0", 64), ".download-123"} {
		if err := os.WriteFile(filepath.Join(downloads, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneToolDownloads(downloads); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(downloads); err != nil || len(entries) != 1 || entries[0].Name() != pinned {
		t.Fatalf("downloads kept %v: %v", entries, err)
	}
}
