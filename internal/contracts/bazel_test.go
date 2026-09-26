package contracts

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCIRepositoryRulesNeverProbeHostedRunnerToolchains(t *testing.T) {
	var paths []string
	for _, line := range strings.Split(string(read(t, filepath.Join(root(t), ".bazelrc"))), "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), "build:ci --repo_env=PATH="); found {
			paths = append(paths, value)
		}
	}
	if !slices.Equal(paths, []string{"/usr/bin:/bin"}) {
		t.Fatalf("CI repository rules search %q, want only /usr/bin:/bin so toolchains installed under /usr/local/bin are never probed", paths)
	}
}
