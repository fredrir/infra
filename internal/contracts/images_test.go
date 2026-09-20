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

var binaryInputs = []string{"cmd", "internal", "go.mod", "go.sum", "build", "MODULE.bazel", "MODULE.bazel.lock", ".bazelversion", ".bazelrc", "BUILD.bazel"}

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
		On struct{ Push struct{ Paths []string } }
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, ".github/workflows/images.yml")), &workflow); err != nil {
		t.Fatal(err)
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
				switch input {
				case "cmd", "internal", "build":
					candidate += "/probe"
				default:
					info, err := os.Stat(filepath.Join(repository, input))
					if err != nil {
						t.Fatalf("catalog input %s: %v", input, err)
					}
					if info.IsDir() {
						candidate += "/probe"
					}
				}
				if !triggered(workflow.On.Push.Paths, candidate) {
					t.Errorf("changes to %s do not trigger image workflow", input)
				}
			}
			recipe := strings.ReplaceAll(string(read(t, filepath.Join(repository, image.Dockerfile))), "\\\n", " ")
			for _, line := range strings.Split(recipe, "\n") {
				fields := strings.Fields(line)
				if len(fields) < 3 || (fields[0] != "COPY" && fields[0] != "ADD") || strings.Contains(line, "--from=") {
					continue
				}
				var sources []string
				for _, field := range fields[1 : len(fields)-1] {
					if !strings.HasPrefix(field, "--") {
						sources = append(sources, field)
					}
				}
				for _, source := range sources {
					if strings.HasPrefix(source, "https://") {
						if !strings.Contains(line, "--checksum=sha256:") {
							t.Errorf("remote image input lacks checksum: %s", source)
						}
						continue
					}
					if source == ".infra-artifacts/infra" {
						for _, input := range binaryInputs {
							if !slices.Contains(image.Inputs, input) {
								t.Errorf("injected binary changes to %s do not invalidate tag", input)
							}
						}
						continue
					}
					if !covered(source) {
						t.Errorf("copied input %s does not invalidate tag", source)
					}
				}
			}
		})
	}
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
