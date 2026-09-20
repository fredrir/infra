package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyChangesRequireRegeneratedBarrier(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("platform/components/policy/policy.yaml", "policy: initial\n")
	write("platform/clusters/production/settings.yaml", "settings: initial\n")
	write("platform/clusters/production/root.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: platform-projects\nspec:\n  dependsOn:\n  - name: platform-policy\n")
	if err := generate(false); err != nil {
		t.Fatal(err)
	}
	if err := generate(true); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(directory, "pause/kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	write("platform/components/policy/policy.yaml", "policy: changed\n")
	if err := generate(true); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("policy change accepted stale barrier: %v", err)
	}
	if err := generate(false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(directory, "pause/kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("policy change retained the old revision barrier")
	}
	if !strings.Contains(string(after), "dep.metadata.generation == dep.status.observedGeneration") || !strings.Contains(string(after), "Ready") {
		t.Fatal("policy barrier lost readiness or generation check")
	}
	write("platform/clusters/production/settings.yaml", "settings: changed\n")
	if err := generate(true); err == nil {
		t.Fatal("shared settings accepted stale policy barrier")
	}
}
