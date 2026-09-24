package fluxartifacts

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestPolicyChangesRequireRegeneratedBarrier(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		path = filepath.Join(root, path)
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("platform/components/policy/policy.yaml", "policy: initial\n")
	write("platform/clusters/production/settings.yaml", "settings: initial\n")
	write("platform/clusters/production/root.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: platform-projects\nspec:\n  dependsOn:\n  - name: platform-policy\n")
	if err := generate(root, false); err != nil {
		t.Fatal(err)
	}
	projects, err := os.ReadFile(filepath.Join(root, directory, "cutover/projects.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(projects))
	parserChecked := false
	for {
		var project struct {
			Metadata struct{ Name string }
			Spec     struct {
				Wait         bool
				HealthChecks []struct {
					APIVersion            string `yaml:"apiVersion"`
					Kind, Name, Namespace string
				} `yaml:"healthChecks"`
			}
		}
		if err := decoder.Decode(&project); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if project.Spec.Wait {
			t.Fatalf("%s waits for unrelated workloads", project.Metadata.Name)
		}
		if project.Metadata.Name != "project-llunde-pyparser" {
			if len(project.Spec.HealthChecks) != 0 {
				t.Fatalf("%s inherited parser health checks", project.Metadata.Name)
			}
			continue
		}
		parserChecked = true
		if len(project.Spec.HealthChecks) != 1 {
			t.Fatalf("parser root must check its database before migration: %+v", project.Spec.HealthChecks)
		}
		check := project.Spec.HealthChecks[0]
		if check.APIVersion != "apps/v1" || check.Kind != "StatefulSet" || check.Namespace != "llunde-pyparser" || check.Name != "postgres" {
			t.Fatalf("parser database health check: %+v", check)
		}
	}
	if !parserChecked {
		t.Fatal("parser owner missing")
	}
	if err := generate(root, true); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, directory, "pause/kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	write("platform/components/policy/policy.yaml", "policy: changed\n")
	if err := generate(root, true); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("policy change accepted stale barrier: %v", err)
	}
	if err := generate(root, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, directory, "pause/kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("policy change retained the old revision barrier")
	}
	if !strings.Contains(string(after), "dep.metadata.generation == dep.status.observedGeneration") || !strings.Contains(string(after), "Ready") {
		t.Fatal("policy barrier lost readiness or generation check")
	}
	write("platform/clusters/production/settings.yaml", "settings: changed\n")
	if err := generate(root, true); err == nil {
		t.Fatal("shared settings accepted stale policy barrier")
	}
	for _, invalid := range []string{"metadata: {}\n", "metadata: {name: platform-projects}\nspec: {dependsOn: [invalid]}\n"} {
		write("platform/clusters/production/root.yaml", invalid)
		if err := Run(root, false); err == nil {
			t.Fatal("malformed production root accepted")
		}
	}
}

func TestArtifactCheckIncludesParserChildrenAndPropagatesRenderFailure(t *testing.T) {
	root := t.TempDir()
	write := func(path, value string) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		"platform/components/policy/kustomization.yaml",
		"platform/components/backup-job/kustomization.yaml",
		"platform/components/repository-maintenance/kustomization.yaml",
		"platform/projects/llunde/kustomization.yaml",
		"platform/projects/portfolio/kustomization.yaml",
		"platform/projects/y/kustomization.yaml",
		"platform/projects/llunde-pyparser/kustomization.yaml",
		"platform/projects/llunde-pyparser/migration/kustomization.yaml",
		"platform/projects/llunde-pyparser/application/kustomization.yaml",
	} {
		write(path, "resources: []\n")
	}
	write("platform/clusters/production/settings.yaml", "settings: fixture\n")
	write("platform/clusters/production/root.yaml", "metadata:\n  name: platform-projects\nspec: {}\n")
	if err := Run(root, false); err != nil {
		t.Fatal(err)
	}
	var rendered []string
	var failure error
	render := func(path string) ([]byte, error) {
		rendered = append(rendered, path)
		if strings.HasSuffix(path, "llunde-pyparser/application") {
			return nil, failure
		}
		return []byte("resources: []\n"), nil
	}
	if err := Check(root, render); err != nil {
		t.Fatal(err)
	}
	if len(rendered) != 14 {
		t.Fatalf("rendered %d paths, want 14: %v", len(rendered), rendered)
	}
	for _, child := range []string{"migration", "application"} {
		count := 0
		for _, path := range rendered {
			if strings.HasSuffix(path, "llunde-pyparser/"+child) {
				count++
			}
		}
		if count != 2 {
			t.Fatalf("%s rendered %d times, want repository and artifact", child, count)
		}
	}
	failure = errors.New("parser application cannot render")
	if err := Check(root, render); !errors.Is(err, failure) {
		t.Fatalf("render failure lost: %v", err)
	}
}
