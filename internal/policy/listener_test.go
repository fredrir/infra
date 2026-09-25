package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const valuesAnnotation = "infra.fredrir.com/values"

func valuesHash(t *testing.T, resources []object, name string) string {
	t.Helper()
	for _, resource := range resources {
		if resource["kind"] == "HelmRelease" && at(resource, "metadata", "name") == name {
			annotations, _ := at(resource, "spec", "values", "template", "metadata", "annotations").(object)
			hash, _ := annotations[valuesAnnotation].(string)
			return hash
		}
	}
	t.Fatalf("HelmRelease %s not rendered", name)
	return ""
}

func TestWarmRunnerSetsReplaceIdleRunnersWhenTheirValuesChange(t *testing.T) {
	warm := 0
	for _, overlay := range at(load(t, "platform/components/runners/kustomization.yaml"), "resources").([]any) {
		directory := "platform/components/runners/" + overlay.(string)
		kustomization := load(t, directory+"/kustomization.yaml")
		sources, releases := map[string]string{}, map[string]string{}
		generators, _ := kustomization["configMapGenerator"].([]any)
		for _, generator := range generators {
			files := at(generator, "files").([]any)
			sources[at(generator, "name").(string)] = directory + "/" + files[0].(string)
		}
		resources, _ := kustomization["resources"].([]any)
		for _, file := range resources {
			path := directory + "/" + file.(string)
			data, err := os.ReadFile(filepath.Join(repoRoot(t), path))
			if err != nil {
				continue
			}
			for _, document := range yamlObjects(t, data) {
				if document["kind"] == "HelmRelease" {
					releases[at(document, "metadata", "name").(string)] = path
				}
			}
		}
		for _, release := range rendered(t, directory) {
			if release["kind"] != "HelmRelease" || at(release, "spec", "chart", "spec", "chart") != "gha-runner-scale-set" {
				continue
			}
			values := at(release, "spec", "values").(object)
			if minRunners, _ := values["minRunners"].(int); minRunners == 0 {
				continue
			}
			warm++
			name := at(release, "metadata", "name").(string)
			t.Run(name, func(t *testing.T) {
				metadata, _ := at(values, "template").(object)["metadata"].(object)
				annotations, _ := metadata["annotations"].(object)
				hash, _ := annotations[valuesAnnotation].(string)
				generator := hash[:max(strings.LastIndex(hash, "-"), 0)]
				source, ok := sources[generator]
				if !ok {
					t.Fatalf("idle runners of %s outlive listener changes because its runner template has no %s hash of its values", name, valuesAnnotation)
				}
				declared := load(t, source)
				set(values, generator, "template", "metadata", "annotations", valuesAnnotation)
				live, _ := json.Marshal(values)
				want, _ := json.Marshal(at(declared, "spec", "values"))
				if !bytes.Equal(live, want) {
					t.Fatalf("%s renders values beyond %s, whose hash replaces its idle runners", name, source)
				}
				for field, value := range map[string]any{
					"minRunners":       2,
					"maxRunners":       3,
					"listenerTemplate": object{"spec": object{"containers": []any{object{"name": "listener"}}}},
				} {
					changed := clone(declared).(object)
					set(changed, value, "spec", "values", field)
					data, err := yaml.Marshal(changed)
					if err != nil {
						t.Fatal(err)
					}
					rehashed := valuesHash(t, renderedWith(t, directory, map[string][]byte{source: data}), name)
					if rehashed == hash || !strings.HasPrefix(rehashed, generator+"-") {
						t.Errorf("changing %s keeps the runner template hash %s: %s", field, hash, rehashed)
					}
				}
				release := load(t, releases[name])
				set(release, "45m", "spec", "interval")
				data, err := yaml.Marshal(release)
				if err != nil {
					t.Fatal(err)
				}
				if rehashed := valuesHash(t, renderedWith(t, directory, map[string][]byte{releases[name]: data}), name); rehashed != hash {
					t.Errorf("changing the reconcile interval of %s replaces its idle runners: %s", name, rehashed)
				}
			})
		}
	}
	if warm == 0 {
		t.Fatal("no runner scale set keeps warm runners")
	}
}
