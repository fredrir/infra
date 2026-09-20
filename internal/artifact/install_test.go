package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInstallVerifiesDownloadAndReusesCachedBinary(t *testing.T) {
	data := linuxFixture()
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write(data) }))
	defer server.Close()
	o := InstallOptions{URL: server.URL, Client: server.Client(), Revision: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Platform: "linux/amd64", CacheDir: t.TempDir(), Destination: filepath.Join(t.TempDir(), "infra")}
	first, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if first.CacheHit || first.DownloadedBytes != int64(len(data)) {
		t.Fatalf("receipt: %+v", first)
	}
	second, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !second.CacheHit || calls.Load() != 1 {
		t.Fatalf("cache was not reused: %+v", second)
	}
	info, err := os.Stat(o.Destination)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode: %v, %v", info, err)
	}
}

func TestInstallRejectsCorruptDownloadWithoutReplacingDestination(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("corrupt")) }))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "infra")
	if err := os.WriteFile(destination, []byte("previous"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := InstallOptions{URL: server.URL, Client: server.Client(), Revision: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Platform: "linux/amd64", CacheDir: t.TempDir(), Destination: destination}
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupt download accepted: %v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "previous" {
		t.Fatal("existing binary replaced")
	}
}

func TestInstallRevalidatesCacheAndRejectsWrongArchitecture(t *testing.T) {
	data := linuxFixture()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer server.Close()
	o := InstallOptions{URL: server.URL, Client: server.Client(), Revision: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Platform: "linux/amd64", CacheDir: t.TempDir()}
	first, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(first.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.Path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Install(context.Background(), o)
	if err != nil || second.CacheHit {
		t.Fatalf("corruption was not repaired: %+v %v", second, err)
	}
	o.Platform = "linux/arm64"
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "architecture") {
		t.Fatalf("wrong architecture accepted: %v", err)
	}
}

func linuxFixture() []byte {
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 2)
	binary.LittleEndian.PutUint16(data[18:], 62)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint16(data[52:], 64)
	binary.LittleEndian.PutUint16(data[54:], 56)
	binary.LittleEndian.PutUint16(data[58:], 64)
	return data
}
