package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunnerAdmissionBoundaries(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	base := rustRunner(t, "main")
	if !e.runner(base, "ci-nsql", controller) {
		t.Fatal("qualified runner rejected")
	}
	for name, value := range (object{"hostNetwork": true, "automountServiceAccountToken": true, "runtimeClassName": "runc", "activeDeadlineSeconds": 86400}) {
		t.Run(name, func(t *testing.T) {
			p := clone(base).(object)
			set(p, value, "spec", name)
			if e.runner(p, "ci-nsql", controller) {
				t.Fatal("unsafe runner admitted")
			}
		})
	}
	for _, mutate := range []func(object){func(p object) { appendAt(p, clone(at(p, "spec", "containers", 0)), "spec", "containers") }, func(p object) {
		set(p, []any{object{"name": "credentials", "secret": object{"secretName": "github-app"}}}, "spec", "volumes")
	}} {
		p := clone(base).(object)
		mutate(p)
		if e.runner(p, "ci-nsql", controller) {
			t.Fatal("sidecar or secret volume admitted")
		}
	}
}

func TestRunnerJITTokenIsBoundToControllerAndOwnPod(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	p := rustRunner(t, "main")
	secret := object{"name": "ACTIONS_RUNNER_INPUT_JITCONFIG", "valueFrom": object{"secretKeyRef": object{"name": "rust-1", "key": "jitToken"}}}
	appendAt(p, secret, "spec", "containers", 0, "env")
	if !e.runner(p, "ci-nsql", controller) || e.runner(p, "ci-nsql", "untrusted") {
		t.Fatal("JIT controller boundary")
	}
	set(secret, "github-app", "valueFrom", "secretKeyRef", "name")
	if e.runner(p, "ci-nsql", controller) {
		t.Fatal("foreign JIT token admitted")
	}
}

func TestRunnerSlotQuantitiesAreExact(t *testing.T) {
	t.Parallel()
	base := rustRunner(t, "main")
	for _, tc := range []struct {
		value   any
		allowed bool
	}{{"1", true}, {1, true}, {"2", false}, {"0", false}, {"1000m", false}, {nil, false}} {
		t.Run(fmt.Sprint(tc.value), func(t *testing.T) {
			e := newEvaluator(t)
			p := clone(base).(object)
			if tc.value == nil {
				delete(at(p, "spec", "containers", 0, "resources", "limits").(object), "infra.fredrir.com/ci-slot")
			} else {
				set(p, tc.value, "spec", "containers", 0, "resources", "limits", "infra.fredrir.com/ci-slot")
			}
			if e.runner(p, "ci-nsql", controller) != tc.allowed {
				t.Fatal("incorrect slot admission")
			}
		})
	}
}

func TestDeployPoolRemainsBounded(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	deploy := runner(t, "infra/deploy-values.yaml")
	deploy["metadata"] = object{"name": "deploy-1", "labels": object{"actions.github.com/scale-set-name": "deploy-amd64"}}
	if !e.runner(deploy, "ci-infra", controller) || e.runner(deploy, "ci-nsql", controller) {
		t.Fatal("deploy namespace boundary")
	}
	unapprovedDeploy := clone(deploy).(object)
	set(unapprovedDeploy, "ghcr.io/fredrir/infra-runner-deploy@sha256:"+strings.Repeat("f", 64), "spec", "containers", 0, "image")
	if e.runner(unapprovedDeploy, "ci-infra", controller) {
		t.Fatal("unapproved deploy image admitted")
	}
	for _, mutate := range []func(object){func(p object) { set(p, "other-amd64", "metadata", "labels", "actions.github.com/scale-set-name") }, func(p object) { set(p, "4", "spec", "containers", 0, "resources", "limits", "cpu") }, func(p object) { set(p, "8Gi", "spec", "containers", 0, "resources", "limits", "memory") }} {
		p := clone(deploy).(object)
		mutate(p)
		if e.runner(p, "ci-infra", controller) {
			t.Fatal("unbounded slot-free deploy admitted")
		}
	}
}

