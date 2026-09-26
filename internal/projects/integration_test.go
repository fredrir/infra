package projects

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"go.yaml.in/yaml/v3"
)

type encryptedFixtureProvider struct {
	fixtureProvider
	native NativeProvider
}

func (provider *encryptedFixtureProvider) Encrypt(ctx context.Context, data []byte, recipients []string) ([]byte, error) {
	return provider.native.Encrypt(ctx, data, recipients)
}
func TestNativeOnboardingQualification(t *testing.T) {
	if os.Getenv("INFRA_ONBOARD_INTEGRATION") != "1" {
		t.Skip("set INFRA_ONBOARD_INTEGRATION=1 to qualify native SOPS, Helm and Kustomize")
	}
	ctx := context.Background()
	runner := ci.Runner{Stdout: io.Discard, Stderr: io.Discard}
	directory := t.TempDir()
	key := filepath.Join(directory, "age.key")
	if err := runner.Run(ctx, "age-keygen", "-o", key); err != nil {
		t.Fatal(err)
	}
	recipient, err := runner.Output(ctx, "age-keygen", "-y", key)
	if err != nil {
		t.Fatal(err)
	}
	options, _ := rustFixture(t)
	if err := os.WriteFile(filepath.Join(options.Root, "platform/existing.secret.sops.yaml"), []byte("sops:\n  age:\n    - recipient: "+strings.TrimSpace(string(recipient))+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	provider := &encryptedFixtureProvider{fixtureProvider: fixtureProvider{identity: identityFixture()}, native: NativeProvider{Runner: runner}}
	if err := OnboardRust(ctx, provider, options); err != nil {
		t.Fatal(err)
	}
	runner.Env = []string{"SOPS_AGE_KEY_FILE=" + key}
	for _, name := range []string{"github-app", "sccache-ro", "sccache-rw", "sccache-release"} {
		path := filepath.Join(options.Root, runnersPath, "example", name+".secret.sops.yaml")
		encrypted, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encrypted), "private-key") {
			t.Fatal("secret retained plaintext")
		}
		data, err := runner.Output(ctx, "sops", "decrypt", path)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := yaml.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		if nested(document, "metadata", "namespace") != "ci-example" {
			t.Fatal("encrypted runner namespace differs")
		}
	}
	projectOptions := optionsFixture(filepath.Join(directory, "project"))
	if err := Onboard(ctx, &fixtureProvider{identity: identityFixture()}, projectOptions); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(ctx, "kustomize", "build", filepath.Join(projectOptions.Output, "project")); err != nil {
		t.Fatal(err)
	}
	release := readDocument(t, filepath.Join(projectOptions.Output, "project/release.yaml"))
	values, err := json.Marshal(nested(release, "spec", "values"))
	if err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(directory, "values.json")
	if err := os.WriteFile(valuesPath, values, 0600); err != nil {
		t.Fatal(err)
	}
	source := os.Getenv("INFRA_TEST_SOURCE_ROOT")
	if source == "" {
		source = filepath.Join("..", "..")
	}
	rendered, err := runner.Output(ctx, "helm", "template", "example", filepath.Join(source, "charts/project"), "--namespace", "project-example", "-f", valuesPath)
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(rendered)))
	foundDeployment, foundIngress := false, false
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch document["kind"] {
		case "Deployment":
			foundDeployment = true
			if nested(document, "spec", "replicas") != 0 {
				t.Fatal("workload started before secrets provisioned")
			}
			container := nested(document, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
			if nested(container, "securityContext", "allowPrivilegeEscalation") != false || nested(container, "lifecycle", "preStop", "sleep", "seconds") != 5 || container["image"] != projectOptions.Image {
				t.Fatal("workload security or shutdown contract changed")
			}
			if options := nested(document, "spec", "template", "spec", "dnsConfig", "options"); !reflect.DeepEqual(options, []any{map[string]any{"name": "ndots", "value": "2"}}) {
				t.Fatalf("workload resolves with DNS options %v, want ndots:2", options)
			}
		case "Ingress":
			foundIngress = nested(document, "spec", "ingressClassName") == "platform"
		}
	}
	if !foundDeployment || !foundIngress {
		t.Fatal("generated chart omitted workload or ingress")
	}
}
