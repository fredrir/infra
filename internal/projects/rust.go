package projects

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const runnersPath = "platform/components/runners"
const cachePath = "platform/components/build-cache/projects"
const registryPath = ".github/rust-projects.yaml"

type RustProvider interface {
	RepositoryResolver
	Credentials(context.Context) (map[string]string, error)
	Encrypt(context.Context, []byte, []string) ([]byte, error)
}
type RustOptions struct{ Repository, Project, WorkflowRef, Output, Root string }

func (provider NativeProvider) Credentials(ctx context.Context) (map[string]string, error) {
	fields := map[string]string{"github_app_id": "ARC_GITHUB_APP_ID", "github_app_installation_id": "ARC_GITHUB_APP_INSTALLATION_ID", "github_app_private_key": "ARC_GITHUB_APP_PRIVATE_KEY"}
	data, err := provider.Runner.Output(ctx, "doppler", "secrets", "get", "ARC_GITHUB_APP_ID", "ARC_GITHUB_APP_INSTALLATION_ID", "ARC_GITHUB_APP_PRIVATE_KEY", "--project", "infra", "--config", "ops", "--json")
	if err != nil {
		return nil, err
	}
	var values map[string]struct {
		Computed string `json:"computed"`
	}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("invalid application credential response")
	}
	result := map[string]string{}
	for key, name := range fields {
		if values[name].Computed == "" {
			return nil, fmt.Errorf("missing application credential %s", name)
		}
		result[key] = values[name].Computed
	}
	return result, nil
}
func (provider NativeProvider) Encrypt(ctx context.Context, plaintext []byte, recipients []string) ([]byte, error) {
	directory, err := os.MkdirTemp("", "infra-encrypt-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	input := filepath.Join(directory, "secret.yaml")
	if err := os.WriteFile(input, plaintext, 0600); err != nil {
		return nil, err
	}
	runner := provider.Runner
	runner.Dir = directory
	data, err := runner.Output(ctx, "sops", "encrypt", "--encrypted-regex", "^(data|stringData)$", "--age", strings.Join(recipients, ","), "--input-type", "yaml", "--output-type", "yaml", input)
	if err != nil {
		return nil, fmt.Errorf("secret encryption failed")
	}
	return data, nil
}
func PlatformRecipients(root string) ([]string, error) {
	var expected []string
	pattern := regexp.MustCompile(`recipient: (age1[0-9a-z]+)`)
	err := filepath.WalkDir(filepath.Join(root, "platform"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".secret.sops.yaml") {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("secret recipient source must not be a symlink")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var found []string
		for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
			found = append(found, match[1])
		}
		slices.Sort(found)
		found = slices.Compact(found)
		if len(found) == 0 {
			return fmt.Errorf("platform secret has no age recipients")
		}
		if expected == nil {
			expected = found
		} else if !slices.Equal(expected, found) {
			return fmt.Errorf("platform secrets disagree on their age recipients")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(expected) == 0 {
		return nil, fmt.Errorf("platform has no age recipients")
	}
	return expected, nil
}
func secret(name, namespace string, data map[string]string, labels map[string]string) map[string]any {
	metadata := map[string]any{"name": name, "namespace": namespace}
	if len(labels) > 0 {
		metadata["labels"] = labels
	}
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": metadata, "type": "Opaque", "stringData": data}
}
func RustCallers(identity Identity, reference string) (map[string][]byte, error) {
	if !revisionPattern.MatchString(reference) || validateIdentity(identity, identity.FullName) != nil {
		return nil, fmt.Errorf("invalid caller identity or revision")
	}
	workflow := func(name string) string { return "fredrir/infra/.github/workflows/" + name + ".yml@" + reference }
	documents := map[string]any{"project/.github/workflows/ci.yml": map[string]any{"name": "CI", "on": map[string]any{"push": map[string]any{"branches": []string{"main"}}, "pull_request": map[string]any{}}, "permissions": map[string]string{"contents": "read"}, "concurrency": map[string]any{"group": "ci-${{ github.ref }}", "cancel-in-progress": "${{ github.event_name == 'pull_request' }}"}, "jobs": map[string]any{"rust": map[string]string{"uses": workflow("rust-ci")}}}}
	if !identity.Private {
		documents["project/.github/workflows/auto-tag.yml"] = map[string]any{"name": "Tag release", "on": map[string]any{"push": map[string]any{"branches": []string{"main"}, "paths-ignore": []string{".github/**", "**.md", "cliff.toml"}}, "workflow_dispatch": map[string]any{}}, "permissions": map[string]string{"contents": "read", "id-token": "write"}, "concurrency": map[string]any{"group": "auto-tag", "cancel-in-progress": false}, "jobs": map[string]any{"tag": map[string]string{"uses": workflow("rust-auto-tag")}}}
		documents["project/.github/workflows/release.yml"] = map[string]any{"name": "Release", "on": map[string]any{"push": map[string]any{"tags": []string{"v*"}}, "workflow_dispatch": map[string]any{}}, "permissions": map[string]string{"contents": "write", "id-token": "write", "attestations": "write"}, "concurrency": map[string]any{"group": "release-${{ github.ref }}", "cancel-in-progress": false}, "jobs": map[string]any{"release": map[string]string{"uses": workflow("rust-release")}}}
		documents["project/.github/chainguard/auto-tag.sts.yaml"] = TrustPolicy(identity, "ref:refs/heads/main", "self-hosted", map[string]string{"ref": "^refs/heads/main$", "event_name": "^(push|workflow_dispatch)$", "job_workflow_ref": `^fredrir/infra/\.github/workflows/rust-auto-tag\.yml@[0-9a-f]{40}$`}, map[string]string{"contents": "write"})
		documents[fmt.Sprintf("packages/.github/chainguard/dispatch-%d.sts.yaml", identity.ID)] = TrustPolicy(identity, "environment:release", "self-hosted", map[string]string{"ref": `^refs/tags/v[0-9A-Za-z.+-]+$`, "event_name": "^push$", "environment": "^release$", "job_workflow_ref": `^fredrir/infra/\.github/workflows/rust-release\.yml@[0-9a-f]{40}$`}, map[string]string{"actions": "write"})
	}
	files := map[string][]byte{}
	for path, document := range documents {
		data, err := yaml.Marshal(document)
		if err != nil {
			return nil, err
		}
		files[path] = data
	}
	return files, nil
}
func AppendResource(data []byte, entry string) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid kustomization")
	}
	fields := document.Content[0].Content
	for index := 0; index < len(fields); index += 2 {
		if fields[index].Value != "resources" {
			continue
		}
		list := fields[index+1]
		if list.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("resources must be a list")
		}
		for _, item := range list.Content {
			if item.Value == entry {
				return nil, fmt.Errorf("resource already listed")
			}
		}
		list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: entry})
		return yaml.Marshal(&document)
	}
	return nil, fmt.Errorf("kustomization has no resources list")
}
func OnboardRust(ctx context.Context, provider RustProvider, options RustOptions) error {
	if !repositoryPattern.MatchString(options.Repository) || !rustProjectPattern.MatchString(options.Project) || !revisionPattern.MatchString(options.WorkflowRef) || options.Root == "" || options.Output == "" {
		return fmt.Errorf("invalid Rust onboarding options")
	}
	if err := requireAbsent(options.Output); err != nil {
		return err
	}
	root, err := os.OpenRoot(options.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := root.OpenFile(".infra-onboarding.lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("another onboarding operation is active or its lock requires recovery")
	}
	defer root.Remove(".infra-onboarding.lock")
	if err := lock.Close(); err != nil {
		return err
	}
	overlay := runnersPath + "/" + options.Project
	keys := cachePath + "/" + options.Project + ".secret.sops.yaml"
	for _, path := range []string{overlay, keys} {
		if _, err := root.Lstat(path); !os.IsNotExist(err) {
			return fmt.Errorf("project already onboarded or path inaccessible")
		}
	}
	originals := map[string][]byte{}
	for _, path := range []string{registryPath, runnersPath + "/kustomization.yaml", cachePath + "/kustomization.yaml"} {
		data, err := root.ReadFile(path)
		if err != nil {
			return err
		}
		originals[path] = data
	}
	var registry struct {
		Projects []map[string]any `yaml:"projects"`
	}
	if err := yaml.Unmarshal(originals[registryPath], &registry); err != nil {
		return err
	}
	for _, project := range registry.Projects {
		if project["project"] == options.Project {
			return fmt.Errorf("project already onboarded")
		}
	}
	identity, err := provider.Repository(ctx, options.Repository)
	if err != nil {
		return err
	}
	if err := validateIdentity(identity, options.Repository); err != nil {
		return err
	}
	for _, project := range registry.Projects {
		if fmt.Sprint(project["id"]) == fmt.Sprint(identity.ID) {
			return fmt.Errorf("repository already onboarded")
		}
	}
	recipients, err := PlatformRecipients(options.Root)
	if err != nil {
		return err
	}
	credentials, err := provider.Credentials(ctx)
	if err != nil {
		return err
	}
	for _, field := range []string{"github_app_id", "github_app_installation_id", "github_app_private_key"} {
		if credentials[field] == "" {
			return fmt.Errorf("missing application credential")
		}
	}
	namespace := "ci-" + options.Project
	documents := map[string]any{overlay + "/github-app.secret.sops.yaml": secret("github-app", namespace, credentials, nil)}
	resources := []string{"../ci-namespace", "github-app.secret.sops.yaml"}
	provisioner := map[string]string{}
	for _, pool := range []string{"ro", "rw", "release"} {
		id := make([]byte, 12)
		key := make([]byte, 32)
		if _, err := rand.Read(id); err != nil {
			return err
		}
		if _, err := rand.Read(key); err != nil {
			return err
		}
		access := "GK" + hex.EncodeToString(id)
		secretKey := hex.EncodeToString(key)
		name := "sccache-" + pool
		documents[overlay+"/"+name+".secret.sops.yaml"] = secret(name, namespace, map[string]string{"AWS_ACCESS_KEY_ID": access, "AWS_SECRET_ACCESS_KEY": secretKey}, nil)
		resources = append(resources, name+".secret.sops.yaml")
		provisioner[pool+"_id"] = access
		provisioner[pool+"_secret"] = secretKey
	}
	documents[keys] = secret("build-cache-"+options.Project, "build-cache", provisioner, map[string]string{"infra.fredrir.com/build-cache-project": options.Project})
	files := map[string][]byte{}
	for path, document := range documents {
		plaintext, err := yaml.Marshal(document)
		if err != nil {
			return err
		}
		encrypted, err := provider.Encrypt(ctx, plaintext, recipients)
		if err != nil {
			return err
		}
		if len(encrypted) == 0 {
			return fmt.Errorf("secret encryption returned empty output")
		}
		files[path] = encrypted
	}
	patch, err := yaml.Marshal([]map[string]string{{"op": "replace", "path": "/spec/values/githubConfigUrl", "value": "https://github.com/" + identity.FullName}})
	if err != nil {
		return err
	}
	files[overlay+"/kustomization.yaml"], err = yaml.Marshal(map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "namespace": namespace, "resources": resources, "components": []string{"../rust"}, "patches": []any{map[string]any{"target": map[string]string{"kind": "HelmRelease"}, "patch": string(patch)}}})
	if err != nil {
		return err
	}
	for path, entry := range map[string]string{runnersPath + "/kustomization.yaml": options.Project, cachePath + "/kustomization.yaml": options.Project + ".secret.sops.yaml"} {
		files[path], err = AppendResource(originals[path], entry)
		if err != nil {
			return err
		}
	}
	visibility := "public"
	if identity.Private {
		visibility = "private"
	}
	registry.Projects = append(registry.Projects, map[string]any{"project": options.Project, "repository": identity.FullName, "id": identity.ID, "visibility": visibility})
	files[registryPath], err = yaml.Marshal(registry)
	if err != nil {
		return err
	}
	callers, err := RustCallers(identity, options.WorkflowRef)
	if err != nil {
		return err
	}
	for path, expected := range originals {
		current, err := root.ReadFile(path)
		if err != nil || !bytes.Equal(current, expected) {
			return fmt.Errorf("onboarding inputs changed during preparation")
		}
	}
	if err := writeFreshDirectory(options.Output, callers); err != nil {
		return err
	}
	var changed []string
	rollback := func() {
		for index := len(changed) - 1; index >= 0; index-- {
			path := changed[index]
			if original, ok := originals[path]; ok {
				_ = replaceFile(root, path, original)
			} else {
				_ = root.Remove(path)
			}
		}
		_ = root.Remove(overlay)
		_ = os.RemoveAll(options.Output)
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := root.MkdirAll(filepath.Dir(path), 0755); err != nil {
			rollback()
			return err
		}
		if _, existing := originals[path]; !existing {
			if _, err := root.Lstat(path); !os.IsNotExist(err) {
				rollback()
				return fmt.Errorf("onboarding destination appeared during preparation")
			}
		}
		changed = append(changed, path)
		if err := replaceFile(root, path, files[path]); err != nil {
			rollback()
			return err
		}
	}
	return nil
}

func replaceFile(root *os.Root, path string, data []byte) error {
	identifier := make([]byte, 12)
	if _, err := rand.Read(identifier); err != nil {
		return err
	}
	staging := filepath.Join(filepath.Dir(path), ".onboard-"+hex.EncodeToString(identifier))
	file, err := root.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer root.Remove(staging)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return root.Rename(staging, path)
}
