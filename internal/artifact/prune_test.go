package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneUsesRecencyAndPreservesProtectedRevision(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for i, prefix := range []string{"a", "b", "c"} {
		directory := filepath.Join(root, strings.Repeat(prefix, 40), "linux-amd64", strings.Repeat(prefix, 64))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "infra")
		if err := os.WriteFile(path, make([]byte, 10), 0o555); err != nil {
			t.Fatal(err)
		}
		when := now.Add(time.Duration(i-3) * 24 * time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Prune(PruneOptions{CacheDir: root, KeepRevision: strings.Repeat("a", 40), MaxBytes: 20, MaxAge: 7 * 24 * time.Hour, Now: now})
	if err != nil || result.Removed != 1 || result.FreedBytes != 10 || result.RetainedBytes != 20 {
		t.Fatalf("prune: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, strings.Repeat("a", 40), "linux-amd64", strings.Repeat("a", 64), "infra")); err != nil {
		t.Fatal("protected revision removed")
	}
	if _, err := os.Stat(filepath.Join(root, strings.Repeat("b", 40), "linux-amd64", strings.Repeat("b", 64), "infra")); !os.IsNotExist(err) {
		t.Fatal("oldest eligible entry retained")
	}
}

func TestPruneLeavesUnrelatedFilesAndRejectsInvalidLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "user-data"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(PruneOptions{CacheDir: root, MaxBytes: 1, MaxAge: time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "user-data")); err != nil {
		t.Fatal("unrelated file removed")
	}
	if _, err := Prune(PruneOptions{CacheDir: root}); err == nil {
		t.Fatal("invalid limits accepted")
	}
}
