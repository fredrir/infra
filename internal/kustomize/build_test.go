package kustomize

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		filename := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestBuildMatchesKubectlLegacyOrderComponentsAndGenerators(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"overlay/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: example\nresources:\n- deployment.yaml\n- namespace.yaml\ncomponents:\n- ../labels\nconfigMapGenerator:\n- name: settings\n  literals:\n  - mode=validate\n",
		"overlay/deployment.yaml":    "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n      containers:\n      - name: web\n        image: web\n        envFrom:\n        - configMapRef:\n            name: settings\n",
		"overlay/namespace.yaml":     "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: example\n",
		"labels/kustomization.yaml":  "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nlabels:\n- pairs:\n    team: platform\n",
	})
	rendered, err := Build(filepath.Join(root, "overlay"))
	if err != nil {
		t.Fatal(err)
	}
	want := `apiVersion: v1
kind: Namespace
metadata:
  labels:
    team: platform
  name: example
---
apiVersion: v1
data:
  mode: validate
kind: ConfigMap
metadata:
  labels:
    team: platform
  name: settings-cc752h9fg9
  namespace: example
---
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    team: platform
  name: web
  namespace: example
spec:
  template:
    spec:
      containers:
      - envFrom:
        - configMapRef:
            name: settings-cc752h9fg9
        image: web
        name: web
`
	if string(rendered) != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", rendered, want)
	}
}

func TestBuildRejectsHelmChartsAndFilesOutsideTheRoot(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"chart/kustomization.yaml":  "helmCharts:\n- name: chart\n  repo: https://example.invalid\n  version: 1.0.0\n",
		"escape/kustomization.yaml": "resources:\n- ../outside.yaml\n",
		"outside.yaml":              "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: outside\n",
	})
	for directory, message := range map[string]string{"chart": "must specify --enable-helm", "escape": "is not in or below"} {
		if _, err := Build(filepath.Join(root, directory)); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%s: error %v lacks %q", directory, err, message)
		}
	}
}

func TestBuildMatchesKubectlOnRepositoryOverlays(t *testing.T) {
	root := os.Getenv("INFRA_TEST_SOURCE_ROOT")
	if os.Getenv("INFRA_KUSTOMIZE_QUALIFY") != "1" || root == "" {
		t.Skip("set INFRA_KUSTOMIZE_QUALIFY=1 and INFRA_TEST_SOURCE_ROOT")
	}
	listed, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "kustomization.yaml", "**/kustomization.yaml").Output()
	if err != nil {
		t.Fatal(err)
	}
	directories := strings.Split(strings.TrimRight(string(listed), "\x00"), "\x00")
	if len(directories) < 10 {
		t.Fatalf("found only %d kustomizations", len(directories))
	}
	for _, path := range directories {
		directory := filepath.Join(root, filepath.Dir(path))
		t.Run(filepath.Dir(path), func(t *testing.T) {
			want, kubectlErr := exec.Command("kubectl", "kustomize", directory).Output()
			rendered, err := Build(directory)
			if (kubectlErr != nil) != (err != nil) {
				t.Fatalf("kubectl error %v, in-process error %v", kubectlErr, err)
			}
			if !bytes.Equal(rendered, want) {
				t.Fatalf("in-process render differs from kubectl kustomize (%d and %d bytes)", len(rendered), len(want))
			}
		})
	}
}
