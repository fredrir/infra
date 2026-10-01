package contracts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"go.yaml.in/yaml/v3"
)

func TestProjectProfilesMatchDeploymentAndCandidateIdentities(t *testing.T) {
	root := root(t)
	profiles := t.TempDir()
	if err := os.MkdirAll(filepath.Join(profiles, "build/projects"), 0755); err != nil {
		t.Fatal(err)
	}
	var candidates struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []struct{ Repository, ID, Suites string }
				}
			}
		}
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root, ".github/workflows/ci-candidate.yml")), &candidates); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates.Jobs["fixtures"].Strategy.Matrix.Include {
		name := candidate.Repository + ".json"
		if err := os.WriteFile(filepath.Join(profiles, "build/projects", name), read(t, filepath.Join(root, "build/projects", name)), 0644); err != nil {
			t.Fatal(err)
		}
		profile, err := ci.ReadProject(profiles, "fredrir/"+candidate.Repository, candidate.ID)
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
		for _, name := range strings.Fields(candidate.Suites) {
			found := false
			for _, check := range profile.Checks {
				if check.Name != name {
					continue
				}
				found = true
				data, err := os.ReadFile(filepath.Join(root, check.Recipe))
				if err != nil || !strings.Contains(string(data), " AS "+check.Target+"\n") {
					t.Fatalf("candidate recipe target missing: %s/%s", profile.Repository, name)
				}
			}
			if !found {
				t.Fatalf("candidate suite missing: %s/%s", profile.Repository, name)
			}
		}
	}
}
