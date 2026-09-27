package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParserReachesItsDatasetOnlyThroughTheNLCell(t *testing.T) {
	authority, err := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/object-store-trust/ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	resources := renderedTree(t, "platform", "platform/projects/llunde-pyparser/application")
	bundle := ""
	for _, resource := range resources {
		name, _ := lookup(resource, "metadata", "name").(string)
		if resource["kind"] == "ConfigMap" && strings.HasPrefix(name, "object-store-ca-") && lookup(resource, "data", "ca.crt") == string(authority) {
			bundle = name
		}
	}
	if bundle == "" {
		t.Fatal("parser application does not publish the object store CA under a content-hashed name")
	}
	want := map[string]string{
		"AWS_ENDPOINT_URL_S3":        "https://seaweedfs-nl.object-store.svc.cluster.local:8333",
		"AWS_CA_BUNDLE":              "/etc/object-store/ca.crt",
		"AWS_DEFAULT_REGION":         "nl",
		"PYPARSER_S3_DATASET_BUCKET": "parser-dataset",
	}
	writers := map[string]bool{}
	for _, resource := range resources {
		if resource["kind"] != "Deployment" {
			continue
		}
		name := at(resource, "metadata", "name").(string)
		writers[name] = true
		pod := at(resource, "spec", "template", "spec").(object)
		volumes := map[string]string{}
		for _, volume := range pod["volumes"].([]any) {
			if reference, ok := lookup(volume, "configMap", "name").(string); ok {
				volumes[at(volume, "name").(string)] = reference
			}
		}
		for _, item := range pod["containers"].([]any) {
			container := item.(object)
			environment := map[string]string{}
			for _, env := range container["env"].([]any) {
				if value, ok := lookup(env, "value").(string); ok {
					environment[at(env, "name").(string)] = value
				}
			}
			for key, value := range want {
				if environment[key] != value {
					t.Errorf("%s sets %s=%q, want %q", name, key, environment[key], value)
				}
			}
			mounted := false
			for _, mount := range container["volumeMounts"].([]any) {
				mounted = mounted || (lookup(mount, "mountPath") == "/etc/object-store" && volumes[at(mount, "name").(string)] == bundle)
			}
			if !mounted {
				t.Errorf("%s does not mount the object store CA at /etc/object-store", name)
			}
		}
	}
	if len(writers) != 3 || !writers["review"] || !writers["worker-extract"] || !writers["worker-light"] {
		t.Fatalf("parser writers: %v", writers)
	}
}
