package filtertest

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestQualificationClientWritesAndReadsThroughTheFilter(t *testing.T) {
	c := startCache(t)
	for _, role := range []string{"writer", "reader"} {
		writer := c.writer
		if role == "reader" {
			writer = hostIP + ":" + freePort(t)
		}
		command := exec.Command("python3", "-B", "../../../../.github/scripts/cache_qualification.py", role,
			"--nonce", t.Name(), "--reader", c.reader, "--writer", writer, "--report", filepath.Join(t.TempDir(), "receipt.json"))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s qualification failed: %v\n%s", role, err, output)
		}
	}
}
