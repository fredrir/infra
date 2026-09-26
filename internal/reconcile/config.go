package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Selection struct {
	Tofu         bool     `json:"tofu"`
	Kubernetes   bool     `json:"kubernetes"`
	Ansible      bool     `json:"ansible"`
	MonitorOnly  bool     `json:"monitor_only"`
	Tooling      bool     `json:"tooling"`
	Projects     []string `json:"projects,omitempty"`
	HostScope    string   `json:"host_scope"`
	RunnerInputs []string `json:"runner_inputs,omitempty"`
	Reasons      []string `json:"reasons,omitempty"`
}

var projectNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

const (
	HostScopeFull    = "full"
	HostScopeRunners = "runners"
	HostScopeMonitor = "monitor"
	HostScopeNone    = "none"
)

func All() Selection {
	return Selection{Tofu: true, Kubernetes: true, Ansible: true, HostScope: HostScopeFull, Reasons: []string{"full reconciliation"}}
}

func Affected(paths []string) Selection {
	selected := Selection{HostScope: HostScopeNone}
	var deploymentPaths []string
	for _, path := range paths {
		if deploymentIndependent(path) {
			selected.Reasons = append(selected.Reasons, "deployment-independent input: "+path)
			continue
		}
		if toolingInput(path) {
			selected.Tooling = true
			selected.Reasons = append(selected.Reasons, "tooling input: "+path)
			continue
		}
		deploymentPaths = append(deploymentPaths, path)
		selected.Reasons = append(selected.Reasons, "deployment input: "+path)
		switch {
		case path == "platform/clusters/production/settings.yaml":
			selected.Tofu, selected.Kubernetes = true, true
			selectHosts(&selected, HostScopeMonitor)
		case strings.HasPrefix(path, "ansible/roles/gatus/"), strings.HasPrefix(path, "ansible/roles/verification_trigger/"):
			selectHosts(&selected, HostScopeMonitor)
		case path == "build/cli-release.json", path == "build/runners.json", path == "build/toolchain.json", path == "ansible/build-runners.yml", path == "ansible/verify-runners.yml", strings.HasPrefix(path, "ansible/roles/build_runner/"), strings.HasPrefix(path, "ansible/roles/build_engine/"):
			selected.RunnerInputs = append(selected.RunnerInputs, path)
			selectHosts(&selected, HostScopeRunners)
		case strings.HasPrefix(path, "tofu/"):
			selected.Tofu = true
			selectHosts(&selected, HostScopeFull)
		case sharedDeploymentInput(path):
			full := All()
			full.Reasons = []string{"shared deployment input: " + path}
			return full
		case strings.HasPrefix(path, "platform/"), strings.HasPrefix(path, "charts/"), strings.HasPrefix(path, "keys/"):
			selected.Kubernetes = true
		case strings.HasPrefix(path, "ansible/"), strings.HasPrefix(path, "secrets/"):
			selectHosts(&selected, HostScopeFull)
		default:
			full := All()
			full.Reasons = []string{"unclassified input: " + path}
			return full
		}
	}
	if selected.Kubernetes && !selected.Tofu && !selected.Ansible {
		selected.Projects = projectScope(deploymentPaths)
	}
	return selected
}

func sharedDeploymentInput(path string) bool {
	for _, prefix := range []string{"tailscale/", "build/rollout/flux-artifacts/", "platform/components/policy/", "platform/clusters/production/flux-system/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	switch path {
	case "platform/clusters/production/root.yaml", ".sops.yaml", "pyproject.toml", "uv.lock":
		return true
	default:
		return false
	}
}

func toolingInput(path string) bool {
	if !canonicalPath(path) {
		return false
	}
	for _, prefix := range []string{"cmd/", "internal/", ".github/", "build/tools/", "build/release/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	if filepath.Dir(path) == "build/rollout" {
		return true
	}
	switch path {
	case "build/BUILD.bazel", "build/consumers.json", "build/dagger-embed.patch", "build/publisher.json", "go.mod", "go.sum", "BUILD.bazel", "MODULE.bazel", "MODULE.bazel.lock", ".bazelrc", ".bazelversion", ".dockerignore", ".envrc", ".gitignore":
		return true
	default:
		return false
	}
}

func canonicalPath(path string) bool {
	return !strings.HasPrefix(path, "/") && !strings.Contains(path, "\\") && filepath.ToSlash(filepath.Clean(path)) == path
}

func deploymentIndependent(path string) bool {
	if !canonicalPath(path) {
		return false
	}
	switch path {
	case ".github/workflows/performance.yml", ".github/workflows/check.yml", ".github/workflows/infra-cli.yml", ".github/workflows/images.yml", ".github/workflows/build-image.yml", ".github/workflows/test-image.yml", ".github/workflows/cli-release.yml", ".github/workflows/rust-ci.yml", ".github/workflows/rust-release.yml", ".github/workflows/rust-auto-tag.yml", ".github/workflows/packages-publish.yml", ".github/actionlint.yaml", ".github/CODEOWNERS", ".editorconfig", ".gitleaks.toml", ".gitleaksignore", ".taplo.toml", ".yamllint.yaml", "biome.json", "renovate.json", "ruff.toml":
		return true
	}
	return strings.HasPrefix(path, "docs/") || strings.HasPrefix(path, "build/evidence/") ||
		strings.HasPrefix(path, ".vscode/") || strings.HasPrefix(path, "internal/dev/") ||
		strings.HasPrefix(path, "images/") || strings.HasPrefix(path, "dev/") ||
		strings.HasPrefix(path, "tests/") || pushIgnored(path) ||
		((strings.HasPrefix(path, "internal/") || strings.HasPrefix(path, "cmd/")) && strings.HasSuffix(path, "_test.go"))
}

func selectHosts(selected *Selection, scope string) {
	current := selected.HostScope
	if current == HostScopeFull || (current != HostScopeNone && current != "" && current != scope) {
		scope = HostScopeFull
	}
	selected.HostScope = scope
	selected.Ansible = scope != HostScopeNone
	selected.MonitorOnly = scope == HostScopeMonitor
}

func effectiveHostScope(selected Selection) string {
	if !selected.Ansible {
		return HostScopeNone
	}
	switch selected.HostScope {
	case HostScopeFull, HostScopeRunners, HostScopeMonitor:
		return selected.HostScope
	default:
		if selected.MonitorOnly {
			return HostScopeMonitor
		}
		return HostScopeFull
	}
}

func projectScope(paths []string) []string {
	var projects []string
	for _, path := range paths {
		parts := strings.Split(path, "/")
		if len(parts) < 3 || parts[0] != "platform" || parts[1] != "projects" || !projectNamePattern.MatchString(parts[2]) {
			return nil
		}
		projects = append(projects, parts[2])
	}
	slices.Sort(projects)
	return slices.Compact(projects)
}

func Host(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "platform/clusters/production/settings.yaml"))
	if err != nil {
		return "", err
	}
	var settings struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return "", err
	}
	host := settings.Data["GRAFANA_HOST"]
	if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.fredrir\.com$`).MatchString(host) {
		return "", fmt.Errorf("invalid Grafana hostname %q", host)
	}
	return host, nil
}
