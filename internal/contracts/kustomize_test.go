package contracts

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/fredrir/infra/internal/ci"
)

type kustomizeModules struct{ API, KYAML string }

// Versions come from the k8s.io/kubectl and sigs.k8s.io/kustomize/kustomize/v5 go.mod of each release.
var embeddedKustomize = map[string]kustomizeModules{
	"kubectl v1.36.3":  {API: "v0.21.1", KYAML: "v0.21.1"},
	"kubectl v1.36.4":  {API: "v0.21.1", KYAML: "v0.21.1"},
	"kustomize v5.8.1": {API: "v0.21.1", KYAML: "v0.21.1"},
}

func TestInProcessKustomizeMatchesPinnedBinaries(t *testing.T) {
	repository := root(t)
	module := string(read(t, filepath.Join(repository, "go.mod")))
	required := func(path string) string {
		match := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(path) + `\s+(v\S+)`).FindStringSubmatch(module)
		if match == nil {
			t.Fatalf("go.mod does not require %s", path)
		}
		return match[1]
	}
	linked := kustomizeModules{API: required("sigs.k8s.io/kustomize/api"), KYAML: required("sigs.k8s.io/kustomize/kyaml")}
	containerfile := string(read(t, filepath.Join(repository, "images/runner-check/Containerfile")))
	pins := map[string]string{}
	for source, pin := range map[string]struct{ tool, text, pattern string }{
		"runner-check kubectl":   {"kubectl", containerfile, `kubernetes/kubernetes/tar\.gz/refs/tags/(v[0-9.]+)`},
		"runner-check kustomize": {"kustomize", containerfile, `kubernetes-sigs/kustomize/tar\.gz/refs/tags/kustomize/(v[0-9.]+)`},
		"CI kubectl":             {"kubectl", toolURL(t, "kubectl"), `/release/(v[0-9.]+)/`},
		"CI kustomize":           {"kustomize", toolURL(t, "kustomize"), `/download/kustomize/(v[0-9.]+)/`},
	} {
		match := regexp.MustCompile(pin.pattern).FindStringSubmatch(pin.text)
		if match == nil {
			t.Fatalf("%s pin not found", source)
		}
		pins[source] = pin.tool + " " + match[1]
	}
	pinned := map[string]bool{}
	for source, release := range pins {
		pinned[release] = true
		embedded, known := embeddedKustomize[release]
		if !known {
			t.Errorf("%s pins %s; record its embedded kustomize api and kyaml versions", source, release)
		} else if embedded != linked {
			t.Errorf("%s pins %s embedding kustomize api %s and kyaml %s; go.mod links api %s and kyaml %s", source, release, embedded.API, embedded.KYAML, linked.API, linked.KYAML)
		}
	}
	for release := range embeddedKustomize {
		if !pinned[release] {
			t.Errorf("%s is no longer pinned; remove it", release)
		}
	}
}

func toolURL(t *testing.T, name string) string {
	t.Helper()
	asset, ok := ci.Tool(name)
	if !ok {
		t.Fatalf("%s is not a pinned CI tool", name)
	}
	return asset.URL
}
