package dev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

const fixtureRoot = `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: platform-policy
  namespace: flux-system
spec:
  interval: 10m
  path: ./platform/components/policy
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  decryption:
    provider: sops
    secretRef:
      name: sops-age
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: platform-projects
  namespace: flux-system
spec:
  interval: 10m
  path: ./platform/projects
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  dependsOn:
  - name: platform-policy
  - name: platform-sources
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: platform-controllers
  namespace: flux-system
spec:
  path: ./platform/components/controllers
  sourceRef:
    kind: GitRepository
    name: flux-system
`

func clusterRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, clusterKustomizations), fixtureRoot)
	writeFile(t, filepath.Join(root, settingsFile), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: platform-settings\n  namespace: flux-system\ndata:\n  STORAGE_CLASS: local-retain\n  GRAFANA_HOST: grafana.example.test\n")
	writeFile(t, filepath.Join(root, clusterPatchesFile), "- target:\n    kind: Deployment\n  patch: |\n    - op: add\n      path: /spec/replicas\n      value: 0\n")
	writeFile(t, filepath.Join(root, clusterConfigFile), "apiVersion: k3d.io/v1alpha5\nkind: Simple\n")
	writeFile(t, filepath.Join(root, fluxComponentsFile), "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: flux-system\n")
	writeFile(t, filepath.Join(root, "platform/projects/example/kustomization.yaml"), "resources:\n- app.yaml\n- app.secret.sops.yaml\n- pull.secret.sops.yaml\n")
	writeFile(t, filepath.Join(root, "platform/projects/example/app.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n")
	writeFile(t, filepath.Join(root, "platform/projects/example/app.secret.sops.yaml"), "apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: example\n  labels:\n    app: example\ntype: Opaque\nstringData:\n  DATABASE_URL: ENC[AES256_GCM,data:abc,type:str]\n  TOKEN: ENC[AES256_GCM,data:def,type:str]\nsops:\n  age:\n  - recipient: age1production\n  version: 3.13.3\n")
	writeFile(t, filepath.Join(root, "platform/projects/example/pull.secret.sops.yaml"), "apiVersion: v1\nkind: Secret\nmetadata:\n  name: ghcr\n  namespace: example\ntype: kubernetes.io/dockerconfigjson\ndata:\n  .dockerconfigjson: ENC[AES256_GCM,data:ghi,type:str]\nsops:\n  version: 3.13.3\n")
	writeFile(t, filepath.Join(root, clusterSecretsDir, "platform/projects/example/pull.secret.sops.yaml"), "apiVersion: v1\nkind: Secret\nmetadata:\n  name: ghcr\n  namespace: example\ntype: kubernetes.io/dockerconfigjson\nstringData:\n  .dockerconfigjson: '{\"auths\":{\"ghcr.io\":{\"auth\":\"ZGV2OmRldg==\"}}}'\n")
	return root
}

type clusterFake struct {
	t        *testing.T
	root     string
	exists   bool
	running  int
	commands [][]string
	stdin    map[string]string
	ready    bool
}

func (f *clusterFake) runner() ci.Runner {
	return ci.Runner{Execute: func(_ context.Context, options process.Options) (process.Result, error) {
		if options.Dir != f.root {
			f.t.Errorf("%s ran outside the repository: %s", options.Name, options.Dir)
		}
		command := append([]string{filepath.Base(options.Name)}, options.Args...)
		f.commands = append(f.commands, command)
		joined := strings.Join(command, " ")
		if options.Stdin != nil {
			data, _ := io.ReadAll(options.Stdin)
			f.stdin[joined] = string(data)
		}
		if strings.HasPrefix(joined, "kubectl") || strings.HasPrefix(joined, "flux") {
			if !slices.ContainsFunc(options.Env, func(entry string) bool {
				return entry == "KUBECONFIG="+filepath.Join(f.root, ".cache/dev/cluster/kubeconfig")
			}) {
				f.t.Errorf("%s ran without the cluster kubeconfig", joined)
			}
		}
		switch {
		case joined == "k3d cluster list --output json":
			if !f.exists {
				return process.Result{Stdout: []byte("[]")}, nil
			}
			return process.Result{Stdout: []byte(fmt.Sprintf(`[{"name":"infra-dev","serversRunning":%d,"serversCount":1}]`, f.running))}, nil
		case strings.HasPrefix(joined, "k3d cluster create"):
			f.exists, f.running = true, 1
		case strings.HasPrefix(joined, "k3d cluster start"):
			f.running = 1
		case strings.HasPrefix(joined, "k3d cluster delete"):
			f.exists = false
		case joined == "k3d kubeconfig get infra-dev":
			return process.Result{Stdout: []byte("apiVersion: v1\nkind: Config\n")}, nil
		case strings.HasPrefix(joined, "age-keygen -o "):
			if err := os.WriteFile(command[2], []byte("AGE-SECRET-KEY-1DEV\n"), 0o600); err != nil {
				f.t.Fatal(err)
			}
		case strings.HasPrefix(joined, "age-keygen -y "):
			return process.Result{Stdout: []byte("age1devrecipient\n")}, nil
		case strings.HasPrefix(joined, "sops "):
			path := command[len(command)-1]
			data, err := os.ReadFile(path)
			if err != nil {
				f.t.Fatal(err)
			}
			if strings.Contains(string(data), "ENC[") {
				f.t.Errorf("sops received still-encrypted input %s", path)
			}
			if err := os.WriteFile(path, append(data, "sops:\n  age:\n  - recipient: age1devrecipient\n"...), 0o600); err != nil {
				f.t.Fatal(err)
			}
		case joined == "git rev-parse HEAD":
			return process.Result{Stdout: []byte(strings.Repeat("c", 40) + "\n")}, nil
		case joined == "git remote get-url origin":
			return process.Result{Stdout: []byte("git@github.com:fredrir/infra.git\n")}, nil
		case strings.HasPrefix(joined, "kubectl wait kustomization/") && !f.ready:
			return process.Result{ExitCode: 1}, errors.New("kubectl failed: exit status 1")
		case strings.HasPrefix(joined, "kubectl get kustomizations"):
			condition := "True"
			if !f.ready {
				condition = "False"
			}
			return process.Result{Stdout: []byte(`{"items":[{"metadata":{"name":"platform-policy"},"status":{"lastAppliedRevision":"dev@sha1:` + strings.Repeat("c", 40) + `","conditions":[{"type":"Ready","status":"True","message":"Applied revision"}]}},{"metadata":{"name":"platform-projects"},"status":{"conditions":[{"type":"Ready","status":"` + condition + `","message":"health check"}]}}]}`)}, nil
		}
		return process.Result{}, nil
	}}
}

func (f *clusterFake) count(prefix string) int {
	total := 0
	for _, command := range f.commands {
		if strings.HasPrefix(strings.Join(command, " "), prefix) {
			total++
		}
	}
	return total
}

func TestClusterUpCreatesClusterInstallsFluxAndReconcilesArtifact(t *testing.T) {
	root := clusterRoot(t)
	fake := &clusterFake{t: t, root: root, stdin: map[string]string{}, ready: true}
	var log strings.Builder
	status, err := ClusterUp(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner(), Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.Revision != strings.Repeat("c", 40) || len(status.Kustomizations) != 2 || !status.Kustomizations[1].Ready {
		t.Fatalf("unexpected status %+v", status)
	}
	config, _ := filepath.Abs(filepath.Join(root, clusterConfigFile))
	for _, expected := range []string{"k3d cluster create --config " + config, "k3d kubeconfig get infra-dev", "kubectl apply --server-side --force-conflicts -f " + filepath.Join(root, fluxComponentsFile), "kubectl wait --for=condition=Established", "kubectl rollout status deployment/source-controller", "kubectl rollout status deployment/kustomize-controller", "kubectl rollout status deployment/helm-controller", "age-keygen -o " + filepath.Join(root, ".cache/dev/cluster/age.key"), "kubectl apply --server-side --force-conflicts -f -", "flux push artifact oci://127.0.0.1:5111/platform:dev --path=" + filepath.Join(root, ".cache/dev/cluster/artifact") + " --source=git@github.com:fredrir/infra.git --revision=dev@sha1:" + strings.Repeat("c", 40) + " --insecure-registry --provider=generic", "kubectl apply --server-side --force-conflicts -f " + filepath.Join(root, ".cache/dev/cluster/root.yaml"), "flux reconcile source oci platform-dev", "kubectl wait kustomization/platform-policy --namespace=flux-system --for=condition=Ready", "kubectl wait kustomization/platform-projects", "kubectl get kustomizations.kustomize.toolkit.fluxcd.io --namespace=flux-system --output=json"} {
		if fake.count(expected) != 1 {
			t.Errorf("expected exactly one %q, got %d in %v", expected, fake.count(expected), fake.commands)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, ".cache/dev/cluster/kubeconfig")); err != nil || !strings.Contains(string(data), "kind: Config") {
		t.Fatalf("kubeconfig not written: %v", err)
	}
	secret := fake.stdin["kubectl apply --server-side --force-conflicts -f -"]
	if !strings.Contains(secret, "name: sops-age") || !strings.Contains(secret, "age.agekey: "+"QUdFLVNFQ1JFVC1LRVktMURFVgo=") {
		t.Fatalf("dev decryption key not installed:\n%s", secret)
	}
	if fake.count("sops --config "+filepath.Join(root, ".cache/dev/cluster/sops.yaml")+" --encrypt --in-place ") != 2 {
		t.Fatalf("expected two encryptions: %v", fake.commands)
	}
	artifact := filepath.Join(root, ".cache/dev/cluster/artifact")
	synthesized, err := os.ReadFile(filepath.Join(artifact, "platform/projects/example/app.secret.sops.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"name: app", "namespace: example", "app: example", "DATABASE_URL: dev", "TOKEN: dev", "type: Opaque", "recipient: age1devrecipient"} {
		if !strings.Contains(string(synthesized), fragment) {
			t.Errorf("synthesized secret lacks %q:\n%s", fragment, synthesized)
		}
	}
	if strings.Contains(string(synthesized), "age1production") || strings.Contains(string(synthesized), "ENC[") {
		t.Fatalf("synthesized secret retained production material:\n%s", synthesized)
	}
	overridden, err := os.ReadFile(filepath.Join(artifact, "platform/projects/example/pull.secret.sops.yaml"))
	if err != nil || !strings.Contains(string(overridden), "ZGV2OmRldg==") {
		t.Fatalf("override not used: %v\n%s", err, overridden)
	}
	if _, err := os.Stat(filepath.Join(artifact, "platform/projects/example/app.yaml")); err != nil {
		t.Fatal("plain manifests not copied:", err)
	}
	rules, err := os.ReadFile(filepath.Join(root, ".cache/dev/cluster/sops.yaml"))
	if err != nil || !strings.Contains(string(rules), "age: age1devrecipient") || !strings.Contains(string(rules), "encrypted_regex: ^(data|stringData)$") {
		t.Fatalf("sops rules: %v\n%s", err, rules)
	}
	if !strings.Contains(log.String(), "Created: infra-dev") {
		t.Fatal("creation not logged")
	}
	fake.commands = nil
	if _, err := ClusterUp(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner()}); err != nil || fake.count("k3d cluster create") != 0 || fake.count("age-keygen -o") != 0 {
		t.Fatalf("existing cluster or key recreated: %v %v", err, fake.commands)
	}
	fake.running = 0
	if _, err := ClusterUp(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner()}); err != nil || fake.count("k3d cluster start infra-dev") != 1 {
		t.Fatalf("stopped cluster not started: %v", err)
	}
}

func TestClusterSyncReportsUnreadyKustomizations(t *testing.T) {
	root := clusterRoot(t)
	fake := &clusterFake{t: t, root: root, stdin: map[string]string{}, exists: true, running: 1}
	if _, err := ClusterSync(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner()}); err == nil || !strings.Contains(err.Error(), "kubeconfig missing") {
		t.Fatalf("sync without kubeconfig accepted: %v", err)
	}
	writeFile(t, filepath.Join(root, ".cache/dev/cluster/kubeconfig"), "kind: Config\n")
	status, err := ClusterSync(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner(), Profile: "minimal"})
	if !errors.Is(err, ErrClusterNotReady) || len(status.Kustomizations) != 2 || status.Kustomizations[1].Ready || status.Kustomizations[1].Message != "health check" {
		t.Fatalf("unready kustomization not reported: %v %+v", err, status)
	}
	if _, err := ClusterSync(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner(), Profile: "everything"}); err == nil {
		t.Fatal("unknown profile accepted")
	}
	fake.exists = false
	if _, err := ClusterSync(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner()}); err == nil || !strings.Contains(err.Error(), "cluster up") {
		t.Fatalf("sync without cluster accepted: %v", err)
	}
}

