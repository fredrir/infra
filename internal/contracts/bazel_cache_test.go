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
	if alias := parseTailnetPolicy(t, read(t, filepath.Join(repository, "tailscale/policy.hujson"))).Hosts["bazel-cache"]; alias != address {
		t.Errorf("tailnet host bazel-cache is %q, want %s's tailnet address %s", alias, node, address)
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

func TestBazelCachePortsAreGrantedOnlyToTheCacheRoles(t *testing.T) {
	policy := parseTailnetPolicy(t, read(t, filepath.Join(root(t), "tailscale/policy.hujson")))
	var grants []string
	for _, rule := range policy.ACLs {
		for _, destination := range rule.Destination {
			host, ports, err := splitTailnetEndpoint(destination)
			if err != nil {
				t.Fatal(err)
			}
			for _, port := range []int{9092, 9093, 9095} {
				matched, err := tailnetPortMatches(ports, port)
				if err != nil {
					t.Fatal(err)
				}
				if !matched || host == "autogroup:self" {
					continue
				}
				if host != "bazel-cache" {
					t.Errorf("%s grants port %d on %s, which may include the cache host", rule.Source, port, host)
					continue
				}
				for _, source := range rule.Source {
					grants = append(grants, fmt.Sprintf("%s %s → %d", source, rule.Proto, port))
				}
			}
		}
	}
	slices.Sort(grants)
	want := []string{"archie tcp → 9092", "macie tcp → 9092", "tag:ci-bazel-reader tcp → 9092", "tag:ci-bazel-writer tcp → 9092", "tag:ci-bazel-writer tcp → 9093"}
	if !slices.Equal(grants, want) {
		t.Errorf("cache grants = %q, want %q", grants, want)
	}
}

func TestOnlyProtectedMainRunsReachTheCacheWriterPort(t *testing.T) {
	policy := parseTailnetPolicy(t, read(t, filepath.Join(root(t), "tailscale/policy.hujson")))
	identities := federatedIdentities(t)
	workflow := func(file, ref string) string { return "fredrir/infra/.github/workflows/" + file + "@" + ref }
	for _, test := range []struct {
		name    string
		subject string
		claims  map[string]string
		ports   []int
	}{
		{"main push", "ref:refs/heads/main", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("check.yml", "refs/heads/main")}, []int{9092, 9093}},
		{"main dispatch of the CLI build", "ref:refs/heads/main", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("infra-cli.yml", "refs/heads/main")}, []int{9092, 9093}},
		{"main image build", "ref:refs/heads/main", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("build-image.yml", "refs/heads/main")}, nil},
		{"main signing release", "ref:refs/heads/main", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("cosign-release.yml", "refs/heads/main")}, nil},
		{"main deploy dispatch", "ref:refs/heads/main", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("deploy.yml", "refs/heads/main")}, nil},
		{"future workflow on main", "ref:refs/heads/main", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("future.yml", "refs/heads/main")}, nil},
		{"check workflow from a branch ending in main's ref", "ref:refs/heads/main", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("check.yml", "refs/heads/x@refs/heads/main")}, nil},
		{"check workflow of another repository", "ref:refs/heads/main", map[string]string{"ref_protected": "true", "job_workflow_ref": "attacker/infra/.github/workflows/check.yml@refs/heads/main"}, nil},
		{"pull request", "pull_request", map[string]string{"ref_protected": "false", "job_workflow_ref": workflow("check.yml", "refs/pull/7/merge")}, []int{9092}},
		{"pull request claiming main's workflow", "pull_request", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("check.yml", "refs/heads/main")}, []int{9092}},
		{"release tag", "ref:refs/tags/infra-v0.2.20", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("cli-release.yml", "refs/tags/infra-v0.2.20")}, []int{9092}},
		{"unprotected tag", "ref:refs/tags/infra-v9", map[string]string{"ref_protected": "false", "job_workflow_ref": workflow("cli-release.yml", "refs/tags/infra-v9")}, nil},
		{"branch push", "ref:refs/heads/feature", map[string]string{"ref_protected": "false", "job_workflow_ref": workflow("check.yml", "refs/heads/feature")}, nil},
		{"branch named after main", "ref:refs/heads/main-copy", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("check.yml", "refs/heads/main-copy")}, nil},
		{"unprotected main", "ref:refs/heads/main", map[string]string{"ref_protected": "false", "job_workflow_ref": workflow("check.yml", "refs/heads/main")}, nil},
		{"unprotected main CLI build", "ref:refs/heads/main", map[string]string{"ref_protected": "false", "job_workflow_ref": workflow("infra-cli.yml", "refs/heads/main")}, nil},
		{"workflow from a pull request ref on main", "ref:refs/heads/main", map[string]string{"ref_protected": "true", "job_workflow_ref": workflow("check.yml", "refs/pull/7/merge")}, nil},
		{"plan environment", "environment:infrastructure-plan", map[string]string{"ref_protected": "false", "job_workflow_ref": workflow("reconcile-job.yml", "refs/pull/7/merge")}, nil},
	} {
		var reached []int
		for _, identity := range identities {
			for _, prefix := range []string{repositorySubject, "repo:fredrir/infra:", "repo:attacker@1/infra@1328085692:"} {
				token := oidcToken{subject: prefix + test.subject, audience: identity.Audience, claims: test.claims}
				if !identity.accepts(token) {
					continue
				}
				if prefix != repositorySubject {
					t.Errorf("%s: %s accepts subject %s", test.name, identity.Name, token.subject)
				}
				for _, tag := range identity.Tags {
					for _, port := range []int{9092, 9093, 9095} {
						allowed, err := policy.allows(tag, "tcp", fmt.Sprintf("bazel-cache:%d", port))
						if err != nil {
							t.Fatal(err)
						}
						if allowed && !slices.Contains(reached, port) {
							reached = append(reached, port)
						}
					}
				}
			}
		}
		slices.Sort(reached)
		if !slices.Equal(reached, test.ports) {
			t.Errorf("%s reaches cache ports %v, want %v", test.name, reached, test.ports)
		}
	}
}
