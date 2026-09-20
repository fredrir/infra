package artifact

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstallRepairsExecutableModeOnCacheHit(t *testing.T) {
	data := linuxFixture()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	options := InstallOptions{URL: server.URL, Client: server.Client(), Revision: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Platform: "linux/amd64", CacheDir: filepath.Join(t.TempDir(), "cache")}
	first, err := Install(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(first.Path, 0600); err != nil {
		t.Fatal(err)
	}
	options.URL = ""
	second, err := Install(context.Background(), options)
	if err != nil || !second.CacheHit {
		t.Fatalf("cache reuse: %+v %v", second, err)
	}
	info, err := os.Stat(second.Path)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatalf("cached executable not repaired: %v %v", info, err)
	}
}

func TestPruneWaitsForInstallAndObservesRefreshedUsage(t *testing.T) {
	data := linuxFixture()
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			_, _ = w.Write(data)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	options := InstallOptions{URL: server.URL, Client: server.Client(), Revision: strings.Repeat("a", 40), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Platform: "linux/amd64", CacheDir: filepath.Join(t.TempDir(), "cache")}
	if err := privateCache(options.CacheDir, options.Revision, "linux-amd64", options.SHA256); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(options.CacheDir, options.Revision, "linux-amd64", options.SHA256, "infra")
	if err := os.WriteFile(path, []byte("old corrupt cache"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	installed := make(chan error, 1)
	go func() { _, err := Install(context.Background(), options); installed <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	pruned := make(chan error, 1)
	go func() {
		result, err := Prune(PruneOptions{CacheDir: options.CacheDir, MaxBytes: 1 << 20, MaxAge: time.Hour})
		if err == nil && result.Removed != 0 {
			err = fmt.Errorf("fresh artifact pruned: %+v", result)
		}
		pruned <- err
	}()
	select {
	case err := <-pruned:
		close(release)
		t.Fatalf("prune did not wait for installer: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-installed; err != nil {
		t.Fatal(err)
	}
	if err := <-pruned; err != nil {
		t.Fatal(err)
	}
	if err := verify(path, options.SHA256, options.Platform); err != nil {
		t.Fatalf("fresh artifact disappeared: %v", err)
	}
}

func TestInstallLockWaitHonorsCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	held, err := lockCache(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = Install(ctx, InstallOptions{CacheDir: root, Revision: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Platform: "linux/amd64"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock cancellation lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, strings.Repeat("a", 40))); !os.IsNotExist(err) {
		t.Fatal("waiting installer changed cache")
	}
}

func TestCacheLockRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "shared", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "cache")
			if err := privateCache(root); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".lock")
			outside := filepath.Join(t.TempDir(), "data")
			if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, path)
			case "hardlink":
				err = os.Link(outside, path)
			case "shared":
				err = os.WriteFile(path, nil, 0644)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			lock, err := lockCache(context.Background(), root)
			if err == nil {
				lock.Close()
				t.Fatal("unsafe lock accepted")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "unchanged" {
				t.Fatal("outside file changed")
			}
		})
	}
}
