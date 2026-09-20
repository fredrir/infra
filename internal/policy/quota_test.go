package policy

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func quantity(t *testing.T, value any) *big.Rat {
	t.Helper()
	q, e := resource.ParseQuantity(fmt.Sprint(value))
	if e != nil {
		t.Fatal(e)
	}
	r, ok := new(big.Rat).SetString(q.AsDec().String())
	if !ok {
		t.Fatal("invalid parsed quantity")
	}
	return r
}

func TestEveryRunnerOverlayQuotaIncludesRuntimeOverhead(t *testing.T) {
	data, e := os.ReadFile(filepath.Join(repoRoot(t), "platform/components/policy/runtime.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	overhead := map[string]object{}
	for _, r := range yamlObjects(t, data) {
		overhead[at(r, "metadata", "name").(string)] = at(r, "overhead", "podFixed").(object)
	}
	overlays := at(load(t, "platform/components/runners/kustomization.yaml"), "resources").([]any)
	hasInfra := false
	for _, overlay := range overlays {
		if overlay == "infra" {
			hasInfra = true
		}
		t.Run(overlay.(string), func(t *testing.T) {
			resources := rendered(t, "platform/components/runners/"+overlay.(string))
			var quota object
			var releases []object
			for _, r := range resources {
				switch r["kind"] {
				case "ResourceQuota":
					if quota != nil {
						t.Fatal("multiple runner quotas")
					}
					quota = at(r, "spec", "hard").(object)
				case "HelmRelease":
					releases = append(releases, at(r, "spec", "values").(object))
				}
			}
			if quota == nil || len(releases) == 0 {
				t.Fatal("runner quota or releases missing")
			}
			pods := new(big.Rat)
			for _, r := range releases {
				pods.Add(pods, quantity(t, r["maxRunners"]))
			}
			if quantity(t, quota["pods"]).Cmp(pods) < 0 {
				t.Fatal("pod quota insufficient")
			}
			for _, field := range []string{"requests", "limits"} {
				for _, res := range []string{"cpu", "memory"} {
					required := new(big.Rat)
					for _, r := range releases {
						runtime := at(r, "template", "spec", "runtimeClassName").(string)
						cost := quantity(t, overhead[runtime][res])
						for _, c := range at(r, "template", "spec", "containers").([]any) {
							cost.Add(cost, quantity(t, at(c, "resources", field, res)))
						}
						cost.Mul(cost, quantity(t, r["maxRunners"]))
						required.Add(required, cost)
					}
					if quantity(t, quota[field+"."+res]).Cmp(required) < 0 {
						t.Fatalf("%s.%s quota %v below %s", field, res, quota[field+"."+res], required.RatString())
					}
				}
			}
			for _, r := range releases {
				for _, c := range at(r, "template", "spec", "containers").([]any) {
					slot := at(c, "resources", "limits").(object)["infra.fredrir.com/ci-slot"]
					if r["runnerScaleSetName"] == "deploy-amd64" {
						if slot != nil {
							t.Fatal("deploy pool consumes worker slot")
						}
					} else if slot != "1" {
						t.Fatalf("runner must hold exactly one slot: %v", slot)
					}
				}
			}
			if _, ok := quota["requests.infra.fredrir.com/ci-slot"]; ok {
				t.Fatal("extended-resource quota unexpectedly duplicates node capacity")
			}
		})
	}
	if !hasInfra {
		t.Fatal("infra runner overlay missing")
	}
}
