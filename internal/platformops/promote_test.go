package platformops

import (
	"errors"
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
	for _, fixture := range []struct{ directory, generator, files string }{
		{"controllers", "ci-slots", "ci-slots.sh"},
		{"build-cache", "build-cache-provisioner", "provision.sh"},
		{"backup-job", "backup-hook", "backup.sh heartbeat.sh"},
	} {
		directory := filepath.Join(root, "platform/components", fixture.directory)
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
		manifest := "resources: [keep.yaml]\nconfigMapGenerator:\n- name: " + fixture.generator + "\n  files:\n"
		for _, name := range strings.Fields(fixture.files) {
			manifest += "  - " + name + "\n"
			if err := os.WriteFile(filepath.Join(directory, name), []byte("legacy script"), 0750); err != nil {
				t.Fatal(err)
			}
		}
		manifest += "- name: unrelated\n  literals: [keep=value]\n"
		if err := os.WriteFile(filepath.Join(directory, "kustomization.yaml"), []byte(manifest), 0640); err != nil {
			t.Fatal(err)
		}
	}
	image := "ghcr.io/fredrir/platform-backup-tools@sha256:" + strings.Repeat("b", 64)
	edits, err := ToolsPromotion(root, image)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 13 {
		t.Fatalf("expected thirteen changes, got %d", len(edits))
	}
	for _, edit := range edits {
		text := string(edit.After)
		if strings.Contains(string(edit.Before), "containers:") && (!strings.Contains(text, image) || !strings.Contains(text, "/usr/local/bin/infra") || !strings.Contains(text, "platform") || strings.Contains(text, "name: hook") || !strings.Contains(text, "name: files")) {
			t.Fatalf("invalid promotion: %s", text)
		}
		if strings.HasSuffix(edit.Path, "kustomization.yaml") && (!strings.Contains(text, "name: unrelated") || !strings.Contains(text, "keep.yaml") || strings.Contains(text, ".sh")) {
			t.Fatalf("unrelated generator or resource changed: %s", text)
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

func TestPromotionRollbackRestoresDeletedFilesContentsAndModes(t *testing.T) {
	root := t.TempDir()
	var edits []Edit
	for index, name := range []string{"update.yaml", "delete.sh", "fail.yaml"} {
		path := filepath.Join(root, name)
		mode := os.FileMode(0640)
		if index == 1 {
			mode = 0751
		}
		if err := os.WriteFile(path, []byte(name), mode); err != nil {
			t.Fatal(err)
		}
		edit := Edit{Path: path, Before: []byte(name), After: []byte("promoted")}
		if index == 1 {
			edit.Delete, edit.After = true, nil
		}
		edits = append(edits, edit)
	}
	failure := errors.New("injected write failure")
	err := applyEdits(edits, func(edit Edit, mode os.FileMode) error {
		if strings.HasSuffix(edit.Path, "fail.yaml") {
			return failure
		}
		return applyEdit(edit, mode)
	})
	if !errors.Is(err, failure) {
		t.Fatalf("unexpected failure: %v", err)
	}
	for index, edit := range edits {
		data, err := os.ReadFile(edit.Path)
		if err != nil || string(data) != string(edit.Before) {
			t.Fatalf("rollback lost %s: %s %v", edit.Path, data, err)
		}
		info, err := os.Stat(edit.Path)
		want := os.FileMode(0640)
		if index == 1 {
			want = 0751
		}
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("rollback changed mode for %s", edit.Path)
		}
	}
}

func TestPromotionPreflightsEveryDeletionBeforeChangingFiles(t *testing.T) {
	for _, scenario := range []string{"changed", "symlink", "directory"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			first, last := filepath.Join(root, "first"), filepath.Join(root, "last")
			if err := os.WriteFile(first, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch scenario {
			case "changed":
				err = os.WriteFile(last, []byte("concurrent change"), 0600)
			case "symlink":
				err = os.Symlink(first, last)
			case "directory":
				err = os.Mkdir(last, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := ApplyEdits([]Edit{{Path: first, Before: []byte("original"), Delete: true}, {Path: last, Before: []byte("original"), Delete: true}}); err == nil {
				t.Fatal("invalid deletion accepted")
			}
			data, err := os.ReadFile(first)
			if err != nil || string(data) != "original" {
				t.Fatal("preflight failure removed an earlier file")
			}
		})
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
