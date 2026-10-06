package contracts

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/images"
	"go.yaml.in/yaml/v3"
)

func root(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "images/catalog.yaml")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("image contract fixtures unavailable")
		}
		directory = parent
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestImageInputsInvalidateTagsAndTriggerRebuilds(t *testing.T) {
	repository := root(t)
	var catalog []images.Image
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "images/catalog.yaml")), &catalog); err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			Push struct {
				PathsIgnore []string `yaml:"paths-ignore"`
			}
		}
		Jobs map[string]struct{ Uses string }
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, ".github/workflows/reconcile.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.Jobs["images"].Uses != "./.github/workflows/images.yml" {
		t.Fatal("CI does not invoke the image planner")
	}
	seen := map[string]bool{}
	for _, image := range catalog {
		t.Run(image.Image, func(t *testing.T) {
			if seen[image.Image] {
				t.Fatal("duplicate image identity")
			}
			seen[image.Image] = true
			covered := func(path string) bool {
				return slices.ContainsFunc(image.Inputs, func(input string) bool { return path == input || strings.HasPrefix(path, input+"/") })
			}
			if !covered(image.Dockerfile) {
				t.Fatal("Dockerfile does not invalidate the image input tag")
			}
			for _, input := range image.Inputs {
				candidate := input
				info, err := os.Stat(filepath.Join(repository, input))
				if err != nil {
					t.Fatalf("catalog input %s: %v", input, err)
				}
				if info.IsDir() {
					candidate += "/probe"
				}
				if triggered(workflow.On.Push.PathsIgnore, candidate) {
					t.Errorf("changes to %s do not trigger image workflow", input)
				}
			}
			sources := recipeSources(t, read(t, filepath.Join(repository, image.Dockerfile)))
			for _, source := range sources {
				if strings.HasPrefix(source, "https://") || source == ".infra-artifacts/infra" {
					continue
				}
				if !covered(source) {
					t.Errorf("copied input %s does not invalidate tag", source)
				}
			}
			for _, input := range image.Inputs {
				consumed := input == image.Dockerfile || strings.HasPrefix(image.Dockerfile, input+"/") || slices.ContainsFunc(sources, func(source string) bool {
					return source == input || strings.HasPrefix(source, input+"/") || strings.HasPrefix(input, source+"/")
				})
				if !consumed {
					t.Errorf("input %s is not read by the recipe and rebuilds the image without changing it", input)
				}
			}
			if embeds := slices.Contains(sources, ".infra-artifacts/infra"); embeds != image.CLI {
				t.Errorf("recipe embeds the infra binary: %t, catalog declares cli: %t", embeds, image.CLI)
			}
			if image.CLI && triggered(workflow.On.Push.PathsIgnore, "build/cli-release.json") {
				t.Error("CLI release does not trigger image workflow")
			}
		})
	}
}

func TestCatalogImagesBuildWithTheirDeclaredCLI(t *testing.T) {
	repository := root(t)
	var catalog []images.Image
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "images/catalog.yaml")), &catalog); err != nil {
		t.Fatal(err)
	}
	var planner struct {
		Jobs map[string]struct{ With map[string]string }
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, ".github/workflows/images.yml")), &planner); err != nil {
		t.Fatal(err)
	}
	var builder struct {
		Jobs map[string]struct{ If string }
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, ".github/workflows/build-image.yml")), &builder); err != nil {
		t.Fatal(err)
	}
	build := workflowCondition(t, builder.Jobs["build"].If)
	for _, image := range catalog {
		t.Run(image.Image, func(t *testing.T) {
			caller := map[string]any{
				"matrix": map[string]any{"cli": image.CLI, "image": image.Image},
				"inputs": map[string]any{"cli-artifact": "commit-artifact", "cli-sha256": "commit-sha256", "cli-revision": "commit-revision"},
				"needs":  map[string]any{"cli": map[string]any{"outputs": map[string]any{}}},
			}
			inputs := map[string]any{}
			for _, name := range []string{"image", "release-cli", "cli-artifact"} {
				value, err := workflowValue(t, planner.Jobs["image"].With[name]).value(caller)
				if err != nil {
					t.Fatal(err)
				}
				inputs[name] = value
			}
			if inputs["release-cli"] != image.CLI || (inputs["cli-artifact"] == "") != image.CLI {
				t.Fatalf("image embeds the released CLI: %t, but receives %v", image.CLI, inputs)
			}
			allowed, err := build.allows(map[string]any{
				"github":          map[string]any{"repository": "fredrir/infra", "repository_owner_id": "114402558", "event_name": "push", "ref": "refs/heads/main", "ref_protected": true},
				"inputs":          inputs,
				"needs":           map[string]any{"cli": map[string]any{"result": "skipped"}},
				"cancelledStatus": false,
			})
			if err != nil || !allowed {
				t.Fatalf("build-image refuses the catalog image: %v", err)
			}
		})
	}
}

func recipeSources(t *testing.T, recipe []byte) []string {
	t.Helper()
	var sources []string
	for _, line := range strings.Split(strings.ReplaceAll(string(recipe), "\\\n", " "), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "COPY", "ADD":
			if len(fields) < 3 || strings.Contains(line, "--from=") {
				continue
			}
			for _, field := range fields[1 : len(fields)-1] {
				if !strings.HasPrefix(field, "--") {
					sources = append(sources, field)
				}
				if strings.HasPrefix(field, "https://") && !strings.Contains(line, "--checksum=sha256:") {
					t.Errorf("remote image input lacks checksum: %s", field)
				}
			}
		case "RUN":
			for _, field := range fields[1:] {
				mount, found := strings.CutPrefix(field, "--mount=")
				if !found || !strings.Contains(mount, "type=bind") || strings.Contains(mount, "from=") {
					continue
				}
				for _, option := range strings.Split(mount, ",") {
					if source, found := strings.CutPrefix(option, "source="); found {
						sources = append(sources, source)
					}
				}
			}
		}
	}
	return sources
}

func triggered(patterns []string, candidate string) bool {
	included := false
	for _, pattern := range patterns {
		negative := strings.HasPrefix(pattern, "!")
		expression := regexp.QuoteMeta(strings.TrimPrefix(pattern, "!"))
		expression = strings.ReplaceAll(expression, `\*\*`, ".*")
		expression = strings.ReplaceAll(expression, `\*`, "[^/]*")
		if regexp.MustCompile("^" + expression + "$").MatchString(candidate) {
			included = !negative
		}
	}
	return included
}

func TestHostedDaggerEnginesUseDisposableStorageVolumes(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(root(t), ".github/workflows/*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		var workflow struct {
			Jobs map[string]struct {
				Steps []struct{ Run string }
			}
		}
		if err := yaml.Unmarshal(read(t, path), &workflow); err != nil {
			t.Fatal(err)
		}
		for job, configuration := range workflow.Jobs {
			for _, step := range configuration.Steps {
				for _, line := range strings.Split(step.Run, "\n") {
					if strings.Contains(line, "docker run") && strings.Contains(line, "--name infra-dagger ") && !strings.Contains(line, "--volume /var/lib/dagger ") {
						t.Errorf("%s job %s starts Dagger without its snapshot storage volume", filepath.Base(path), job)
					}
					if strings.Contains(line, "docker rm") && strings.Contains(line, "infra-dagger") && !strings.Contains(line, "--volumes") {
						t.Errorf("%s job %s leaves the ephemeral engine volume behind", filepath.Base(path), job)
					}
				}
			}
		}
	}
}
