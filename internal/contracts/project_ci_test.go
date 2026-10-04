package contracts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"go.yaml.in/yaml/v3"
)

func TestProjectProfilesMatchDeploymentIdentitiesAndRecipes(t *testing.T) {
	root := root(t)
	profiles := t.TempDir()
	if err := os.MkdirAll(filepath.Join(profiles, "build/projects"), 0755); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "build/projects/*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("project profiles unavailable: %v", err)
	}
	for _, path := range paths {
		if err := os.WriteFile(filepath.Join(profiles, "build/projects", filepath.Base(path)), read(t, path), 0644); err != nil {
			t.Fatal(err)
		}
		profile, err := ci.ReadProject(profiles, "fredrir/"+strings.TrimSuffix(filepath.Base(path), ".json"), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(profile.Images) > 0 {
			var mapping struct {
				Repository string
				Images     map[string]any
			}
			if err := yaml.Unmarshal(read(t, filepath.Join(root, ".github/deployments", profile.RepositoryID+".yaml")), &mapping); err != nil {
				t.Fatal(err)
			}
			if mapping.Repository != profile.Repository {
				t.Fatalf("deployment identity mismatch: %s", profile.Repository)
			}
			for _, image := range profile.Images {
				if _, ok := mapping.Images[image.Image]; !ok {
					t.Fatalf("unmapped project image: %s", image.Image)
				}
			}
		}
		for _, check := range profile.Checks {
			data, err := os.ReadFile(filepath.Join(root, check.Recipe))
			if err != nil || !strings.Contains(string(data), " AS "+check.Target+"\n") {
				t.Fatalf("project recipe target missing: %s/%s", profile.Repository, check.Name)
			}
		}
	}
}
