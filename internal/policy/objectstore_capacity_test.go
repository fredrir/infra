package policy

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestObjectStoreCellsFitTheirVolumes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), objectStore, "buckets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Cells []struct {
			Name    string `yaml:"name"`
			Buckets []struct {
				QuotaGiB int `yaml:"quotaGiB"`
			} `yaml:"buckets"`
		} `yaml:"cells"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	quotas := map[string]int{}
	for _, cell := range spec.Cells {
		for _, bucket := range cell.Buckets {
			quotas[cell.Name] += bucket.QuotaGiB
		}
	}
	checked := 0
	for _, resource := range objectStoreResources(t) {
		name, _ := lookup(resource, "metadata", "name").(string)
		cell, ok := strings.CutPrefix(name, "seaweedfs-")
		if resource["kind"] != "StatefulSet" || !ok {
			continue
		}
		flags := map[string]int{}
		for _, argument := range at(resource, "spec", "template", "spec", "containers", 0, "args").([]any) {
			key, value, found := strings.Cut(strings.TrimPrefix(argument.(string), "-"), "=")
			if number, err := strconv.Atoi(value); found && err == nil {
				flags[key] = number
			}
		}
		claimed := 0
		for _, claim := range at(resource, "spec", "volumeClaimTemplates").([]any) {
			if at(claim, "metadata", "name") == "data" {
				claimed, err = strconv.Atoi(strings.TrimSuffix(at(claim, "spec", "resources", "requests", "storage").(string), "Gi"))
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		slots := flags["volume.max"] * flags["master.volumeSizeLimitMB"] / 1024
		if slots == 0 || claimed == 0 {
			t.Fatalf("%s declares %d GiB of volumes on a %d Gi claim", name, slots, claimed)
		}
		if quotas[cell]*13 > slots*10 {
			t.Errorf("%s quotas total %d GiB; with 30%% slack they need more than its %d GiB of volumes", name, quotas[cell], slots)
		}
		if slots+4 > claimed {
			t.Errorf("%s volumes reach %d GiB of its %d Gi claim, leaving no room for a compaction copy, the filer store and indexes", name, slots, claimed)
		}
		checked++
	}
	if checked != len(quotas) {
		t.Fatalf("checked %d cells for %d declared in buckets.yaml", checked, len(quotas))
	}
}
