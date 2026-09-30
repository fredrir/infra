package contracts

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/deployment"
	"go.yaml.in/yaml/v3"
)

func TestBuildImageTrustPoliciesNameExactRevisions(t *testing.T) {
	policies, err := filepath.Glob(filepath.Join(root(t), ".github/chainguard/*.sts.yaml"))
	if err != nil || len(policies) == 0 {
		t.Fatalf("trust policies unavailable: %v", err)
	}
	for _, path := range policies {
		var policy struct {
			ClaimPattern struct {
				WorkflowRef string `yaml:"job_workflow_ref"`
				WorkflowSHA string `yaml:"job_workflow_sha"`
			} `yaml:"claim_pattern"`
		}
		if err := yaml.Unmarshal(read(t, path), &policy); err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		if !strings.Contains(policy.ClaimPattern.WorkflowRef, "build-image") {
			continue
		}
		if _, err := deployment.WorkflowRevisions(policy.ClaimPattern.WorkflowSHA); err != nil {
			t.Errorf("%s: %v", filepath.Base(path), err)
		}
	}
}
