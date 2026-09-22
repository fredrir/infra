package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Selection struct {
	Tofu        bool     `json:"tofu"`
	Kubernetes  bool     `json:"kubernetes"`
	Ansible     bool     `json:"ansible"`
	MonitorOnly bool     `json:"monitor_only"`
	Projects    []string `json:"projects,omitempty"`
}

var projectNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func All() Selection { return Selection{Tofu: true, Kubernetes: true, Ansible: true} }

func Affected(paths []string) Selection {
	var selected Selection
	hosts := false
	for _, path := range paths {
		switch {
		case path == "platform/clusters/production/settings.yaml":
			selected.Tofu, selected.Kubernetes, selected.Ansible = true, true, true
		case strings.HasPrefix(path, "ansible/roles/gatus/"):
			selected.Ansible = true
		case strings.HasPrefix(path, "tofu/"):
			selected.Tofu, selected.Ansible, hosts = true, true, true
		case strings.HasPrefix(path, "platform/"), strings.HasPrefix(path, "charts/"), strings.HasPrefix(path, "keys/"):
			selected.Kubernetes = true
			if strings.HasPrefix(path, "platform/components/backups/") {
				selected.Ansible, hosts = true, true
			}
		case strings.HasPrefix(path, "ansible/"), strings.HasPrefix(path, "secrets/"), path == "build/cli-release.json", path == "build/toolchain.json":
			selected.Ansible, hosts = true, true
		case strings.HasPrefix(path, "cmd/"), strings.HasPrefix(path, "internal/"), path == "go.mod", path == "go.sum", path == ".github/workflows/reconcile.yml", strings.HasPrefix(path, ".github/actions/setup-reconciliation/"):
			return All()
		}
	}
	selected.MonitorOnly = selected.Ansible && !hosts
	if selected.Kubernetes && !selected.Tofu && !selected.Ansible {
		selected.Projects = projectScope(paths)
	}
	return selected
}

func projectScope(paths []string) []string {
	var project string
	for _, path := range paths {
		parts := strings.Split(path, "/")
		if len(parts) < 3 || parts[0] != "platform" || parts[1] != "projects" || !projectNamePattern.MatchString(parts[2]) {
			return nil
		}
		if project == "" {
			project = parts[2]
			continue
		}
		if project != parts[2] {
			return nil
		}
	}
	if project == "" || len(paths) == 0 {
		return nil
	}
	return []string{project}
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
