package ci

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestProvenanceRequiresExactSourceAndWorkflow(t *testing.T) {
	workflow, revision := strings.Repeat("d", 40), strings.Repeat("b", 40)
	image := "ghcr.io/fredrir/example@sha256:" + strings.Repeat("a", 64)
	for _, visibility := range []string{"public", "private"} {
		name, args, err := ProvenanceCommand(visibility, "fredrir/example", workflow, revision, image)
		if err != nil {
			t.Fatal(err)
		}
		if visibility == "public" {
			if name != "gh" || !slices.Contains(args, workflow) || !slices.Contains(args, revision) || !slices.Contains(args, "oci://"+image) {
				t.Fatalf("wrong verification command: %s %v", name, args)
			}
		} else if name != "cosign" || !slices.Contains(args, "https://github.com/fredrir/infra/.github/workflows/build-image.yml@"+workflow) || !slices.Contains(args, "source-revision="+revision) || args[len(args)-1] != image {
			t.Fatalf("wrong verification command: %s %v", name, args)
		}
	}
	for _, visibility := range []string{"internal", ""} {
		if _, _, err := ProvenanceCommand(visibility, "fredrir/example", workflow, revision, image); err == nil {
			t.Fatal("unclassified image accepted")
		}
	}
}

func TestUpdateWorkloadPreservesScaleAndOtherWorkloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release.yaml")
	input := "kind: HelmRelease\nspec:\n  values:\n    workloads:\n      web:\n        image: old\n        replicas: 0\n      worker:\n        image: worker\n        replicas: 2\n"
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateWorkload(path, "missing", "image", "revision"); err == nil {
		t.Fatal("accepted missing workload")
	}
	if data, _ := os.ReadFile(path); string(data) != input {
		t.Fatal("failed update changed file")
	}
	if err := UpdateWorkload(path, "web", "image", "revision"); err != nil {
		t.Fatal(err)
	}
	var resource struct {
		Spec struct {
			Values struct {
				Workloads map[string]struct {
					Image    string
					Replicas int
					Revision string `yaml:"sourceRevision"`
				}
			}
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &resource); err != nil {
		t.Fatal(err)
	}
	web := resource.Spec.Values.Workloads["web"]
	worker := resource.Spec.Values.Workloads["worker"]
	if web.Image != "image" || web.Revision != "revision" || web.Replicas != 0 || worker.Image != "worker" || worker.Replicas != 2 {
		t.Fatalf("unexpected workload mutation: %+v", resource)
	}
	before := string(data)
	if err := UpdateWorkload(path, "web", "image", "revision"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != before {
		t.Fatal("repeated update changed output")
	}
}
