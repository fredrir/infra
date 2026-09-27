package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestLockedBucketsRejectBatchDeletes(t *testing.T) {
	caddyfile, err := os.ReadFile(filepath.Join(repoRoot(t), objectStore, "s3-filter.caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	batchDelete := "{http.request.uri.path}.matches(\"^/[^/]+/?$\")\n\t\t\t\t&& !{http.request.uri.path}.matches(\"^/restic-\")\n\t\t\t\t&& {http.request.uri.query}.matches(\"(^|&)delete([=&]|$)\")"
	if !strings.Contains(string(caddyfile), batchDelete) {
		t.Fatal("the S3 filter admits batch deletes on restic- buckets")
	}
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
