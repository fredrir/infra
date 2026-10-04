package contracts

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestKubernetesNodesRaiseInotifyLimitsAboveShimUsage(t *testing.T) {
	var tasks []struct {
		Copy struct {
			Dest    string `yaml:"dest"`
			Content string `yaml:"content"`
		} `yaml:"ansible.builtin.copy"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), "ansible/roles/k3s/tasks/main.yml")), &tasks); err != nil {
		t.Fatal(err)
	}
	settings := map[string]int{}
	for _, task := range tasks {
		if task.Copy.Dest != "/etc/sysctl.d/90-k3s.conf" {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(task.Copy.Content), "\n") {
			key, value, _ := strings.Cut(line, "=")
			settings[key], _ = strconv.Atoi(value)
		}
	}
	for key, floor := range map[string]int{"fs.inotify.max_user_instances": 8192, "fs.inotify.max_user_watches": 1048576} {
		if settings[key] < floor {
			t.Errorf("%s is %d on Kubernetes nodes, below %d: containerd shims and the k3s agent exhaust the stock limit and starve PID 1", key, settings[key], floor)
		}
	}
}
