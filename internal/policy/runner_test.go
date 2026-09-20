package policy

import (
	"fmt"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestRunnerAdmissionBoundaries(t *testing.T) {
	e := newEvaluator(t)
	base := runner(t, "base/buildkit.yaml")
	if !e.runner(base, "ci-y", controller) {
		t.Fatal("qualified runner rejected")
	}
	for name, value := range (object{"hostNetwork": true, "automountServiceAccountToken": true, "runtimeClassName": "runc", "activeDeadlineSeconds": 86400}) {
		t.Run(name, func(t *testing.T) {
			p := clone(base).(object)
			set(p, value, "spec", name)
			if e.runner(p, "ci-y", controller) {
				t.Fatal("unsafe runner admitted")
			}
		})
	}
	for _, mutate := range []func(object){func(p object) { appendAt(p, clone(at(p, "spec", "containers", 0)), "spec", "containers") }, func(p object) {
		set(p, []any{object{"name": "credentials", "secret": object{"secretName": "github-app"}}}, "spec", "volumes")
	}} {
		p := clone(base).(object)
		mutate(p)
		if e.runner(p, "ci-y", controller) {
			t.Fatal("sidecar or secret volume admitted")
		}
	}
}

func TestRunnerJITTokenIsBoundToControllerAndOwnPod(t *testing.T) {
	e := newEvaluator(t)
	p := runner(t, "base/buildkit.yaml")
	secret := object{"name": "ACTIONS_RUNNER_INPUT_JITCONFIG", "valueFrom": object{"secretKeyRef": object{"name": "runner-1", "key": "jitToken"}}}
	appendAt(p, secret, "spec", "containers", 0, "env")
	if !e.runner(p, "ci-y", controller) || e.runner(p, "ci-y", "untrusted") {
		t.Fatal("JIT controller boundary")
	}
	set(secret, "github-app", "valueFrom", "secretKeyRef", "name")
	if e.runner(p, "ci-y", controller) {
		t.Fatal("foreign JIT token admitted")
	}
}

func TestAtticCredentialsRemainBoundToQualifiedLegacyPool(t *testing.T) {
	e := newEvaluator(t)
	p := runner(t, "infra/nix.yaml")
	if !e.runner(p, "ci-infra", controller) || e.runner(p, "ci-y", controller) || e.runner(p, "ci-infra", "untrusted") {
		t.Fatal("legacy credential boundary")
	}
}

func TestRunnerSlotQuantitiesAreExact(t *testing.T) {
	base := runner(t, "base/buildkit.yaml")
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
			if e.runner(p, "ci-y", controller) != tc.allowed {
				t.Fatal("incorrect slot admission")
			}
		})
	}
}

func TestCheckAndDeployPoolsRemainBounded(t *testing.T) {
	e := newEvaluator(t)
	check := runner(t, "infra/check.yaml")
	check["metadata"] = object{"name": "check-1", "labels": object{"actions.github.com/scale-set-name": "check-amd64"}}
	if !e.runner(check, "ci-infra", controller) {
		t.Fatal("approved check pool rejected")
	}
	set(check, "ghcr.io/fredrir/infra-runner-check@sha256:"+strings.Repeat("f", 64), "spec", "containers", 0, "image")
	if e.runner(check, "ci-infra", controller) {
		t.Fatal("unapproved image admitted")
	}
	deploy := runner(t, "infra/deploy.yaml")
	deploy["metadata"] = object{"name": "deploy-1", "labels": object{"actions.github.com/scale-set-name": "deploy-amd64"}}
	if !e.runner(deploy, "ci-infra", controller) || e.runner(deploy, "ci-y", controller) {
		t.Fatal("deploy namespace boundary")
	}
	for _, mutate := range []func(object){func(p object) { set(p, "buildkit-amd64", "metadata", "labels", "actions.github.com/scale-set-name") }, func(p object) { set(p, "4", "spec", "containers", 0, "resources", "limits", "cpu") }, func(p object) { set(p, "8Gi", "spec", "containers", 0, "resources", "limits", "memory") }} {
		p := clone(deploy).(object)
		mutate(p)
		if e.runner(p, "ci-infra", controller) {
			t.Fatal("unbounded slot-free deploy admitted")
		}
	}
}

func TestRustCacheCredentialsStayWithinPool(t *testing.T) {
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
	buildkitImage := at(runner(t, "base/buildkit.yaml"), "spec", "containers", 0, "image")
	for _, mutate := range []func(object){func(p object) { set(p, object{}, "metadata", "labels") }, func(p object) { set(p, "buildkit-amd64", "metadata", "labels", "actions.github.com/scale-set-name") }, func(p object) { set(p, buildkitImage, "spec", "containers", 0, "image") }, func(p object) {
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

func TestLegacyBuildkitCacheCredentialsStayWithinPool(t *testing.T) {
	e := newEvaluator(t)
	base := runner(t, "base/buildkit.yaml")
	set(base, object{"actions.github.com/scale-set-name": "buildkit-amd64"}, "metadata", "labels")
	component := load(t, "platform/components/runners/buildkit-cache/kustomization.yaml")
	var patches []object
	if err := yaml.Unmarshal([]byte(at(component, "patches", 0, "patch").(string)), &patches); err != nil {
		t.Fatal(err)
	}
	for _, patch := range patches {
		appendAt(base, patch["value"], "spec", "containers", 0, "env")
	}
	if !e.runner(base, "ci-y", controller) || e.runner(base, "ci-y", "untrusted") {
		t.Fatal("build cache controller boundary")
	}
	rustImage := at(rustRunner(t, "main"), "spec", "containers", 0, "image")
	last := len(at(base, "spec", "containers", 0, "env").([]any)) - 1
	for _, mutate := range []func(object){func(p object) { set(p, object{}, "metadata", "labels") }, func(p object) { set(p, "publish-amd64", "metadata", "labels", "actions.github.com/scale-set-name") }, func(p object) { set(p, rustImage, "spec", "containers", 0, "image") }, func(p object) {
		set(p, "sccache-rw", "spec", "containers", 0, "env", last, "valueFrom", "secretKeyRef", "name")
	}, func(p object) {
		set(p, "AWS_ACCESS_KEY_ID", "spec", "containers", 0, "env", last, "valueFrom", "secretKeyRef", "key")
	}, func(p object) {
		appendAt(p, object{"name": "GITHUB_TOKEN", "valueFrom": object{"secretKeyRef": object{"name": "buildkit-cache", "key": "AWS_ACCESS_KEY_ID"}}}, "spec", "containers", 0, "env")
	}} {
		p := clone(base).(object)
		mutate(p)
		if e.runner(p, "ci-y", controller) {
			t.Fatal("build cache credential boundary bypass")
		}
	}
}