func TestGenerateRootRetargetsSelectedKustomizations(t *testing.T) {
	root := clusterRoot(t)
	data, err := generateRoot(root, ClusterProfiles["minimal"])
	if err != nil {
		t.Fatal(err)
	}
	var documents []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, document)
	}
	if len(documents) != 4 {
		t.Fatalf("expected source, settings and two kustomizations, got %d", len(documents))
	}
	source := documents[0]["spec"].(map[string]any)
	if documents[0]["kind"] != "OCIRepository" || source["url"] != "oci://infra-dev-registry:5000/platform" || source["insecure"] != true || source["ref"].(map[string]any)["tag"] != "dev" {
		t.Fatalf("unexpected source %+v", documents[0])
	}
	settings := documents[1]["data"].(map[string]any)
	if settings["STORAGE_CLASS"] != "local-path" || settings["GRAFANA_HOST"] != "grafana.example.test" {
		t.Fatalf("unexpected settings %+v", settings)
	}
	projects := documents[3]["spec"].(map[string]any)
	if documents[3]["metadata"].(map[string]any)["name"] != "platform-projects" || projects["sourceRef"].(map[string]any)["kind"] != "OCIRepository" || projects["sourceRef"].(map[string]any)["name"] != "platform-dev" || projects["interval"] != "1m" || projects["wait"] != false || projects["timeout"] != "2m" {
		t.Fatalf("projects not retargeted: %+v", projects)
	}
	if dependencies := projects["dependsOn"].([]any); len(dependencies) != 1 || dependencies[0].(map[string]any)["name"] != "platform-policy" {
		t.Fatalf("unselected dependency retained: %+v", projects["dependsOn"])
	}
	if patches := projects["patches"].([]any); len(patches) != 1 || !strings.Contains(patches[0].(map[string]any)["patch"].(string), "/spec/replicas") {
		t.Fatalf("dev patches missing: %+v", projects["patches"])
	}
	policy := documents[2]["spec"].(map[string]any)
	if _, ok := policy["dependsOn"]; ok {
		t.Fatalf("empty dependsOn retained: %+v", policy)
	}
	if policy["decryption"].(map[string]any)["secretRef"].(map[string]any)["name"] != "sops-age" {
		t.Fatalf("decryption changed: %+v", policy)
	}
	if _, err := generateRoot(root, []string{"platform-missing"}); err == nil {
		t.Fatal("missing kustomization accepted")
	}
}

