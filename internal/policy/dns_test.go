package policy

import (
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

var projectDNS = object{"options": []any{object{"name": "ndots", "value": "2"}}}

func podSpec(resource object) (object, bool) {
	switch resource["kind"] {
	case "Deployment", "StatefulSet", "DaemonSet", "Job":
		spec, ok := at(resource, "spec", "template", "spec").(object)
		return spec, ok
	case "CronJob":
		spec, ok := at(resource, "spec", "jobTemplate", "spec", "template", "spec").(object)
		return spec, ok
	}
	return nil, false
}

func postRendersProjectDNS(release object) bool {
	renderers, _ := at(release, "spec", "postRenderers").([]any)
	for _, renderer := range renderers {
		patches, _ := at(renderer, "kustomize", "patches").([]any)
		for _, patch := range patches {
			text, _ := at(patch, "patch").(string)
			var operations []object
			if yaml.Unmarshal([]byte(text), &operations) != nil {
				continue
			}
			for _, operation := range operations {
				if operation["op"] == "add" && operation["path"] == "/spec/template/spec/dnsConfig" && reflect.DeepEqual(operation["value"], projectDNS) {
					return true
				}
			}
		}
	}
	return false
}

func TestProjectPodsSearchClusterDomainsOnlyForShortNames(t *testing.T) {
	root := repoRoot(t)
	var overlays []string
	err := filepath.WalkDir(filepath.Join(root, "platform/projects"), func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Name() == "kustomization.yaml" {
			relative, _ := filepath.Rel(root, filepath.Dir(path))
			overlays = append(overlays, filepath.ToSlash(relative))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, overlay := range overlays {
		for _, resource := range renderedTree(t, "platform", overlay) {
			name := overlay + ": " + resource["kind"].(string) + " " + at(resource, "metadata", "name").(string)
			if resource["kind"] == "HelmRelease" {
				checked++
				if !postRendersProjectDNS(resource) {
					t.Errorf("%s does not post-render ndots:2 into its pods", name)
				}
				continue
			}
			spec, ok := podSpec(resource)
			if !ok {
				continue
			}
			checked++
			if !reflect.DeepEqual(spec["dnsConfig"], projectDNS) {
				t.Errorf("%s resolves with %v, want ndots:2", name, spec["dnsConfig"])
			}
			if policy, set := spec["dnsPolicy"]; set && policy != "ClusterFirst" {
				t.Errorf("%s uses dnsPolicy %v", name, policy)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no project workloads rendered")
	}
}
