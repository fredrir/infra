package policy

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

func promtool(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("promtool")
	if err != nil {
		if os.Getenv("INFRA_PROMTOOL_TEST") == "required" {
			t.Fatal("promtool is required on PATH")
		}
		t.Skip("promtool is not on PATH; the check workflow's alerts job runs this test")
	}
	return path
}

func TestEveryObjectStoreAlertHasPromtoolCases(t *testing.T) {
	var cases struct {
		Tests []struct {
			AlertRuleTest []struct {
				Alertname string `yaml:"alertname"`
			} `yaml:"alert_rule_test"`
		} `yaml:"tests"`
	}
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "internal/policy/testdata/object-store-alerts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	var tested []string
	for _, test := range cases.Tests {
		for _, check := range test.AlertRuleTest {
			tested = append(tested, check.Alertname)
		}
	}
	for _, resource := range objectStoreResources(t) {
		if resource["kind"] != "PrometheusRule" {
			continue
		}
		for _, group := range at(resource, "spec", "groups").([]any) {
			for _, rule := range group.(object)["rules"].([]any) {
				if name, ok := rule.(object)["alert"].(string); ok && !slices.Contains(tested, name) {
					t.Errorf("alert %s has no promtool case in testdata/object-store-alerts.yaml", name)
				}
			}
		}
	}
}

func TestObjectStoreAlertsFireOnlyOnTheirConditions(t *testing.T) {
	binary := promtool(t)
	var groups any
	for _, resource := range objectStoreResources(t) {
		if resource["kind"] == "PrometheusRule" {
			groups = at(resource, "spec", "groups")
		}
	}
	if groups == nil {
		t.Fatal("object store renders no PrometheusRule")
	}
	rules, err := yaml.Marshal(object{"groups": groups})
	if err != nil {
		t.Fatal(err)
	}
	cases, err := os.ReadFile(filepath.Join(repoRoot(t), "internal/policy/testdata/object-store-alerts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for name, data := range map[string][]byte{"object-store-rules.yaml": rules, "object-store-alerts.yaml": cases} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"check", "rules", "object-store-rules.yaml"}, {"test", "rules", "object-store-alerts.yaml"}} {
		command := exec.Command(binary, args...)
		command.Dir = directory
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("promtool %v: %v\n%s", args, err, output)
		}
	}
}
