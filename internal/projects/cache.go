package projects

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

const storePath = "platform/components/object-store"
const cacheCell = "hel1"

func cacheCredentialPrefix(project string) string {
	return "CI_" + strings.ToUpper(strings.ReplaceAll(project, "-", "_")) + "_"
}

func cacheCredentialSecret(project string) string {
	return "seaweedfs-" + cacheCell + "-ci-" + project
}

func AppendCacheIdentities(data []byte, project string) ([]byte, error) {
	var config struct {
		Identities []map[string]any `json:"identities"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("invalid object store identities: %w", err)
	}
	prefix := cacheCredentialPrefix(project)
	main, release := "ci-"+project+"-main", "ci-"+project+"-release"
	for _, pool := range []struct {
		role    string
		actions []string
	}{
		{"ro", []string{"Read:" + main, "List:" + main}},
		{"rw", []string{"Read:" + main, "Write:" + main + "/*", "List:" + main}},
		{"release", []string{"Read:" + release, "Write:" + release + "/*", "List:" + release, "Read:toolchains", "List:toolchains"}},
	} {
		name := "ci-" + project + "-" + pool.role
		for _, identity := range config.Identities {
			if identity["name"] == name {
				return nil, fmt.Errorf("object store identity %s already declared", name)
			}
		}
		variable := prefix + strings.ToUpper(pool.role) + "_"
		config.Identities = append(config.Identities, map[string]any{
			"actions":     pool.actions,
			"credentials": []map[string]string{{"accessKey": "${" + variable + "ACCESS_KEY_ID}", "secretKey": "${" + variable + "SECRET_ACCESS_KEY}"}},
			"name":        name,
		})
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func AppendCacheBuckets(data []byte, project string) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	cells := mappingValue(document.Content, "cells")
	if cells == nil || cells.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("object store spec has no cells")
	}
	for _, cell := range cells.Content {
		if name := mappingValue([]*yaml.Node{cell}, "name"); name == nil || name.Value != cacheCell {
			continue
		}
		buckets := mappingValue([]*yaml.Node{cell}, "buckets")
		if buckets == nil || buckets.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("object store cell %s has no bucket list", cacheCell)
		}
		for _, bucket := range []struct {
			Name       string `yaml:"name"`
			QuotaGiB   int    `yaml:"quotaGiB"`
			ExpireDays int    `yaml:"expireDays"`
		}{{"ci-" + project + "-main", 20, 14}, {"ci-" + project + "-release", 10, 14}} {
			for _, existing := range buckets.Content {
				if name := mappingValue([]*yaml.Node{existing}, "name"); name != nil && name.Value == bucket.Name {
					return nil, fmt.Errorf("bucket %s already declared", bucket.Name)
				}
			}
			var node yaml.Node
			if err := node.Encode(bucket); err != nil {
				return nil, err
			}
			buckets.Content = append(buckets.Content, &node)
		}
		buckets.Style = 0
		return encodeDocuments([]*yaml.Node{&document})
	}
	return nil, fmt.Errorf("object store cell %s missing", cacheCell)
}

func AppendCellCredentials(data []byte, secretName string) ([]byte, error) {
	var documents []*yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	appended := false
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		documents = append(documents, &document)
		kind, name := mappingValue(document.Content, "kind"), mappingValue([]*yaml.Node{mappingValue(document.Content, "metadata")}, "name")
		if kind == nil || kind.Value != "StatefulSet" || name == nil || name.Value != "seaweedfs-"+cacheCell {
			continue
		}
		template := mappingValue([]*yaml.Node{mappingValue(document.Content, "spec")}, "template")
		containers := mappingValue([]*yaml.Node{mappingValue([]*yaml.Node{template}, "spec")}, "containers")
		if containers == nil {
			return nil, fmt.Errorf("object store cell has no containers")
		}
		for _, container := range containers.Content {
			if containerName := mappingValue([]*yaml.Node{container}, "name"); containerName == nil || containerName.Value != "server" {
				continue
			}
			sources := mappingValue([]*yaml.Node{container}, "envFrom")
			if sources == nil || sources.Kind != yaml.SequenceNode {
				return nil, fmt.Errorf("object store server has no envFrom list")
			}
			var reference yaml.Node
			if err := reference.Encode(map[string]map[string]string{"secretRef": {"name": secretName}}); err != nil {
				return nil, err
			}
			for _, source := range sources.Content {
				if existing := mappingValue([]*yaml.Node{mappingValue([]*yaml.Node{source}, "secretRef")}, "name"); existing != nil && existing.Value == secretName {
					return nil, fmt.Errorf("object store credentials %s already referenced", secretName)
				}
			}
			sources.Content = append(sources.Content, &reference)
			sources.Style = 0
			appended = true
		}
	}
	if !appended {
		return nil, fmt.Errorf("object store cell %s server missing", cacheCell)
	}
	return encodeDocuments(documents)
}

func mappingValue(nodes []*yaml.Node, key string) *yaml.Node {
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if node.Kind == yaml.DocumentNode {
			return mappingValue(node.Content, key)
		}
		if node.Kind != yaml.MappingNode {
			continue
		}
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == key {
				return node.Content[index+1]
			}
		}
	}
	return nil
}

func encodeDocuments(documents []*yaml.Node) ([]byte, error) {
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	for _, document := range documents {
		if err := encoder.Encode(document); err != nil {
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
