package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const bazelCacheComponent = "platform/components/bazel-cache"

type bazelCacheStatefulSet struct {
	Spec struct {
		Template struct {
			Spec struct {
				NodeSelector map[string]string `yaml:"nodeSelector"`
				Containers   []struct {
					Name  string `yaml:"name"`
					Image string `yaml:"image"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func bazelCacheManifest(t *testing.T) bazelCacheStatefulSet {
	t.Helper()
	var statefulSet bazelCacheStatefulSet
	if err := yaml.Unmarshal(read(t, filepath.Join(root(t), bazelCacheComponent, "bazel-cache.yaml")), &statefulSet); err != nil {
		t.Fatal(err)
	}
	return statefulSet
}

func TestBazelCacheFilterBindsTheTailnetAddressOfItsNode(t *testing.T) {
	repository := root(t)
	node := bazelCacheManifest(t).Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
	var inventory struct {
		All map[string]any `yaml:"all"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, "ansible/inventory/production.yml")), &inventory); err != nil {
		t.Fatal(err)
	}
	hosts := map[string]map[string]any{}
	inventoryHosts(inventory.All, hosts)
	address := fmt.Sprint(hosts[node]["tailscale_ip"])
	if node == "" || !strings.HasPrefix(address, "100.") {
		t.Fatalf("bazel cache node %q has tailnet address %q", node, address)
	}
	filter := string(read(t, filepath.Join(repository, bazelCacheComponent, "nginx.conf")))
	listens := regexp.MustCompile(`(?m)^\s*listen ([^:;\s]+):\d+;$`).FindAllStringSubmatch(filter, -1)
	denials := regexp.MustCompile(`(?m)^\s*deny (\d[^;]*);$`).FindAllStringSubmatch(filter, -1)
	if len(listens) != 3 || len(denials) != 2 || strings.Count(filter, "listen ") != 3 {
		t.Fatalf("filter listens on %v and refuses %v, want three listeners and two refusals of the node", listens, denials)
	}
	for _, bound := range append(listens, denials...) {
		if bound[1] != address {
			t.Errorf("filter names %s, want %s's tailnet address %s", bound[1], node, address)
		}
	}
}

func TestBazelCacheFilterTestRunsTheShippedVersionsOnEveryFilterChange(t *testing.T) {
	repository := root(t)
	sum := sha256.Sum256(read(t, filepath.Join(repository, bazelCacheComponent, "nginx.conf")))
	want := hex.EncodeToString(sum[:]) + "  nginx.conf\n"
	if recorded := string(read(t, filepath.Join(repository, bazelCacheComponent, "filtertest/nginx.conf.sha256"))); recorded != want {
		t.Fatalf("filtertest/nginx.conf.sha256 records %q, the filter hashes to %q; rerun the filter test and update the digest", recorded, want)
	}
	module := string(read(t, filepath.Join(repository, bazelCacheComponent, "filtertest/go.mod")))
	required := func(path string) string {
		match := regexp.MustCompile(`(?m)^(?:require )?\s*` + regexp.QuoteMeta(path) + ` (\S+)`).FindStringSubmatch(module)
		if match == nil {
			return ""
		}
		return match[1]
	}
	for _, container := range bazelCacheManifest(t).Spec.Template.Spec.Containers {
		if name, tag, found := strings.Cut(strings.Split(container.Image, "@")[0], ":"); found && name == "docker.io/buchgr/bazel-remote-cache" && required("github.com/buchgr/bazel-remote/v2") != tag {
			t.Errorf("filter test builds bazel-remote %q, the cache runs %s", required("github.com/buchgr/bazel-remote/v2"), tag)
		}
	}
	if !regexp.MustCompile(`(?m)^tool github\.com/buchgr/bazel-remote/v2$`).MatchString(module) {
		t.Error("filter test module does not pin bazel-remote as a tool")
	}
	harness := string(read(t, filepath.Join(repository, bazelCacheComponent, "filtertest/filter_test.go")))
	if !strings.Contains(harness, `"../bazel-cache.yaml"`) || strings.Contains(harness, "docker.io/library/nginx") {
		t.Error("filter test does not run the filter image the cache ships")
	}
	var workflow struct {
		On map[string]struct {
			Paths []string `yaml:"paths"`
		} `yaml:"on"`
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(read(t, filepath.Join(repository, ".github/workflows/bazel-cache-filter.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"push", "pull_request"} {
		for _, path := range []string{bazelCacheComponent + "/**", ".github/workflows/bazel-cache-filter.yml"} {
			if !slices.Contains(workflow.On[event].Paths, path) {
				t.Errorf("bazel-cache-filter.yml does not run on %s changes to %s", event, path)
			}
		}
	}
	runs := slices.ContainsFunc(workflow.Jobs["filter"].Steps, func(step struct {
		Run string `yaml:"run"`
	}) bool {
		return strings.Contains(step.Run, "-C "+bazelCacheComponent+"/filtertest test ./...")
	})
	if !runs {
		t.Error("bazel-cache-filter.yml does not run the filter test module")
	}
}
