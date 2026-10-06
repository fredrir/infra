package platformops

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/kustomize"
)

func TestToolsPromotionRendersUpdatedWorkloads(t *testing.T) {
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(source, "platform/components/controllers/ci-slots.yaml")); err == nil {
			break
		}
		parent := filepath.Dir(source)
		if parent == source {
			t.Fatal("platform fixtures not found")
		}
		source = parent
	}
	root := t.TempDir()
	err = filepath.WalkDir(filepath.Join(source, "platform"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(root, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0755)
		}
		if strings.HasSuffix(path, "BUILD.bazel") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	image := "ghcr.io/fredrir/platform-backup-tools@sha256:" + strings.Repeat("c", 64)
	edits, err := ToolsPromotion(root, image)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range edits {
		data, err := os.ReadFile(edit.Path)
		if err != nil || string(data) != string(edit.Before) {
			t.Fatalf("dry run changed %s", edit.Path)
		}
	}
	if err := ApplyEdits(edits); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"components/controllers", "components/object-store", "components/repository-maintenance", "projects/y", "projects/llunde-pyparser", "projects/portfolio"} {
		t.Run(directory, func(t *testing.T) {
			data, err := kustomize.Build(filepath.Join(root, "platform", directory))
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if !strings.Contains(text, image) || !strings.Contains(text, "/usr/local/bin/infra") {
				t.Fatal("rendered workload did not select native tools image")
			}
		})
	}
	second, err := ToolsPromotion(root, image)
	if err != nil || len(second) != 0 {
		t.Fatalf("promotion not idempotent: %d %v", len(second), err)
	}
}
