package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCIPreemptsOnlyBackfill(t *testing.T) {
	data, e := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/policy/priorities.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	classes := map[string]object{}
	for _, class := range yamlObjects(t, data) {
		name := at(class, "metadata", "name").(string)
		if class["globalDefault"] != false {
			t.Errorf("%s replaces the default priority", name)
		}
		classes[name] = class
	}
	ladder := []string{"backfill", "ci", "batch", "production", "platform-critical"}
	if len(classes) != len(ladder) {
		t.Fatalf("priority classes %d, want %d", len(classes), len(ladder))
	}
	value := func(name string) int { return at(classes[name], "value").(int) }
	preemption := func(name string) string { return at(classes[name], "preemptionPolicy").(string) }
	for i := 1; i < len(ladder); i++ {
		if value(ladder[i-1]) >= value(ladder[i]) {
			t.Errorf("%s does not rank below %s", ladder[i-1], ladder[i])
		}
	}
	if !(value("ci") < 0 && value("batch") > 0) {
		t.Error("unclassified pods must outrank CI and yield to batch")
	}
	if preemption("ci") != "PreemptLowerPriority" {
		t.Error("CI cannot reclaim capacity from backfill")
	}
	for name := range classes {
		if value(name) < value("ci") && preemption(name) != "Never" {
			t.Errorf("%s preempts although it ranks below CI", name)
		}
	}
}
