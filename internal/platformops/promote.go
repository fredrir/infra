package platformops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Edit struct {
	Path   string `json:"path"`
	Before []byte `json:"-"`
	After  []byte `json:"-"`
}

func ToolsPromotion(root, image string) ([]Edit, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`^ghcr\.io/fredrir/platform-backup-tools@sha256:[a-f0-9]{64}$`).MatchString(image) {
		return nil, fmt.Errorf("digest-pinned backup tools image required")
	}
	paths := []string{
		"platform/components/controllers/ci-slots.yaml",
		"platform/components/object-store/provisioner.yaml",
		"platform/components/repository-maintenance/maintenance.yaml",
		"platform/projects/y/backup.yaml",
		"platform/projects/llunde-pyparser/backup.yaml",
		"platform/projects/portfolio/backup.yaml",
	}
	var edits []Edit
	for _, item := range paths {
		path := filepath.Join(root, item)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		if resolved != path {
			return nil, fmt.Errorf("promotion path must not traverse symlinks: %s", item)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		decoder := yaml.NewDecoder(bytes.NewReader(before))
		var documents []*yaml.Node
		matched := 0
		for {
			var document yaml.Node
			if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, err
			}
			walkYAML(&document, func(node *yaml.Node) {
				containers := mapValue(node, "containers")
				if containers == nil {
					return
				}
				for _, container := range containers.Content {
					current := mapValue(container, "image")
					if current == nil || !strings.HasPrefix(current.Value, "ghcr.io/fredrir/platform-backup-tools@sha256:") {
						continue
					}
					current.Value = image
					matched++
				}
			})
			documents = append(documents, &document)
		}
		if matched == 0 {
			return nil, fmt.Errorf("no tools container in %s", item)
		}
		var after bytes.Buffer
		encoder := yaml.NewEncoder(&after)
		encoder.SetIndent(2)
		for _, document := range documents {
			if err := encoder.Encode(document); err != nil {
				return nil, err
			}
		}
		if err := encoder.Close(); err != nil {
			return nil, err
		}
		if !bytes.Equal(before, after.Bytes()) {
			edits = append(edits, Edit{Path: path, Before: before, After: after.Bytes()})
		}
	}
	return edits, nil
}

func ApplyEdits(edits []Edit) error {
	return applyEdits(edits, applyEdit)
}

func applyEdits(edits []Edit, apply func(Edit, os.FileMode) error) error {
	modes := make([]os.FileMode, len(edits))
	seen := map[string]bool{}
	for index, edit := range edits {
		path, err := filepath.Abs(edit.Path)
		if err != nil {
			return err
		}
		if seen[path] {
			return fmt.Errorf("invalid or duplicate promotion edit: %s", edit.Path)
		}
		seen[path] = true
		info, err := os.Lstat(edit.Path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("promotion requires regular files: %s", edit.Path)
		}
		modes[index] = info.Mode().Perm()
		current, err := os.ReadFile(edit.Path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, edit.Before) {
			return fmt.Errorf("file changed during promotion: %s", edit.Path)
		}
	}
	for index, edit := range edits {
		if err := apply(edit, modes[index]); err != nil {
			for i := index - 1; i >= 0; i-- {
				err = errors.Join(err, atomicFile(edits[i].Path, edits[i].Before, modes[i]))
			}
			return err
		}
	}
	return nil
}

func applyEdit(edit Edit, mode os.FileMode) error {
	return atomicFile(edit.Path, edit.After, mode)
}

func atomicFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".infra-promote-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Chmod(mode), f.Sync(), f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func walkYAML(node *yaml.Node, visit func(*yaml.Node)) {
	if node.Kind == yaml.MappingNode {
		visit(node)
	}
	for _, child := range node.Content {
		walkYAML(child, visit)
	}
}

func mapValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
