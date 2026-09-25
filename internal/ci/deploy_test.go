package ci

import (
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

func TestPinWorkloadPreservesScaleAndOtherWorkloads(t *testing.T) {
	input := []byte("kind: HelmRelease\nspec:\n  values:\n    workloads:\n      web:\n        image: old\n        replicas: 0\n      worker:\n        image: worker\n        replicas: 2\n")
	if _, err := pinWorkload(input, "missing", "image", "revision"); err == nil {
		t.Fatal("accepted missing workload")
	}
	data, err := pinWorkload(input, "web", "image", "revision")
	if err != nil {
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
	if err := yaml.Unmarshal(data, &resource); err != nil {
		t.Fatal(err)
	}
	web := resource.Spec.Values.Workloads["web"]
	worker := resource.Spec.Values.Workloads["worker"]
	if web.Image != "image" || web.Revision != "revision" || web.Replicas != 0 || worker.Image != "worker" || worker.Replicas != 2 {
		t.Fatalf("unexpected workload mutation: %+v", resource)
	}
	if repeated, err := pinWorkload(data, "web", "image", "revision"); err != nil || string(repeated) != string(data) {
		t.Fatalf("repeated update changed output: %v", err)
	}
}

func TestPinImageUpdatesOnlyTheNamedPin(t *testing.T) {
	image, digest := "ghcr.io/fredrir/example", "sha256:"+strings.Repeat("a", 64)
	for _, test := range []struct {
		name, input, want string
	}{
		{name: "pinned", input: "resources:\n- application.yaml\nimages:\n- name: ghcr.io/fredrir/other\n  digest: sha256:old\n- name: ghcr.io/fredrir/example\n  newTag: latest\n", want: "resources:\n    - application.yaml\nimages:\n    - name: ghcr.io/fredrir/other\n      digest: sha256:old\n    - name: ghcr.io/fredrir/example\n      newName: ghcr.io/fredrir/example\n      digest: " + digest + "\n"},
		{name: "other image", input: "images:\n- name: ghcr.io/fredrir/other\n"},
		{name: "no images", input: "resources:\n- application.yaml\n"},
		{name: "empty", input: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			pinned, err := pinImage([]byte(test.input), image, digest)
			if err != nil || string(pinned) != test.want {
				t.Fatalf("pinned %q, %v; want %q", pinned, err, test.want)
			}
		})
	}
	if _, err := pinImage([]byte("images: ghcr.io/fredrir/example\n"), image, digest); err == nil {
		t.Fatal("accepted image pins that are not a list")
	}
}
