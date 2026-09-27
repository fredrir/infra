package contracts

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestCacheWriterCleanupAgainstHTTPAPI(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-B", "-m", "unittest", "discover", "-s", ".github/scripts", "-p", "cache_writer_cleanup_test.py")
	command.Dir = root(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cleanup HTTP qualification failed: %v\n%s", err, output)
	}
}

func TestCacheWriterJobsExpireBeforeCleanupEligibility(t *testing.T) {
	for workflow, job := range map[string]string{"check.yml": "check", "infra-cli.yml": "build"} {
		var document struct {
			Jobs map[string]struct {
				Timeout int `yaml:"timeout-minutes"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(read(t, filepath.Join(root(t), ".github/workflows", workflow)), &document); err != nil {
			t.Fatal(err)
		}
		if timeout := document.Jobs[job].Timeout; timeout <= 0 || timeout > 30 {
			t.Errorf("%s/%s timeout %d leaves less than 30 minutes before writer cleanup", workflow, job, timeout)
		}
	}
}
