package platformops

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestToolsPromotionChangesOnlyImagePins(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"platform/components/controllers/ci-slots.yaml", "platform/components/object-store/provisioner.yaml", "platform/components/repository-maintenance/maintenance.yaml", "platform/projects/y/backup.yaml", "platform/projects/llunde-pyparser/backup.yaml", "platform/projects/portfolio/backup.yaml"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		data := "spec:\n  containers:\n  - name: backup\n    image: ghcr.io/fredrir/platform-backup-tools@sha256:" + strings.Repeat("a", 64) + "\n    command: [/usr/local/bin/infra]\n    args: [platform, backup]\n    volumeMounts:\n    - name: data\n      mountPath: /data\n    - name: files\n      mountPath: /files\n  volumes:\n  - name: data\n    emptyDir: {}\n  - name: files\n    emptyDir: {}\n"
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
		var want, got any
		if err := yaml.Unmarshal([]byte(strings.ReplaceAll(string(edit.Before), strings.Repeat("a", 64), strings.Repeat("b", 64))), &want); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(edit.After, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("promotion changed more than the image pin: %s", edit.After)
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

func TestPromotionRollbackRestoresContentsAndModes(t *testing.T) {
	root := t.TempDir()
	var edits []Edit
	for index, name := range []string{"update.yaml", "executable", "fail.yaml"} {
		path := filepath.Join(root, name)
		mode := os.FileMode(0640)
		if index == 1 {
			mode = 0751
		}
		if err := os.WriteFile(path, []byte(name), mode); err != nil {
			t.Fatal(err)
		}
		edit := Edit{Path: path, Before: []byte(name), After: []byte("promoted")}
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

func TestPromotionPreflightsEveryEditBeforeChangingFiles(t *testing.T) {
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
			if err := ApplyEdits([]Edit{{Path: first, Before: []byte("original"), After: []byte("promoted")}, {Path: last, Before: []byte("original"), After: []byte("promoted")}}); err == nil {
				t.Fatal("invalid edit accepted")
			}
			data, err := os.ReadFile(first)
			if err != nil || string(data) != "original" {
				t.Fatal("preflight failure changed an earlier file")
			}
		})
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
