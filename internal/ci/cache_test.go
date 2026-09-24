package ci

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func init() {
	if os.Getenv("INFRA_CACHE_TEST_TOOL") == "1" && filepath.Base(os.Args[0]) == "rustc" {
		fmt.Println("rustc 1.98.1\nbinary: rustc\nhost: x86_64-unknown-linux-gnu")
		os.Exit(0)
	}
}

func TestRustCacheRestoresFreshCheckoutAndBoundsWrites(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binaries := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(binaries, "rustc")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binaries+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCCACHE_BUCKET", "ci-example-main")
	t.Setenv("AWS_ACCESS_KEY_ID", "GK"+strings.Repeat("a", 24))
	secret := strings.Repeat("b", 64)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
	t.Setenv("SCCACHE_S3_RW_MODE", "READ_WRITE")
	t.Setenv("RUST_TARGET_CACHE_LIMIT_KIB", "1024")
	var mutex sync.Mutex
	objects := map[string][]byte{}
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		if strings.Contains(r.Header.Get("Authorization"), secret) {
			t.Error("raw signing secret exposed")
		}
		switch r.Method {
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			objects[r.URL.Path] = data
			writes++
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if data, ok := objects[r.URL.Path]; ok {
				w.Write(data)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case http.MethodDelete:
			delete(objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	t.Setenv("SCCACHE_ENDPOINT", server.URL)
	temporary := t.TempDir()
	if err := os.WriteFile(filepath.Join(temporary, "rust-args.json"), []byte(`{"clippy":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	workspace := func(content bool) string {
		directory := t.TempDir()
		if data, err := exec.Command("git", "-C", directory, "init", "--quiet").CombinedOutput(); err != nil {
			t.Fatalf("git init: %s: %v", data, err)
		}
		if err := os.WriteFile(filepath.Join(directory, "main.rs"), []byte("fn main() {}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "Cargo.lock"), []byte("version = 4\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if content {
			if err := os.MkdirAll(filepath.Join(directory, "target/debug"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "target/debug/artifact"), []byte("compiled"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return directory
	}
	var output bytes.Buffer
	run := func(directory, action string) {
		t.Helper()
		runner := Runner{Dir: directory, Env: []string{"INFRA_CACHE_TEST_TOOL=1"}, Stdout: &output, Stderr: &output}
		if err := RustCache(context.Background(), runner, temporary, action); err != nil {
			t.Fatal(err)
		}
	}
	first := workspace(true)
	t.Setenv("SCCACHE_S3_RW_MODE", "READ_ONLY")
	run(first, "save")
	if writes != 0 {
		t.Fatal("read-only pool uploaded")
	}
	t.Setenv("SCCACHE_S3_RW_MODE", "READ_WRITE")
	run(first, "save")
	if writes != 2 {
		t.Fatalf("expected archive+fingerprint, got %d writes", writes)
	}
	run(first, "save")
	if writes != 2 {
		t.Fatal("unchanged dependencies uploaded again")
	}
	fresh := workspace(false)
	run(fresh, "restore")
	restored, err := os.ReadFile(filepath.Join(fresh, "target/debug/artifact"))
	if err != nil || string(restored) != "compiled" {
		t.Fatalf("fresh checkout restore = %q, %v", restored, err)
	}
	if err := os.WriteFile(filepath.Join(first, "main.rs"), []byte("fn main() { println!(\"updated\"); }"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "target/debug/artifact"), []byte("updated"), 0600); err != nil {
		t.Fatal(err)
	}
	run(first, "save")
	if writes != 4 {
		t.Fatal("source-only change did not refresh build outputs")
	}
	run(fresh, "restore")
	if restored, err := os.ReadFile(filepath.Join(fresh, "target/debug/artifact")); err != nil || string(restored) != "updated" {
		t.Fatalf("source-only outputs were not restored: %q, %v", restored, err)
	}
	if err := os.WriteFile(filepath.Join(first, "Cargo.lock"), []byte("version = 5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(first, "save")
	if writes != 6 {
		t.Fatal("changed dependencies were not uploaded")
	}
	if err := os.WriteFile(filepath.Join(first, "target/debug/artifact"), make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "Cargo.lock"), []byte("version = 6\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUST_TARGET_CACHE_LIMIT_KIB", "1")
	run(first, "save")
	if len(objects) != 0 {
		t.Fatal("outgrown cache retained stale objects")
	}
	empty := workspace(false)
	run(empty, "restore")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	run(empty, "restore")
	run(empty, "save")
	if _, err := os.Stat(filepath.Join(empty, "target")); !os.IsNotExist(err) {
		t.Fatal("empty cache created target directory")
	}
	if strings.Contains(output.String(), secret) {
		t.Fatal("secret exposed in logs")
	}
}
