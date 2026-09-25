package policy

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const volatileTaint = "node-restriction.kubernetes.io/volatile"

func toleratesVolatile(tolerations any) bool {
	items, _ := tolerations.([]any)
	return slices.ContainsFunc(items, func(item any) bool {
		toleration, _ := item.(object)
		if !slices.Contains([]any{nil, "", "NoSchedule"}, toleration["effect"]) {
			return false
		}
		switch toleration["key"] {
		case volatileTaint:
			return toleration["operator"] == "Exists" || (toleration["operator"] == "Equal" && toleration["value"] == "true")
		case nil, "":
			return toleration["operator"] == "Exists"
		}
		return false
	})
}

func declaresVolatileToleration(value any) bool {
	switch value := value.(type) {
	case object:
		for key, inner := range value {
			if (key == "tolerations" && toleratesVolatile(inner)) || declaresVolatileToleration(inner) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(value, declaresVolatileToleration)
	case string:
		return strings.Contains(value, volatileTaint)
	}
	return false
}

func TestOnlyNodeMetricsAndUntrustedCIPoolsTolerateVolatileWorkers(t *testing.T) {
	root := repoRoot(t)
	allowed := []string{
		"platform/components/observability/monitoring.yaml",
		"platform/components/runners/infra/check-values.yaml",
		"platform/components/runners/rust/volatile/kustomization.yaml",
	}
	tolerating := map[string]bool{}
	scanned := 0
	for _, tree := range []string{"platform/components", "platform/projects"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(path) != ".yaml" || strings.HasSuffix(path, ".sops.yaml") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			relative, _ := filepath.Rel(root, path)
			for _, document := range yamlObjects(t, data) {
				if declaresVolatileToleration(document) {
					tolerating[filepath.ToSlash(relative)] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned == 0 {
		t.Fatal("no platform manifests scanned")
	}
	if found := slices.Sorted(maps.Keys(tolerating)); !slices.Equal(found, allowed) {
		t.Errorf("workloads tolerating volatile workers %q, want %q", found, allowed)
	}
	alloy := at(load(t, "platform/components/observability/alloy.yaml"), "spec", "values").(object)
	for _, rule := range slices.Concat(at(alloy, "rbac", "rules").([]any), at(alloy, "rbac", "clusterRules").([]any)) {
		for _, resource := range at(rule, "resources").([]any) {
			if !slices.Contains([]any{"pods", "pods/log", "namespaces"}, resource) {
				t.Errorf("log collectors may read %v", resource)
			}
		}
	}
	monitoring := at(load(t, "platform/components/observability/monitoring.yaml"), "spec", "values").(object)
	if at(monitoring, "nodeExporter", "enabled") != true || !toleratesVolatile(at(monitoring, "prometheus-node-exporter", "tolerations")) {
		t.Error("node metrics do not cover volatile workers")
	}
}
