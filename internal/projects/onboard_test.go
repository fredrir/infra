package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type fixtureProvider struct {
	identity       Identity
	calls          int
	encryptions    []map[string]any
	failEncryption int
}

func identityFixture() Identity {
	result := Identity{ID: 123, FullName: "fredrir/example"}
	result.Owner.ID = OwnerID
	return result
}
func (provider *fixtureProvider) Repository(context.Context, string) (Identity, error) {
	provider.calls++
	return provider.identity, nil
}
func (provider *fixtureProvider) Credentials(context.Context) (map[string]string, error) {
	return map[string]string{"github_app_id": "1", "github_app_installation_id": "2", "github_app_private_key": "private-key"}, nil
}
func (provider *fixtureProvider) Encrypt(_ context.Context, plaintext []byte, _ []string) ([]byte, error) {
	document := map[string]any{}
	if err := yaml.Unmarshal(plaintext, &document); err != nil {
		return nil, err
	}
	provider.encryptions = append(provider.encryptions, document)
	if provider.failEncryption == len(provider.encryptions) {
		return nil, errors.New("encryption failed")
	}
	return []byte("encrypted-document"), nil
}
func optionsFixture(output string) OnboardOptions {
	return OnboardOptions{Repository: "fredrir/example", Project: "example", Image: "ghcr.io/fredrir/example@sha256:" + strings.Repeat("a", 64), SourceRevision: strings.Repeat("b", 40), WorkflowRef: strings.Repeat("c", 40), Domain: "example.fredrir.com", HealthPath: "/healthz", Architecture: "amd64", TestCommand: "npm test", Port: 8080, Output: output}
}
func readDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}
func nested(value map[string]any, keys ...string) any {
	var current any = value
	for _, key := range keys {
		current = current.(map[string]any)[key]
	}
	return current
}
func TestProjectOnboardingEmitsZeroReplicaWorkloadAndExactHostedTrust(t *testing.T) {
	output := filepath.Join(t.TempDir(), "output")
	provider := &fixtureProvider{identity: identityFixture()}
	options := optionsFixture(output)
	if err := Onboard(context.Background(), provider, options); err != nil {
		t.Fatal(err)
	}
	release := readDocument(t, filepath.Join(output, "project/release.yaml"))
	if nested(release, "spec", "values", "workloads", "web", "replicas") != 0 || nested(release, "spec", "values", "workloads", "web", "image") != options.Image {
		t.Fatal("generated workload lost its initial scale or immutable image")
	}
	caller := readDocument(t, filepath.Join(output, "caller/.github/workflows/build.yaml"))
	if nested(caller, "jobs", "build", "uses") != imageWorkflow+"@"+options.WorkflowRef {
		t.Fatal("caller is not pinned")
	}
	policy := readDocument(t, filepath.Join(output, "infrastructure/.github/chainguard/deploy-123.sts.yaml"))
	claims := policy["claim_pattern"].(map[string]any)
	for field, wanted := range map[string]string{"runner_environment": "github-hosted", "repository_id": "123", "repository_owner_id": "114402558", "job_workflow_sha": options.WorkflowRef, "job_workflow_ref": imageWorkflow + "@" + options.WorkflowRef} {
		pattern := regexp.MustCompile(claims[field].(string))
		if !pattern.MatchString(wanted) || pattern.MatchString(wanted+"x") {
			t.Fatalf("weak %s claim: %s", field, pattern)
		}
	}
	if _, err := os.Stat(filepath.Join(output, "runner")); !os.IsNotExist(err) {
		t.Fatal("generic onboarding emitted obsolete runner overlay")
	}
}
func TestInvalidOnboardingIsRejectedBeforeNetworkOrWrites(t *testing.T) {
	for _, scenario := range []string{"existing", "mutable", "arm", "escape", "port", "domain"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			options := optionsFixture(filepath.Join(directory, "output"))
			switch scenario {
			case "existing":
				options.Output = directory
			case "mutable":
				options.Image = "ghcr.io/fredrir/example:latest"
			case "arm":
				options.Architecture = "arm64"
			case "escape":
				options.Project = "../escape"
			case "port":
				options.Port = 80
			case "domain":
				options.Domain = "evil.example"
			}
			provider := &fixtureProvider{identity: identityFixture()}
			if err := Onboard(context.Background(), provider, options); err == nil {
				t.Fatal("unsafe input accepted")
			}
			if provider.calls != 0 {
				t.Fatal("invalid input made a remote request")
			}
		})
	}
}
func rustFixture(t *testing.T) (RustOptions, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	files := map[string][]byte{registryPath: []byte("projects: []\n"), runnersPath + "/kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"), cachePath + "/kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"), "platform/existing.secret.sops.yaml": []byte("sops:\n  age:\n    - recipient: age1example\n")}
	for name, data := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return RustOptions{Repository: "fredrir/example", Project: "example", WorkflowRef: strings.Repeat("a", 40), Root: root, Output: filepath.Join(t.TempDir(), "output")}, files
}
func TestRustOnboardingSeparatesPoolCredentialsAndRegistersResources(t *testing.T) {
	options, _ := rustFixture(t)
	provider := &fixtureProvider{identity: identityFixture()}
	if err := OnboardRust(context.Background(), provider, options); err != nil {
		t.Fatal(err)
	}
	if len(provider.encryptions) != 5 {
		t.Fatalf("encrypted documents: %d", len(provider.encryptions))
	}
	var provisioner map[string]any
	pools := map[string]map[string]any{}
	for _, document := range provider.encryptions {
		name := nested(document, "metadata", "name").(string)
		data := document["stringData"].(map[string]any)
		if name == "build-cache-example" {
			provisioner = data
		} else if strings.HasPrefix(name, "sccache-") {
			pools[strings.TrimPrefix(name, "sccache-")] = data
			if nested(document, "metadata", "namespace") != "ci-example" {
				t.Fatal("wrong runner credential namespace")
			}
		}
	}
	ids := map[string]bool{}
	for _, pool := range []string{"ro", "rw", "release"} {
		data := pools[pool]
		id := data["AWS_ACCESS_KEY_ID"].(string)
		key := data["AWS_SECRET_ACCESS_KEY"].(string)
		if !regexp.MustCompile(`^GK[0-9a-f]{24}$`).MatchString(id) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) || ids[id] {
			t.Fatal("invalid or shared cache identity")
		}
		ids[id] = true
		if provisioner[pool+"_id"] != id || provisioner[pool+"_secret"] != key {
			t.Fatal("provisioner and runner credentials differ")
		}
	}
	overlay := readDocument(t, filepath.Join(options.Root, runnersPath, "example/kustomization.yaml"))
	if overlay["namespace"] != "ci-example" || !reflect.DeepEqual(overlay["components"], []any{"../rust"}) {
		t.Fatal("wrong Rust runner overlay")
	}
	registry := readDocument(t, filepath.Join(options.Root, registryPath))
	project := registry["projects"].([]any)[0].(map[string]any)
	if project["repository"] != "fredrir/example" || project["visibility"] != "public" {
		t.Fatal("wrong project registration")
	}
	if err := OnboardRust(context.Background(), provider, options); err == nil {
		t.Fatal("duplicate onboarding accepted")
	}
}
func TestEncryptionFailureLeavesRepositoryAndOutputUntouched(t *testing.T) {
	options, originals := rustFixture(t)
	provider := &fixtureProvider{identity: identityFixture(), failEncryption: 3}
	if err := OnboardRust(context.Background(), provider, options); err == nil {
		t.Fatal("encryption failure ignored")
	}
	for name, expected := range originals {
		data, err := os.ReadFile(filepath.Join(options.Root, name))
		if err != nil || string(data) != string(expected) {
			t.Fatalf("input mutated: %s", name)
		}
	}
	for _, path := range []string{options.Output, filepath.Join(options.Root, runnersPath, "example"), filepath.Join(options.Root, cachePath, "example.secret.sops.yaml")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("partial output exists: %s", path)
		}
	}
}
func TestPlatformRecipientDisagreementFailsBeforeCredentialsAreGenerated(t *testing.T) {
	options, _ := rustFixture(t)
	if err := os.WriteFile(filepath.Join(options.Root, "platform/other.secret.sops.yaml"), []byte("recipient: age1different\n"), 0644); err != nil {
		t.Fatal(err)
	}
	provider := &fixtureProvider{identity: identityFixture()}
	if err := OnboardRust(context.Background(), provider, options); err == nil || len(provider.encryptions) != 0 {
		t.Fatal("inconsistent recipients accepted")
	}
}
func TestPrivateRustProjectsEmitOnlyReadOnlyCI(t *testing.T) {
	identity := identityFixture()
	identity.Private = true
	files, err := RustCallers(identity, strings.Repeat("f", 40))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files["project/.github/workflows/ci.yml"]) == 0 {
		t.Fatal("private project gained publication callers")
	}
}
func TestAppendResourcePreservesOtherSettingsAndRejectsDuplicates(t *testing.T) {
	data, err := AppendResource([]byte("resources: []\nnamespace: ci-example\npatches:\n- target:\n    kind: Secret\n"), "example")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document["namespace"] != "ci-example" || len(document["patches"].([]any)) != 1 {
		t.Fatal("unrelated kustomization settings changed")
	}
	if _, err := AppendResource(data, "example"); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestRustOnboardingRejectsDuplicateRepositoryAndSymlinkEscape(t *testing.T) {
	for _, scenario := range []string{"repository", "escape", "locked"} {
		t.Run(scenario, func(t *testing.T) {
			options, _ := rustFixture(t)
			provider := &fixtureProvider{identity: identityFixture()}
			switch scenario {
			case "repository":
				if err := os.WriteFile(filepath.Join(options.Root, registryPath), []byte("projects:\n- project: other\n  id: 123\n"), 0644); err != nil {
					t.Fatal(err)
				}
			case "escape":
				if err := os.Remove(filepath.Join(options.Root, registryPath)); err != nil {
					t.Fatal(err)
				}
				outside := filepath.Join(t.TempDir(), "registry")
				if err := os.WriteFile(outside, []byte("projects: []\n"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(options.Root, registryPath)); err != nil {
					t.Fatal(err)
				}
			case "locked":
				if err := os.WriteFile(filepath.Join(options.Root, ".infra-onboarding.lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := OnboardRust(context.Background(), provider, options); err == nil {
				t.Fatal("unsafe onboarding accepted")
			}
			if len(provider.encryptions) != 0 {
				t.Fatal("unsafe onboarding generated secrets")
			}
		})
	}
}
func TestRustCallerTrustRejectsWrongIdentityEnvironmentAndWorkflow(t *testing.T) {
	files, err := RustCallers(identityFixture(), strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err := yaml.Unmarshal(files["project/.github/chainguard/auto-tag.sts.yaml"], &policy); err != nil {
		t.Fatal(err)
	}
	claims := policy["claim_pattern"].(map[string]any)
	for field, values := range map[string][]string{"repository_id": {"123", "1234"}, "repository_owner_id": {"114402558", "1"}, "runner_environment": {"self-hosted", "github-hosted"}, "job_workflow_ref": {"fredrir/infra/.github/workflows/rust-auto-tag.yml@" + strings.Repeat("a", 40), "evil/infra/.github/workflows/rust-auto-tag.yml@" + strings.Repeat("a", 40)}} {
		pattern := regexp.MustCompile(claims[field].(string))
		if !pattern.MatchString(values[0]) || pattern.MatchString(values[1]) {
			t.Fatalf("unsafe %s trust: %s", field, pattern)
		}
	}
}
