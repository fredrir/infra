package fluxartifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type object = map[string]any

const directory = "build/rollout/flux-artifacts"

func Run(root string, check bool) error {
	if !check {
		return generate(root, false)
	}
	return Check(root, func(path string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data, err := exec.CommandContext(ctx, "kubectl", "kustomize", path).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("render %s: %w: %s", path, err, data)
		}
		return data, nil
	})
}

func Check(root string, render func(string) ([]byte, error)) error {
	if err := generate(root, true); err != nil {
		return err
	}
	return validateArtifacts(root, render)
}

func generate(root string, check bool) error {
	var paths []string
	if err := filepath.WalkDir(filepath.Join(root, "platform/components/policy"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("policy symlink: %s", path)
		}
		if !entry.IsDir() {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths = append(paths, relative)
		}
		return nil
	}); err != nil {
		return err
	}
	paths = append(paths, "platform/clusters/production/settings.yaml", "platform/clusters/production/root.yaml")
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(hash, "%x  %s\n", sum, path)
	}
	policyName := "platform-policy-" + hex.EncodeToString(hash.Sum(nil))[:40]
	barrier := fmt.Sprintf("dep.spec.sourceRef.kind == 'ExternalArtifact' && dep.spec.sourceRef.name == '%s' && dep.metadata.generation == dep.status.observedGeneration && dep.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')", policyName)
	data, err := os.ReadFile(filepath.Join(root, "platform/clusters/production/root.yaml"))
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var roots []object
	for {
		var root object
		err := decoder.Decode(&root)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		roots = append(roots, root)
	}
	var artifacts []any
	artifact := func(name, path string, backup bool) object {
		copies := []any{object{"from": "@repo/" + path + "/**", "to": "@artifact/" + path + "/"}, object{"from": "@repo/platform/clusters/production/settings.yaml", "to": "@artifact/platform/clusters/production/settings.yaml"}}
		if backup {
			for _, component := range []string{"backup-job", "repository-maintenance"} {
				path := "platform/components/" + component
				copies = append(copies, object{"from": "@repo/" + path + "/**", "to": "@artifact/" + path + "/"})
			}
		}
		return object{"name": name, "originRevision": "@repo", "copy": copies}
	}
	artifacts = append(artifacts, artifact(policyName, "platform/components/policy", false))
	for _, project := range []string{"llunde", "portfolio", "y", "llunde-pyparser"} {
		artifacts = append(artifacts, artifact("project-"+project, "platform/projects/"+project, project != "llunde"))
	}
	sources := object{"apiVersion": "source.extensions.fluxcd.io/v1beta1", "kind": "ArtifactGenerator", "metadata": object{"name": "platform-artifacts", "namespace": "flux-system"}, "spec": object{"sources": []any{object{"alias": "repo", "kind": "GitRepository", "name": "flux-system"}}, "artifacts": artifacts}}
	rootPatch := func(stage string) object {
		return patch("flux-system", []object{{"op": "replace", "path": "/spec/path", "value": "./" + directory + "/" + stage}})
	}
	bootstrap := overlay([]string{"../../../../platform/clusters/production", "../controller", "sources.yaml"}, []object{rootPatch("bootstrap"), {"target": object{"kind": "Deployment", "name": "kustomize-controller", "namespace": "flux-system"}, "patch": "- op: add\n  path: /spec/template/spec/containers/0/args/-\n  value: --feature-gates=ExternalArtifact=true,AdditiveCELDependencyCheck=true\n"}})
	pauses := []object{rootPatch("pause"), patch("platform-projects", []object{{"op": "add", "path": "/spec/suspend", "value": true}, {"op": "replace", "path": "/spec/prune", "value": false}, {"op": "add", "path": "/spec/deletionPolicy", "value": "Orphan"}}), patch("platform-policy", []object{{"op": "replace", "path": "/spec/sourceRef", "value": object{"kind": "ExternalArtifact", "name": policyName}}})}
	for _, root := range roots {
		metadata, metadataOK := root["metadata"].(map[string]any)
		spec, specOK := root["spec"].(map[string]any)
		name, nameOK := metadata["name"].(string)
		if !metadataOK || !specOK || !nameOK || name == "" {
			return fmt.Errorf("production root requires named resources with a spec")
		}
		if dependencies, ok := spec["dependsOn"].([]any); ok {
			for index, dependency := range dependencies {
				dependency, ok := dependency.(map[string]any)
				if !ok {
					return fmt.Errorf("production root %s has invalid dependency", name)
				}
				if dependency["name"] == "platform-policy" {
					pauses = append(pauses, patch(name, []object{{"op": "add", "path": fmt.Sprintf("/spec/dependsOn/%d/readyExpr", index), "value": barrier}}))
				}
			}
		}
	}
	var projectRoots []object
	for _, project := range []string{"llunde", "portfolio", "y", "llunde-pyparser"} {
		projectRoots = append(projectRoots, object{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": object{"name": "project-" + project, "namespace": "flux-system"}, "spec": object{"interval": "10m", "retryInterval": "1m", "timeout": "15m", "sourceRef": object{"kind": "ExternalArtifact", "name": "project-" + project}, "path": "./platform/projects/" + project, "prune": true, "wait": false, "serviceAccountName": "platform-reconciler", "postBuild": object{"substituteFrom": []any{object{"kind": "ConfigMap", "name": "platform-settings"}}}, "decryption": object{"provider": "sops", "secretRef": object{"name": "sops-age"}}, "dependsOn": []any{object{"name": "platform-policy", "readyExpr": barrier}}}})
	}
	cutovers := []object{rootPatch("cutover")}
	for _, name := range []string{"llunde-pyparser-migration", "llunde-pyparser-application"} {
		operations := []object{{"op": "replace", "path": "/spec/sourceRef", "value": object{"kind": "ExternalArtifact", "name": "project-llunde-pyparser"}}}
		if name == "llunde-pyparser-migration" {
			operations = append(operations, object{"op": "replace", "path": "/spec/dependsOn", "value": []any{object{"name": "project-llunde-pyparser"}}})
		}
		cutovers = append(cutovers, patch(name, operations))
	}
	files := map[string][]object{"bootstrap/sources.yaml": {sources}, "bootstrap/kustomization.yaml": {bootstrap}, "pause/kustomization.yaml": {overlay([]string{"../bootstrap"}, pauses)}, "cutover/kustomization.yaml": {overlay([]string{"../pause", "projects.yaml"}, cutovers)}, "cutover/projects.yaml": projectRoots}
	for path, documents := range files {
		var output bytes.Buffer
		encoder := yaml.NewEncoder(&output)
		encoder.SetIndent(2)
		for _, document := range documents {
			if err := encoder.Encode(document); err != nil {
				return err
			}
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		path = filepath.Join(directory, path)
		if check {
			current, err := os.ReadFile(filepath.Join(root, path))
			if err != nil {
				return err
			}
			if !bytes.Equal(current, output.Bytes()) {
				return fmt.Errorf("stale %s; run go -C %s/generate run .", path, directory)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(root, path), output.Bytes(), 0644); err != nil {
				return err
			}
		}
	}
	return nil
}

func patch(name string, operations []object) object {
	data, err := yaml.Marshal(operations)
	if err != nil {
		panic(err)
	}
	return object{"target": object{"group": "kustomize.toolkit.fluxcd.io", "kind": "Kustomization", "name": name, "namespace": "flux-system"}, "patch": string(data)}
}

func overlay(resources []string, patches []object) object {
	return object{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources, "patches": patches}
}

func validateArtifacts(repository string, render func(string) ([]byte, error)) error {
	data, err := os.ReadFile(filepath.Join(repository, directory, "bootstrap/sources.yaml"))
	if err != nil {
		return err
	}
	var config struct {
		Spec struct {
			Artifacts []struct {
				Name string
				Copy []struct{ From, To string }
			}
		}
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp("", "flux-artifact-validation-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	for _, artifact := range config.Spec.Artifacts {
		root := filepath.Join(temporary, artifact.Name)
		for _, copy := range artifact.Copy {
			source := strings.TrimSuffix(strings.TrimPrefix(copy.From, "@repo/"), "/**")
			destination := filepath.Join(root, strings.TrimPrefix(copy.To, "@artifact/"))
			info, err := os.Stat(filepath.Join(repository, source))
			if err != nil {
				return err
			}
			if info.IsDir() {
				if err := os.CopyFS(destination, os.DirFS(filepath.Join(repository, source))); err != nil {
					return err
				}
			} else {
				data, err := os.ReadFile(filepath.Join(repository, source))
				if err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
					return err
				}
				if err := os.WriteFile(destination, data, 0600); err != nil {
					return err
				}
			}
		}
		source := strings.TrimSuffix(strings.TrimPrefix(artifact.Copy[0].From, "@repo/"), "/**")
		paths := []string{source}
		if artifact.Name == "project-llunde-pyparser" {
			paths = append(paths, filepath.Join(source, "migration"), filepath.Join(source, "application"))
		}
		for _, path := range paths {
			expected, err := render(filepath.Join(repository, path))
			if err != nil {
				return err
			}
			actual, err := render(filepath.Join(root, path))
			if err != nil {
				return err
			}
			if !bytes.Equal(expected, actual) {
				return fmt.Errorf("artifact %s changes rendered resources at %s", artifact.Name, path)
			}
		}
	}
	return nil
}
