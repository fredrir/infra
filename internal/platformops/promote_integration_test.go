package platformops

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/kustomize"
	"go.yaml.in/yaml/v3"
)

func TestToolsPromotionRendersCopiedPlatformWithoutLegacyScripts(t *testing.T) {
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
	expectedDeletes := 0
	for _, path := range []string{"components/controllers/ci-slots.sh", "components/build-cache/provision.sh", "components/backup-job/backup.sh", "components/backup-job/heartbeat.sh"} {
		if _, err := os.Stat(filepath.Join(root, "platform", path)); err == nil {
			expectedDeletes++
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	edits, err := ToolsPromotion(root, image)
	if err != nil {
		t.Fatal(err)
	}
	deletes := 0
	for _, edit := range edits {
		data, err := os.ReadFile(edit.Path)
		if err != nil || string(data) != string(edit.Before) {
			t.Fatalf("dry run changed %s", edit.Path)
		}
		if edit.Delete {
			deletes++
		}
	}
	if deletes != expectedDeletes {
		t.Fatalf("expected %d legacy script deletions, got %d", expectedDeletes, deletes)
	}
	if err := ApplyEdits(edits); err != nil {
		t.Fatal(err)
	}
	for _, edit := range edits {
		if edit.Delete {
			if _, err := os.Lstat(edit.Path); !os.IsNotExist(err) {
				t.Fatalf("legacy script retained: %s", edit.Path)
			}
		}
	}
	for _, directory := range []string{"components/controllers", "components/build-cache", "projects/y", "projects/llunde-pyparser", "projects/portfolio", "components/cache"} {
		t.Run(directory, func(t *testing.T) {
			data, err := kustomize.Build(filepath.Join(root, "platform", directory))
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if !strings.Contains(text, image) || !strings.Contains(text, "/usr/local/bin/infra") {
				t.Fatal("rendered workload did not select native tools image")
			}
			for _, legacy := range []string{"ci-slots.sh", "provision.sh", "backup.sh", "heartbeat.sh"} {
				if strings.Contains(text, legacy) {
					t.Fatalf("render retained %s", legacy)
				}
			}
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			for {
				var resource struct {
					Kind     string
					Metadata struct{ Name string }
				}
				if err := decoder.Decode(&resource); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if resource.Kind == "ConfigMap" {
					for _, legacy := range []string{"ci-slots-", "build-cache-provisioner-", "backup-hook-"} {
						if strings.HasPrefix(resource.Metadata.Name, legacy) {
							t.Fatalf("render retained generated legacy ConfigMap %s", resource.Metadata.Name)
						}
					}
				}
			}
		})
	}
	second, err := ToolsPromotion(root, image)
	if err != nil || len(second) != 0 {
		t.Fatalf("promotion not idempotent: %d %v", len(second), err)
	}
}
