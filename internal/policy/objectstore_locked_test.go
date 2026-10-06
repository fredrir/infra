package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestLockedBucketsUseTheProtectedPrefix(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), objectStore, "buckets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Cells []struct {
			Buckets []struct {
				Name     string `yaml:"name"`
				LockDays int    `yaml:"lockDays"`
			} `yaml:"buckets"`
		} `yaml:"cells"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	for _, cell := range spec.Cells {
		for _, bucket := range cell.Buckets {
			if bucket.LockDays > 0 && !strings.HasPrefix(bucket.Name, "restic-") {
				t.Errorf("locked bucket %s is outside the filter's restic- prefix, so writers could batch-delete its expired versions", bucket.Name)
			}
		}
	}
}
