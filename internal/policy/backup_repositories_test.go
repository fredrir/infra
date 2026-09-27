package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

var primaryBackupProjects = map[string]string{"llunde-pyparser": "parser", "y": "y", "portfolio": "portfolio"}

func TestPrimaryBackupBucketsKeepHistoryFromTheirWriters(t *testing.T) {
	var spec struct {
		Cells []struct {
			Name    string
			Buckets []struct {
				Name           string
				LockDays       int `json:"lockDays"`
				NoncurrentDays int `json:"noncurrentDays"`
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(repoRoot(t), objectStore, "buckets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(yamlObjects(t, data)[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &spec); err != nil {
		t.Fatal(err)
	}
	locked := map[string]bool{}
	for _, cell := range spec.Cells {
		for _, bucket := range cell.Buckets {
			if cell.Name == "hel1" && bucket.LockDays >= 30 && bucket.NoncurrentDays > bucket.LockDays {
				locked[bucket.Name] = true
			}
		}
	}
	for _, project := range primaryBackupProjects {
		if !locked["restic-"+project] {
			t.Errorf("restic-%s is not a COMPLIANCE-locked hel1 bucket holding 30 days of history", project)
		}
	}
}