func TestRustCacheCredentialsStayWithinPool(t *testing.T) {
	t.Parallel()
	e := newEvaluator(t)
	pools := map[string]string{"pr": "sccache-ro", "main": "sccache-rw", "release": "sccache-release"}
	for variant, own := range pools {
		t.Run(variant, func(t *testing.T) {
			p := rustRunner(t, variant)
			if !e.runner(p, "ci-example", controller) || e.runner(p, "ci-example", "untrusted") {
				t.Fatal("rust controller boundary")
			}
			for _, other := range pools {
				if other == own {
					continue
				}
				stolen := clone(p).(object)
				for _, env := range at(stolen, "spec", "containers", 0, "env").([]any) {
					if strings.HasPrefix(at(env, "name").(string), "AWS_") {
						set(env, other, "valueFrom", "secretKeyRef", "name")
					}
				}
				if e.runner(stolen, "ci-example", controller) {
					t.Fatal("foreign cache credentials admitted")
				}
			}
		})
	}
	base := rustRunner(t, "main")
	deployImage := at(runner(t, "infra/deploy-values.yaml"), "spec", "containers", 0, "image")
	for _, mutate := range []func(object){func(p object) { set(p, object{}, "metadata", "labels") }, func(p object) { set(p, "deploy-amd64", "metadata", "labels", "actions.github.com/scale-set-name") }, func(p object) { set(p, deployImage, "spec", "containers", 0, "image") }, func(p object) {
		set(p, "AWS_SECRET_ACCESS_KEY", "spec", "containers", 0, "env", 4, "valueFrom", "secretKeyRef", "key")
	}, func(p object) {
		appendAt(p, object{"name": "GITHUB_TOKEN", "valueFrom": object{"secretKeyRef": object{"name": "sccache-rw", "key": "AWS_ACCESS_KEY_ID"}}}, "spec", "containers", 0, "env")
	}} {
		p := clone(base).(object)
		mutate(p)
		if e.runner(p, "ci-example", controller) {
			t.Fatal("rust cache boundary bypass")
		}
	}
	p := rustRunner(t, "pr")
	set(p, "kata", "spec", "runtimeClassName")
	set(p, object{"node-restriction.kubernetes.io/kata": "true", "kubernetes.io/arch": "amd64"}, "spec", "nodeSelector")
	set(p, object{"type": "Localhost", "localhostProfile": "kata-nix.json"}, "spec", "securityContext", "seccompProfile")
	if e.runner(p, "ci-example", controller) {
		t.Fatal("unqualified rust Kata runtime admitted")
	}
}

const (
	gvisorWorker = "node-restriction.kubernetes.io/gvisor"
	kataWorker   = "node-restriction.kubernetes.io/kata"
)

func placementScore(nodeAffinity object, labels ...string) int {
	score := 0
	preferences, _ := nodeAffinity["preferredDuringSchedulingIgnoredDuringExecution"].([]any)
	for _, item := range preferences {
		matches := true
		for _, expression := range at(item, "preference", "matchExpressions").([]any) {
			present := slices.Contains(labels, at(expression, "key").(string))
			switch at(expression, "operator") {
			case "Exists":
				matches = matches && present
			case "DoesNotExist":
				matches = matches && !present
			case "In":
				matches = matches && present && fmt.Sprint(at(expression, "values")) == "[true]"
			default:
				matches = false
			}
		}
		if matches {
			score += at(item, "weight").(int)
		}
	}
	return score
}

func TestNoCIPoolToleratesVolatileWorkers(t *testing.T) {
	t.Parallel()
	for _, overlay := range at(load(t, "platform/components/runners/kustomization.yaml"), "resources").([]any) {
		for _, resource := range rendered(t, "platform/components/runners/"+overlay.(string)) {
			if resource["kind"] != "HelmRelease" {
				continue
			}
			values := at(resource, "spec", "values").(object)
			if toleratesVolatile(at(values, "template", "spec").(object)["tolerations"]) {
				t.Errorf("%s tolerates volatile workers", values["runnerScaleSetName"])
			}
		}
	}
}

func TestRustPoolsRunOnTheSharedWorker(t *testing.T) {
	t.Parallel()
	const sharedWorkerHeadroom = "850m"
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/policy/runtime.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	overhead := map[string]object{}
	for _, runtime := range yamlObjects(t, data) {
		overhead[at(runtime, "metadata", "name").(string)] = at(runtime, "overhead", "podFixed").(object)
	}
	placed := map[string]bool{}
	for _, resource := range rendered(t, "platform/components/runners/nsql") {
		if resource["kind"] != "HelmRelease" {
			continue
		}
		values := at(resource, "spec", "values").(object)
		name := values["runnerScaleSetName"].(string)
		spec := at(values, "template", "spec").(object)
		if toleratesVolatile(spec["tolerations"]) {
			t.Errorf("%s tolerates volatile workers", name)
		}
		if name != "rust-amd64" && name != "rust-pr-amd64" {
			continue
		}
		placed[name] = true
		affinity, _ := spec["affinity"].(object)
		nodeAffinity, _ := affinity["nodeAffinity"].(object)
		if _, required := nodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"]; required {
			t.Errorf("%s requires a node selection instead of preferring the shared worker", name)
		}
		if shared, kata := placementScore(nodeAffinity, gvisorWorker), placementScore(nodeAffinity, gvisorWorker, kataWorker); shared <= kata {
			t.Errorf("%s scores the shared worker %d and kata workers %d; want the shared worker first", name, shared, kata)
		}
		reserved := quantity(t, overhead[spec["runtimeClassName"].(string)]["cpu"])
		for _, container := range spec["containers"].([]any) {
			reserved.Add(reserved, quantity(t, at(container, "resources", "requests", "cpu")))
		}
		if reserved.Cmp(quantity(t, sharedWorkerHeadroom)) > 0 {
			t.Errorf("%s reserves %s CPU with its sandbox, more than the shared worker headroom of %s", name, reserved.FloatString(3), sharedWorkerHeadroom)
		}
	}
	for _, name := range []string{"rust-amd64", "rust-pr-amd64"} {
		if !placed[name] {
			t.Errorf("Rust pool %s not rendered", name)
		}
	}
}
