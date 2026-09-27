package contracts

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/kustomize"
	"go.yaml.in/yaml/v3"
)

func TestProductionFluxPreservesArtifactOwnershipAndReadiness(t *testing.T) {
	fixture := t.TempDir()
	copyFluxTree(t, filepath.Join(root(t), "platform/clusters/production"), fixture)
	rendered, err := kustomize.Build(fixture)
	if err != nil {
		t.Fatal(err)
	}
	type object struct {
		APIVersion, Kind string
		Metadata         struct{ Name, Namespace string }
		Spec             struct {
			Path      string
			SourceRef struct{ Kind, Name string } `yaml:"sourceRef"`
			DependsOn []struct {
				Name      string
				ReadyExpr string `yaml:"readyExpr"`
			} `yaml:"dependsOn"`
			HealthChecks []struct{ Kind, Name, Namespace string } `yaml:"healthChecks"`
			Template     struct {
				Spec struct{ Containers []struct{ Args []string } }
			}
		}
	}
	owners := map[string]object{}
	controllers := map[string]object{}
	kinds := map[string]bool{}
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var item object
		if err := decoder.Decode(&item); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		kinds[item.Kind+"/"+item.Metadata.Name] = true
		if item.Kind == "Kustomization" {
			owners[item.Metadata.Name] = item
		}
		if item.Kind == "Deployment" {
			controllers[item.Metadata.Name] = item
		}
	}
	if owners["flux-system"].Spec.Path != "./platform/clusters/production" {
		t.Fatal("Flux root does not reconcile the production tree")
	}
	if _, found := owners["platform-projects"]; found {
		t.Fatal("aggregate project owner remains")
	}
	for _, required := range []string{"ArtifactGenerator/platform-artifacts", "CustomResourceDefinition/artifactgenerators.source.extensions.fluxcd.io", "Deployment/source-watcher", "Role/source-watcher-artifacts", "RoleBinding/source-watcher-artifacts"} {
		if !kinds[required] {
			t.Errorf("missing %s", required)
		}
	}
	controller := controllers["kustomize-controller"]
	if len(controller.Spec.Template.Spec.Containers) == 0 || !slices.Contains(controller.Spec.Template.Spec.Containers[0].Args, "--feature-gates=ExternalArtifact=true,AdditiveCELDependencyCheck=true") {
		t.Fatal("artifact or additive policy dependency feature gate missing")
	}
	policy := owners["platform-policy"].Spec.SourceRef
	if policy.Kind != "ExternalArtifact" || !strings.HasPrefix(policy.Name, "platform-policy-") {
		t.Fatalf("policy source is not versioned: %+v", policy)
	}
	for name, owner := range owners {
		for _, dependency := range owner.Spec.DependsOn {
			if dependency.Name == "platform-policy" {
				for _, check := range []string{"dep.spec.sourceRef.kind == 'ExternalArtifact'", "dep.spec.sourceRef.name == '" + policy.Name + "'", "dep.metadata.generation == dep.status.observedGeneration", "c.type == 'Ready' && c.status == 'True'"} {
					if !strings.Contains(dependency.ReadyExpr, check) {
						t.Errorf("%s policy dependency lacks %s", name, check)
					}
				}
			}
		}
	}
	for _, project := range []string{"llunde", "portfolio", "y", "llunde-pyparser"} {
		owner := owners["project-"+project]
		if owner.Spec.Path != "./platform/projects/"+project || owner.Spec.SourceRef.Kind != "ExternalArtifact" || owner.Spec.SourceRef.Name != "project-"+project || len(owner.Spec.DependsOn) != 1 || owner.Spec.DependsOn[0].Name != "platform-policy" {
			t.Errorf("%s project artifact ownership changed: %+v", project, owner.Spec)
		}
	}
	health := owners["project-llunde-pyparser"].Spec.HealthChecks
	if len(health) != 1 || health[0].Kind != "StatefulSet" || health[0].Name != "postgres" || health[0].Namespace != "llunde-pyparser" {
		t.Fatalf("parser database readiness changed: %+v", health)
	}
	for child, parent := range map[string]string{"llunde-pyparser-migration": "project-llunde-pyparser", "llunde-pyparser-application": "llunde-pyparser-migration"} {
		owner := owners[child]
		if owner.Spec.SourceRef.Kind != "ExternalArtifact" || owner.Spec.SourceRef.Name != "project-llunde-pyparser" || len(owner.Spec.DependsOn) != 1 || owner.Spec.DependsOn[0].Name != parent {
			t.Errorf("parser sequencing changed: %s: %+v", child, owner.Spec)
		}
	}
}

func copyFluxTree(t *testing.T, source, destination string) {
	t.Helper()
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	}); err != nil {
		t.Fatal(err)
	}
}
