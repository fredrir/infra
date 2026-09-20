package platformops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolsPromotionChangesPinsAndCommandsTogether(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"platform/components/controllers/ci-slots.yaml", "platform/components/build-cache/provisioner.yaml", "platform/components/repository-maintenance/maintenance.yaml", "platform/projects/y/backup.yaml", "platform/projects/llunde-pyparser/backup.yaml", "platform/projects/portfolio/backup.yaml"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		data := "spec:\n  containers:\n  - name: backup\n    image: ghcr.io/fredrir/platform-backup-tools@sha256:" + strings.Repeat("a", 64) + "\n    args: [/hooks/backup.sh]\n    volumeMounts:\n    - name: hook\n      mountPath: /hooks\n    - name: files\n      mountPath: /files\n  volumes:\n  - name: hook\n    configMap: {name: backup-hook}\n  - name: files\n    emptyDir: {}\n"
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := "ghcr.io/fredrir/platform-backup-tools@sha256:" + strings.Repeat("b", 64)
	edits, err := ToolsPromotion(root, image)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 6 {
		t.Fatalf("expected six changes, got %d", len(edits))
	}
	for _, edit := range edits {
		text := string(edit.After)
		if !strings.Contains(text, image) || !strings.Contains(text, "/usr/local/bin/infra") || !strings.Contains(text, "platform") || strings.Contains(text, "name: hook") || !strings.Contains(text, "name: files") {
			t.Fatalf("invalid promotion: %s", text)
		}
		current, _ := os.ReadFile(edit.Path)
		if string(current) != string(edit.Before) {
			t.Fatal("planning mutated source")
		}
	}
	if err := ApplyEdits(edits); err != nil {
		t.Fatal(err)
	}
	second, err := ToolsPromotion(root, image)
	if err != nil || len(second) != 0 {
		t.Fatalf("promotion not idempotent: %d %v", len(second), err)
	}
}

func TestPromotionRejectsConcurrentEditsBeforeWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyEdits([]Edit{{Path: path, Before: []byte("old"), After: []byte("new")}}); err == nil {
		t.Fatal("concurrent change overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "changed" {
		t.Fatal("file changed")
	}
}

func TestToolsPromotionRejectsSymlinkedSourceDirectories(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "platform", "components"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "platform", "components", "controllers")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outside, "ci-slots.yaml")
	if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ToolsPromotion(root, "ghcr.io/fredrir/platform-backup-tools@sha256:"+strings.Repeat("b", 64))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked source accepted: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "external" {
		t.Fatalf("external file changed: %q %v", data, err)
	}
}