func TestSynthesizeSecretRejectsNonSecrets(t *testing.T) {
	if _, err := synthesizeSecret([]byte("kind: ConfigMap\nmetadata:\n  name: x\ndata:\n  a: b\n")); err == nil {
		t.Fatal("ConfigMap synthesized")
	}
	if _, err := synthesizeSecret([]byte("kind: Secret\nmetadata:\n  name: x\n")); err == nil {
		t.Fatal("keyless secret synthesized")
	}
	data, err := synthesizeSecret([]byte("kind: Secret\nimmutable: true\nmetadata:\n  name: x\ndata:\n  b: ENC\nstringData:\n  a: ENC\n"))
	if err != nil {
		t.Fatal(err)
	}
	var secret map[string]any
	if err := yaml.Unmarshal(data, &secret); err != nil {
		t.Fatal(err)
	}
	if secret["immutable"] != true || secret["type"] != "Opaque" || secret["data"] != nil || len(secret["stringData"].(map[string]any)) != 2 {
		t.Fatalf("unexpected synthesis %s", data)
	}
}

func TestClusterDownDeletesClusterAndKubeconfig(t *testing.T) {
	root := clusterRoot(t)
	fake := &clusterFake{t: t, root: root, stdin: map[string]string{}, exists: true, running: 1}
	writeFile(t, filepath.Join(root, ".cache/dev/cluster/kubeconfig"), "kind: Config\n")
	status, err := InspectCluster(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner()})
	if err != nil || !status.Running || len(status.Kustomizations) != 2 {
		t.Fatalf("running cluster not inspected: %v %+v", err, status)
	}
	if err := ClusterDown(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner()}); err != nil {
		t.Fatal(err)
	}
	if fake.exists || fake.count("k3d cluster delete infra-dev") != 1 {
		t.Fatalf("cluster not deleted: %v", fake.commands)
	}
	if _, err := os.Stat(filepath.Join(root, ".cache/dev/cluster/kubeconfig")); !os.IsNotExist(err) {
		t.Fatal("kubeconfig retained")
	}
	if err := ClusterDown(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner()}); err != nil || fake.count("k3d cluster delete") != 1 {
		t.Fatalf("absent cluster deleted again: %v", err)
	}
	status, err = InspectCluster(context.Background(), ClusterOptions{State: NewState(root), Runner: fake.runner()})
	if err != nil || status.Running || status.Kubeconfig != "" {
		t.Fatalf("absent cluster reported as %+v, %v", status, err)
	}
	_ = json.Valid
}
