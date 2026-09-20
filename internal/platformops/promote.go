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
	return edits, nil
}

func ApplyEdits(edits []Edit) error {
	for _, edit := range edits {
		current, err := os.ReadFile(edit.Path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, edit.Before) {
			return fmt.Errorf("file changed during promotion: %s", edit.Path)
		}
	}
	var written []Edit
	for _, edit := range edits {
		if err := atomicFile(edit.Path, edit.After); err != nil {
			for i := len(written) - 1; i >= 0; i-- {
				err = errors.Join(err, atomicFile(written[i].Path, written[i].Before))
			}
			return err
		}
		written = append(written, edit)
	}
	return nil
}

func atomicFile(path string, data []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("promotion requires regular file")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".infra-promote-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Chmod(info.Mode().Perm()), f.Sync(), f.Close())
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
