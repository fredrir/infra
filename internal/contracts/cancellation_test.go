package contracts

import (
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestImageBuildCancellationPreservesVerifiedArtifactReuse(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct{ If string }
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows/build-image.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	program := workflowCondition(t, workflow.Jobs["build"].If)
	for _, test := range []struct {
		name, repository, bootstrap, artifact, image string
		cancelled, protected, released, allowed      bool
	}{
		{"infra bootstrap", "fredrir/infra", "success", "", "", false, true, false, true},
		{"infra reused artifact", "fredrir/infra", "skipped", "verified-artifact", "", false, true, false, true},
		{"infra missing artifact", "fredrir/infra", "skipped", "", "", false, true, false, false},
		{"cancelled bootstrap", "fredrir/infra", "success", "", "", true, true, false, false},
		{"cancelled artifact reuse", "fredrir/infra", "skipped", "verified-artifact", "", true, true, false, false},
		{"consumer released CLI", "fredrir/example", "skipped", "", "", false, true, false, true},
		{"consumer bootstrap", "fredrir/example", "success", "", "", false, true, false, true},
		{"consumer signed artifact", "fredrir/example", "skipped", "caller-artifact", "", false, true, false, true},
		{"failed consumer bootstrap", "fredrir/example", "failure", "", "", false, true, false, false},
		{"unprotected source", "fredrir/infra", "success", "", "", false, false, false, false},
		{"thin runner released CLI", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/infra-runner-deploy", false, true, true, true},
		{"catalog runner released CLI", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/infra-runner-rust", false, true, true, true},
		{"controller released CLI", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/infra-source-watcher", false, true, true, true},
		{"controller foreign caller", "fredrir/example", "skipped", "", "ghcr.io/fredrir/infra-source-watcher", false, true, true, false},
		{"release CLI wrong image", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/other", false, true, true, false},
		{"release CLI foreign caller", "fredrir/example", "skipped", "", "ghcr.io/fredrir/infra-runner-deploy", false, true, true, false},
		{"release CLI artifact conflict", "fredrir/infra", "skipped", "artifact", "ghcr.io/fredrir/infra-runner-deploy", false, true, true, false},
		{"cancelled release CLI", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/infra-runner-deploy", true, true, true, false},
		{"unprotected release CLI", "fredrir/infra", "skipped", "", "ghcr.io/fredrir/infra-runner-deploy", false, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := program.allows(map[string]any{
				"github":          map[string]any{"repository": test.repository, "repository_owner_id": "114402558", "event_name": "push", "ref": "refs/heads/main", "ref_protected": test.protected},
				"inputs":          map[string]any{"cli-artifact": test.artifact, "release-cli": test.released, "image": test.image},
				"needs":           map[string]any{"cli": map[string]any{"result": test.bootstrap}},
				"cancelledStatus": test.cancelled,
			})
			if err != nil {
				t.Fatal(err)
			}
			if allowed != test.allowed {
				t.Fatalf("build allowed = %v, want %v", allowed, test.allowed)
			}
		})
	}
}
