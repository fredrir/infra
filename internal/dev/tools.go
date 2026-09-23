package dev

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fredrir/infra/internal/ci"
)

type Tool struct {
	Name        string
	VersionArgs []string
}

var Tools = []Tool{
	{"tofu", []string{"version"}},
	{"flux", []string{"--version"}},
	{"kubectl", []string{"version", "--client"}},
	{"helm", []string{"version", "--short"}},
	{"kustomize", []string{"version"}},
	{"actionlint", []string{"-version"}},
	{"sops", []string{"--version"}},
	{"age", []string{"--version"}},
	{"age-keygen", []string{"--version"}},
	{"yq", []string{"--version"}},
	{"k3d", []string{"version"}},
	{"hyperfine", []string{"--version"}},
	{"uv", []string{"--version"}},
	{"nfpm", []string{"--version"}},
}

var versionPattern = regexp.MustCompile(`v?(\d+\.\d+\.\d+)`)

func PinnedVersion(asset ci.ToolAsset) string {
	match := versionPattern.FindStringSubmatch(asset.URL)
	if match == nil {
		return ""
	}
	return match[1]
}

func (s State) toolPath(name string) (string, error) {
	local := filepath.Join(s.Tools(), name)
	if info, err := os.Stat(local); err == nil && info.Mode().IsRegular() {
		return local, nil
	}
	return exec.LookPath(name)
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(line)
}
