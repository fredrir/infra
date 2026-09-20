package platformops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Edit struct {
	Path   string `json:"path"`
	Before []byte `json:"-"`
	After  []byte `json:"-"`
	Delete bool   `json:"delete,omitempty"`
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
	paths := []struct{ path, command string }{
		{"platform/components/controllers/ci-slots.yaml", "ci-slots"},
		{"platform/components/build-cache/provisioner.yaml", "provision-cache"},
		{"platform/components/repository-maintenance/maintenance.yaml", "repository-maintenance"},
		{"platform/projects/y/backup.yaml", "backup"},
		{"platform/projects/llunde-pyparser/backup.yaml", "backup"},
		{"platform/projects/portfolio/backup.yaml", "backup"},
		{"platform/components/cache/backup.yaml", "backup"},
	}
	var edits []Edit
	for _, item := range paths {
		path := filepath.Join(root, item.path)
		resolved, err := filepath.EvalSymlinks(path)
		if os.IsNotExist(err) && item.path == "platform/components/cache/backup.yaml" {
			continue
		}
		if err != nil {
			return nil, err
		}
		if resolved != path {
			return nil, fmt.Errorf("promotion path must not traverse symlinks: %s", item.path)
		}
		before, err := os.ReadFile(path)
		if os.IsNotExist(err) && item.path == "platform/components/cache/backup.yaml" {
			continue
		}
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
				changed := false
				for _, container := range containers.Content {
					current := mapValue(container, "image")
					if current == nil || !strings.HasPrefix(current.Value, "ghcr.io/fredrir/platform-backup-tools@sha256:") {
						continue
					}
					current.Value = image
					setMapValue(container, "command", sequence("/usr/local/bin/infra"))
					setMapValue(container, "args", sequence("platform", item.command))
					if mounts := mapValue(container, "volumeMounts"); mounts != nil {
						removeNamed(mounts, "script", "scripts", "hook")
					}
					matched++
					changed = true
				}
				if changed {
					if volumes := mapValue(node, "volumes"); volumes != nil {
						removeNamed(volumes, "script", "scripts", "hook")
					}
				}
			})
			documents = append(documents, &document)
		}
		if matched == 0 {
			return nil, fmt.Errorf("no tools container in %s", item.path)
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
	cleanup, err := legacyToolsCleanup(root)
	if err != nil {
		return nil, err
	}
	return append(edits, cleanup...), nil
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
		if seen[path] || (edit.Delete && len(edit.After) != 0) {
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
	if edit.Delete {
		return os.Remove(edit.Path)
	}
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

func legacyToolsCleanup(root string) ([]Edit, error) {
	var edits []Edit
	for _, generator := range []struct {
		directory, name string
		files           []string
	}{
		{"platform/components/controllers", "ci-slots", []string{"ci-slots.sh"}},
		{"platform/components/build-cache", "build-cache-provisioner", []string{"provision.sh"}},
		{"platform/components/backup-job", "backup-hook", []string{"backup.sh", "heartbeat.sh"}},
	} {
		path := filepath.Join(root, generator.directory, "kustomization.yaml")
		before, err := promotionSource(path)
		if err != nil {
			return nil, err
		}
		var document yaml.Node
		if err := yaml.Unmarshal(before, &document); err != nil {
			return nil, err
		}
		if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
			return nil, fmt.Errorf("invalid promotion kustomization: %s", path)
		}
		mapping := document.Content[0]
		removed := false
		if generators := mapValue(mapping, "configMapGenerator"); generators != nil {
			if generators.Kind != yaml.SequenceNode {
				return nil, fmt.Errorf("invalid configMapGenerator in %s", path)
			}
			var kept []*yaml.Node
			for _, entry := range generators.Content {
				name := mapValue(entry, "name")
				if name == nil || name.Value != generator.name {
					kept = append(kept, entry)
					continue
				}
				files := mapValue(entry, "files")
				var actual []string
				if files != nil && files.Kind == yaml.SequenceNode {
					for _, file := range files.Content {
						actual = append(actual, file.Value)
					}
				}
				slices.Sort(actual)
				expected := slices.Clone(generator.files)
				slices.Sort(expected)
				if !slices.Equal(actual, expected) || mapValue(entry, "literals") != nil || mapValue(entry, "envs") != nil {
					return nil, fmt.Errorf("legacy generator %s contains unexpected content", generator.name)
				}
				removed = true
			}
			generators.Content = kept
			if len(kept) == 0 {
				removeMapValue(mapping, "configMapGenerator")
			}
		}
		if removed {
			var after bytes.Buffer
			encoder := yaml.NewEncoder(&after)
			encoder.SetIndent(2)
			if err := encoder.Encode(&document); err != nil {
				return nil, err
			}
			if err := encoder.Close(); err != nil {
				return nil, err
			}
			edits = append(edits, Edit{Path: path, Before: before, After: after.Bytes()})
		}
		for _, name := range generator.files {
			path := filepath.Join(root, generator.directory, name)
			before, err := promotionSource(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			edits = append(edits, Edit{Path: path, Before: before, Delete: true})
		}
	}
	return edits, nil
}

func promotionSource(path string) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if resolved != path {
		return nil, fmt.Errorf("promotion path must not traverse symlinks: %s", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("promotion requires regular file: %s", path)
	}
	return os.ReadFile(path)
}

func removeMapValue(node *yaml.Node, key string) {
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			node.Content = append(node.Content[:index], node.Content[index+2:]...)
			return
		}
	}
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

func setMapValue(node *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func sequence(values ...string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, value := range values {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	return node
}

func removeNamed(node *yaml.Node, names ...string) {
	var kept []*yaml.Node
	for _, child := range node.Content {
		name := mapValue(child, "name")
		remove := false
		for _, candidate := range names {
			if name != nil && name.Value == candidate {
				remove = true
			}
		}
		if !remove {
			kept = append(kept, child)
		}
	}
	node.Content = kept
}
