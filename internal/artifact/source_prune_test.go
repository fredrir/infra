package artifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSourcePruneKeepsRecentEntriesAndUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, digit := range []string{"a", "b", "c"} {
		dir := filepath.Join(root, strings.Repeat(digit, 64))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"infra", "sha256", "inputs", "source-revision", "last-used", "notes"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		used := now.Add(-time.Duration(i) * time.Hour)
		if err := os.Chtimes(filepath.Join(dir, "last-used"), used, used); err != nil {
			t.Fatal(err)
		}
	}
	result, err := PruneSource(context.Background(), SourcePruneOptions{CacheDir: root, Keep: 1, MaxAge: 24 * time.Hour, Now: now})
	if err != nil || result.Removed != 2 {
		t.Fatalf("result %+v, error %v", result, err)
	}
	for _, digit := range []string{"a", "b", "c"} {
		dir := filepath.Join(root, strings.Repeat(digit, 64))
		if _, err := os.Stat(filepath.Join(dir, "notes")); err != nil {
			t.Fatal("unrelated file removed")
		}
		_, err := os.Stat(filepath.Join(dir, "infra"))
		if (digit == "a" && err != nil) || (digit != "a" && !os.IsNotExist(err)) {
			t.Fatal("incorrect source cache retention")
		}
	}
}

func TestSourcePruneDoesNotRaceAnActiveCacheReader(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := PruneSource(ctx, SourcePruneOptions{CacheDir: root, Keep: 16, MaxAge: 7 * 24 * time.Hour}); err == nil {
		t.Fatal("collection did not wait for active reader")
	}
}

func TestSourcePruneRejectsLinkedArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, strings.Repeat("a", 64))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(target, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "infra")); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneSource(context.Background(), SourcePruneOptions{CacheDir: root, Keep: 1, MaxAge: time.Nanosecond}); err == nil {
		t.Fatal("linked cache artifact accepted")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "preserved" {
		t.Fatal("linked target changed")
	}
}
