package contracts

import (
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestObjectStoreAlertRulesRunUnderPromtoolInTheCheck(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Needs any `yaml:"needs"`
			Steps []struct {
				If  string            `yaml:"if"`
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows/check.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	alerts, ok := workflow.Jobs["alerts"]
	if !ok {
		t.Fatal("check.yml has no alerts job")
	}
	if alerts.Needs != nil {
		t.Errorf("alerts waits for %v instead of running beside the budgeted checks", alerts.Needs)
	}
	var selects, extracts, tests bool
	for _, step := range alerts.Steps {
		switch {
		case strings.Contains(step.Run, "git diff --quiet") && strings.Contains(step.Run, "platform/components/object-store/") && strings.Contains(step.Run, "internal/policy/testdata/object-store-alerts.yaml"):
			selects = true
		case strings.Contains(step.Run, "sed -n 's/^  prometheus: //p' platform/versions.yaml") && strings.Contains(step.Run, `docker cp "$container:/bin/promtool"`):
			extracts = true
		case strings.Contains(step.Run, "-run '^TestObjectStoreAlertsFireOnlyOnTheirConditions$'") && step.Env["INFRA_PROMTOOL_TEST"] == "required":
			tests = true
		}
	}
	if !selects || !extracts || !tests {
		t.Fatalf("alerts job selects=%v extracts promtool from the pinned Prometheus=%v requires the promtool test=%v", selects, extracts, tests)
	}
}
