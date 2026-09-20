package ci

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if err := InstallTool(context.Background(), server.Client(), asset, path); err == nil {
		t.Fatal("accepted wrong checksum")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "old binary" {
		t.Fatalf("previous binary changed: %q %v", data, err)
	}
	asset.Digest = fmt.Sprintf("%x", sha256.Sum256([]byte("new binary")))
	if err := InstallTool(context.Background(), server.Client(), asset, path); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "new binary" {
		t.Fatalf("binary was not replaced: %q %v", data, err)
	}
}
