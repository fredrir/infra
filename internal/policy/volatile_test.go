package policy

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
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
		return strings.Contains(value, "tolerations") && strings.Contains(value, volatileTaint)
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

func TestReleaseTaggingRunsOffVolatileWorkers(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/rust-auto-tag.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			RunsOn string `yaml:"runs-on"`
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	label := workflow.Jobs["tag"].RunsOn
	found := false
	for _, resource := range rendered(t, "platform/components/runners/nsql") {
		if resource["kind"] != "HelmRelease" || at(resource, "spec", "values", "runnerScaleSetName") != label {
			continue
		}
		found = true
		spec := at(resource, "spec", "values", "template", "spec").(object)
		if toleratesVolatile(spec["tolerations"]) || spec["affinity"] != nil {
			t.Errorf("release tagging pool %s can run on volatile workers", label)
		}
		for _, container := range spec["containers"].([]any) {
			env, _ := at(container, "env").([]any)
			for _, variable := range env {
				if from, ok := at(variable, "valueFrom").(object); ok && from["secretKeyRef"] != nil {
					t.Errorf("release tagging pool %s mounts %v", label, at(variable, "name"))
				}
			}
		}
	}
	if !found {
		t.Fatalf("release tagging runs on undeclared pool %q", label)
	}
}

func requiresCriticalNodes(affinity any) bool {
	declared, _ := affinity.(object)
	nodeAffinity, _ := declared["nodeAffinity"].(object)
	required, _ := nodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"].(object)
	terms, _ := required["nodeSelectorTerms"].([]any)
	return len(terms) > 0 && !slices.ContainsFunc(terms, func(term any) bool {
		expressions, _ := term.(object)["matchExpressions"].([]any)
		return !slices.ContainsFunc(expressions, func(expression any) bool {
			return at(expression, "key") == "node-restriction.kubernetes.io/critical" && at(expression, "operator") == "In" && slices.Equal(at(expression, "values").([]any), []any{"true"})
		})
	})
}

func TestClusterControllersRequireCriticalNodes(t *testing.T) {
	checked := 0
	for _, resource := range renderedTree(t, "platform/clusters/production/flux-system", "platform/clusters/production/flux-system") {
		if resource["kind"] == "Deployment" {
			checked++
			if !requiresCriticalNodes(at(resource, "spec", "template", "spec").(object)["affinity"]) {
				t.Errorf("Flux %s may run outside critical nodes", at(resource, "metadata", "name"))
			}
		}
	}
	for _, resource := range renderedTree(t, "platform/components/controllers", "platform/components/controllers") {
		var affinity any
		switch resource["kind"] {
		case "HelmRelease":
			affinity = at(resource, "spec", "values").(object)["affinity"]
		case "Deployment":
			affinity = at(resource, "spec", "template", "spec").(object)["affinity"]
		default:
			continue
		}
		checked++
		if !requiresCriticalNodes(affinity) {
			t.Errorf("%s %s may run outside critical nodes", resource["kind"], at(resource, "metadata", "name"))
		}
	}
	for _, overlay := range at(load(t, "platform/components/runners/kustomization.yaml"), "resources").([]any) {
		for _, resource := range rendered(t, "platform/components/runners/"+overlay.(string)) {
			if resource["kind"] == "HelmRelease" {
				checked++
				if !requiresCriticalNodes(at(resource, "spec", "values", "listenerTemplate", "spec").(object)["affinity"]) {
					t.Errorf("listener of %s may run outside critical nodes", at(resource, "spec", "values", "runnerScaleSetName"))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no controllers checked")
	}
}
