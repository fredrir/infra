package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type object = map[string]any

const k3sImage = "rancher/k3s@sha256:d0f79175794edd9694b4a12bafc5c52ae1977369a2f7cf256264e7bd2dae0be9"

type fixture struct {
	root, output, cluster, config string
	evidence                      object
}

func main() {
	root := flag.String("root", ".", "Repository root")
	cluster := flag.String("cluster", fmt.Sprintf("reconcile-%d", os.Getpid()), "Disposable cluster name")
	output := flag.String("output", "/tmp/infra-reconcile-flux-test", "Evidence directory")
	reuse := flag.Bool("reuse", false, "Reuse the named disposable cluster")
	keep := flag.Bool("keep", false, "Keep the disposable cluster")
	flag.Parse()
	if err := execute(*root, *cluster, *output, *reuse, *keep); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(input []byte, directory string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = directory
	cmd.Stdin = bytes.NewReader(input)
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	output, err := cmd.Output()
	if err != nil {
		return output, fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, diagnostics.String())
	}
	return output, nil
}

func run(args ...string) ([]byte, error) { return command(nil, "", args...) }

func (f fixture) kubectl(args ...string) ([]byte, error) {
	return run(append([]string{"kubectl", "--kubeconfig=" + f.config}, args...)...)
}

func (f fixture) apply(objects []object) error {
	var data bytes.Buffer
	encoder := yaml.NewEncoder(&data)
	for _, obj := range objects {
		if err := encoder.Encode(obj); err != nil {
			return err
		}
	}
	_, err := command(data.Bytes(), "", "kubectl", "--kubeconfig="+f.config, "apply", "--server-side", "--force-conflicts", "-f", "-")
	return err
}

func readObjects(path string) ([]object, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var result []object
	for {
		var value object
		if err := decoder.Decode(&value); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if value != nil {
			result = append(result, value)
		}
	}
	return result, nil
}

func at(value object, path ...string) any {
	var current any = value
	for _, key := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = mapping[key]
	}
	return current
}

func text(value object, path ...string) string {
	result, _ := at(value, path...).(string)
	return result
}

func (f fixture) get(kind, name string) (object, error) {
	data, err := f.kubectl("get", kind, name, "-n", "flux-system", "-o", "json")
	if err != nil {
		return nil, err
	}
	var value object
	err = json.Unmarshal(data, &value)
	return value, err
}

func wait(label string, check func() (bool, error)) error {
	deadline := time.Now().Add(120 * time.Second)
	var latest error
	for time.Now().Before(deadline) {
		done, err := check()
		if done && err == nil {
			return nil
		}
		latest = err
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout: %s: %v", label, latest)
}

func ready(value object) bool {
	conditions, _ := at(value, "status", "conditions").([]any)
	for _, condition := range conditions {
		c := condition.(map[string]any)
		if c["type"] == "Ready" && c["status"] == "True" {
			return true
		}
	}
	return false
}

func (f fixture) record(name string, resources ...object) error {
	for _, resource := range resources {
		if text(resource, "metadata", "name") == "" {
			return fmt.Errorf("missing resource evidence for %s", name)
		}
	}
	f.evidence["checks"] = append(f.evidence["checks"].([]object), object{"name": name, "passed": true})
	f.evidence["snapshots"].(object)[name] = resources
	fmt.Println("PASS", name)
	return f.save()
}

func (f fixture) save() error {
	data, err := json.MarshalIndent(f.evidence, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.output, "evidence.json"), append(data, '\n'), 0644)
}

func execute(root, cluster, output string, reuse, keep bool) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(cluster, "reconcile-") {
		return fmt.Errorf("test cluster name must start with reconcile-")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	f := fixture{root: root, output: output, cluster: cluster, config: filepath.Join(output, "kubeconfig"), evidence: object{"schema": 1, "cluster": cluster, "k3s_image": k3sImage, "checks": []object{}, "snapshots": object{}, "passed": false}}
	if !reuse {
		if _, err := run("k3d", "cluster", "create", cluster, "--image", k3sImage, "--servers-memory", "4g", "--kubeconfig-update-default=false", "--kubeconfig-switch-context=false", "--no-lb", "--k3s-arg", "--disable=traefik,servicelb,metrics-server@server:0", "--timeout", "120s"); err != nil {
			return err
		}
	}
	defer func() {
		if !keep && !reuse {
			if _, err := run("k3d", "cluster", "delete", cluster); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
		fmt.Println("Evidence:", filepath.Join(output, "evidence.json"))
	}()
	if _, err := run("docker", "update", "--cpus", "4", "k3d-"+cluster+"-server-0"); err != nil {
		return err
	}
	config, err := run("k3d", "kubeconfig", "get", cluster)
	if err != nil {
		return err
	}
	if err := os.WriteFile(f.config, config, 0600); err != nil {
		return err
	}
	if err := f.controllers(); err != nil {
		return err
	}
	if err := f.scenarios(); err != nil {
		return err
	}
	f.evidence["passed"] = true
	return f.save()
}

func (f fixture) controllers() error {
	var objects []object
	for _, path := range []string{"platform/clusters/production/flux-system/gotk-components.yaml", "build/rollout/flux-artifacts/controller/source-watcher.yaml", "build/rollout/flux-artifacts/controller/rbac.yaml"} {
		values, err := readObjects(filepath.Join(f.root, path))
		if err != nil {
			return err
		}
		objects = append(objects, values...)
	}
	var filtered []object
	images := object{}
	for _, obj := range objects {
		if obj["kind"] == "NetworkPolicy" {
			continue
		}
		if obj["kind"] == "Deployment" {
			name := text(obj, "metadata", "name")
			if name != "source-controller" && name != "kustomize-controller" && name != "source-watcher" {
				continue
			}
			container := at(obj, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
			var args []any
			for _, arg := range container["args"].([]any) {
				if !strings.HasPrefix(arg.(string), "--events-addr=") {
					args = append(args, arg)
				}
			}
			if name == "kustomize-controller" {
				args = append(args, "--feature-gates=ExternalArtifact=true,AdditiveCELDependencyCheck=true", "--requeue-dependency=1s")
			}
			container["args"] = args
			container["resources"].(map[string]any)["limits"] = object{"cpu": "1", "memory": "512Mi"}
			images[name] = container["image"]
		}
		filtered = append(filtered, obj)
	}
	if err := f.apply(filtered); err != nil {
		return err
	}
	for _, name := range []string{"source-controller", "kustomize-controller", "source-watcher"} {
		if _, err := f.kubectl("rollout", "status", "deployment/"+name, "-n", "flux-system", "--timeout=180s"); err != nil {
			return err
		}
	}
	f.evidence["controllers"] = images
	return f.record("pinned-controllers-ready")
}

func (f fixture) scenarios() error {
	work, err := os.MkdirTemp(f.output, "flux-source-")
	if err != nil {
		return err
	}
	repo, bare := filepath.Join(work, "work"), filepath.Join(work, "source.git")
	for _, args := range [][]string{{"git", "init", "-q", "--bare", bare}, {"git", "init", "-q", "-b", "main", repo}, {"git", "-C", repo, "config", "user.name", "Flux fixture"}, {"git", "-C", repo, "config", "user.email", "fixture@localhost"}, {"git", "-C", repo, "remote", "add", "origin", bare}} {
		if _, err := run(args...); err != nil {
			return err
		}
	}
	if err := os.Mkdir(filepath.Join(repo, "selected"), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "selected/kustomization.yaml"), []byte("resources: [config.yaml]\n"), 0644); err != nil {
		return err
	}
	initialConfig := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: selected\n  namespace: flux-system\ndata:\n  value: initial\n"
	if err := os.WriteFile(filepath.Join(repo, "selected/config.yaml"), []byte(initialConfig), 0644); err != nil {
		return err
	}
	commit := func(message string) (string, error) {
		for _, args := range [][]string{{"git", "-C", repo, "add", "."}, {"git", "-C", repo, "commit", "-qm", message}, {"git", "-C", repo, "push", "-q", "origin", "HEAD:main"}} {
			if _, err := run(args...); err != nil {
				return "", err
			}
		}
		data, err := run("git", "-C", repo, "rev-parse", "HEAD")
		return strings.TrimSpace(string(data)), err
	}
	revision, err := commit("initial")
	if err != nil {
		return err
	}
	gateway, err := run("docker", "network", "inspect", "k3d-"+f.cluster, "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", strings.TrimSpace(string(gateway))+":0")
	if err != nil {
		return err
	}
	git, err := exec.LookPath("git")
	if err != nil {
		listener.Close()
		return err
	}
	server := &http.Server{Handler: &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + work, "GIT_HTTP_EXPORT_ALL=1"}}, ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	metadata := func(name string) object { return object{"name": name, "namespace": "flux-system"} }
	source := object{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository", "metadata": metadata("flux-system"), "spec": object{"interval": "1s", "url": "http://" + listener.Addr().String() + "/source.git", "ref": object{"branch": "main"}}}
	generator := object{"apiVersion": "source.extensions.fluxcd.io/v1beta1", "kind": "ArtifactGenerator", "metadata": metadata("platform-artifacts"), "spec": object{"sources": []any{object{"alias": "repo", "kind": "GitRepository", "name": "flux-system"}}, "artifacts": []any{object{"name": "project-selected", "originRevision": "@repo", "copy": []any{object{"from": "@repo/selected/**", "to": "@artifact/selected/"}}}}}}
	owner := object{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": metadata("project-selected"), "spec": object{"interval": "10m", "path": "./selected", "prune": true, "wait": false, "suspend": false, "sourceRef": object{"kind": "ExternalArtifact", "name": "project-selected"}}}
	if err := f.apply([]object{source, generator, owner}); err != nil {
		return err
	}
	origin := func(revision string) func() (bool, error) {
		return func() (bool, error) {
			artifact, err := f.get("externalartifact", "project-selected")
			return strings.HasSuffix(text(artifact, "status", "artifact", "metadata", "org.opencontainers.image.revision"), revision), err
		}
	}
	if err := wait("initial artifact", origin(revision)); err != nil {
		return err
	}
	consumed := func() (bool, error) {
		owner, err := f.get("kustomization", "project-selected")
		if err != nil {
			return false, err
		}
		artifact, err := f.get("externalartifact", "project-selected")
		return ready(owner) && text(owner, "status", "lastAppliedRevision") == text(artifact, "status", "artifact", "revision"), err
	}
	if err := wait("initial owner", consumed); err != nil {
		return err
	}
	initial, _ := f.get("externalartifact", "project-selected")
	initialGenerator, _ := f.get("artifactgenerator", "platform-artifacts")
	initialOwner, _ := f.get("kustomization", "project-selected")
	if err := f.record("initial-origin-and-owner", initial, initialGenerator, initialOwner); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("unrelated change\n"), 0644); err != nil {
		return err
	}
	revision, err = commit("unrelated")
	if err != nil {
		return err
	}
	if err := wait("unchanged artifact new origin", origin(revision)); err != nil {
		return err
	}
	unchanged, _ := f.get("externalartifact", "project-selected")
	unchangedGenerator, _ := f.get("artifactgenerator", "platform-artifacts")
	if text(unchanged, "status", "artifact", "digest") != text(initial, "status", "artifact", "digest") {
		return fmt.Errorf("unrelated inputs changed artifact digest")
	}
	if err := f.record("unchanged-content-provenance", unchanged, unchangedGenerator); err != nil {
		return err
	}
	if _, err := f.kubectl("scale", "deployment/source-watcher", "-n", "flux-system", "--replicas=0"); err != nil {
		return err
	}
	if _, err := f.kubectl("wait", "pod", "-n", "flux-system", "-l", "app=source-watcher", "--for=delete", "--timeout=60s"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "selected/config.yaml"), []byte(strings.ReplaceAll(initialConfig, "initial", "changed")), 0644); err != nil {
		return err
	}
	revision, err = commit("selected changed while generator stopped")
	if err != nil {
		return err
	}
	if err := wait("source ahead of generator", func() (bool, error) {
		source, err := f.get("gitrepository", "flux-system")
		return strings.HasSuffix(text(source, "status", "artifact", "revision"), revision), err
	}); err != nil {
		return err
	}
	stale, _ := f.get("externalartifact", "project-selected")
	staleOwner, _ := f.get("kustomization", "project-selected")
	currentSource, _ := f.get("gitrepository", "flux-system")
	if text(stale, "status", "artifact", "digest") != text(initial, "status", "artifact", "digest") || !ready(staleOwner) {
		return fmt.Errorf("generator lag fixture did not retain stale Ready owner")
	}
	if err := f.record("ready-owner-stale-artifact-during-generator-lag", stale, staleOwner, currentSource); err != nil {
		return err
	}
	if _, err := f.kubectl("scale", "deployment/source-watcher", "-n", "flux-system", "--replicas=1"); err != nil {
		return err
	}
	if err := wait("changed artifact", origin(revision)); err != nil {
		return err
	}
	if err := wait("changed owner consumed artifact", consumed); err != nil {
		return err
	}
	changed, _ := f.get("externalartifact", "project-selected")
	changedOwner, _ := f.get("kustomization", "project-selected")
	if text(changed, "status", "artifact", "digest") == text(initial, "status", "artifact", "digest") {
		return fmt.Errorf("selected change retained artifact digest")
	}
	if err := f.record("changed-content-converges", changed, changedOwner); err != nil {
		return err
	}
	token := fmt.Sprintf("fixture-%d", time.Now().UnixNano())
	if _, err := f.kubectl("annotate", "kustomization/project-selected", "-n", "flux-system", "reconcile.fluxcd.io/requestedAt="+token, "--overwrite"); err != nil {
		return err
	}
	if err := wait("request token", func() (bool, error) {
		owner, err := f.get("kustomization", "project-selected")
		return text(owner, "status", "lastHandledReconcileAt") == token, err
	}); err != nil {
		return err
	}
	requested, _ := f.get("kustomization", "project-selected")
	if err := f.record("explicit-request-token", requested); err != nil {
		return err
	}
	if err := f.workloads(repo, commit); err != nil {
		return err
	}
	return f.permissions()
}

const workloadImage = "public.ecr.aws/docker/library/alpine@sha256:d56c381f961d307a21b3ca004cf1e3910f106644aefb1f43e654c8a56c4fd395"

func writeObject(path string, value object) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (f fixture) workloads(repo string, commit func(string) (string, error)) error {
	if _, err := f.kubectl("delete", "kustomization/parser-application", "kustomization/parser-migration", "-n", "flux-system", "--ignore-not-found", "--timeout=30s"); err != nil {
		return err
	}
	if _, err := f.kubectl("delete", "job/fixture-migration", "configmap/fixture-application", "-n", "flux-system", "--ignore-not-found", "--timeout=30s"); err != nil {
		return err
	}
	if _, err := run("docker", "image", "inspect", workloadImage); err == nil {
		if _, err := run("k3d", "image", "import", workloadImage, "--cluster", f.cluster); err != nil {
			return err
		}
	}
	metadata := func(name string) object { return object{"name": name, "namespace": "flux-system"} }
	labels := object{"app": "selected-workload"}
	container := object{"name": "workload", "image": workloadImage, "command": []any{"sleep", "86400"}, "resources": object{"requests": object{"cpu": "10m", "memory": "8Mi"}, "limits": object{"cpu": "50m", "memory": "32Mi"}}}
	pod := object{"containers": []any{container}, "nodeSelector": object{"fixture.invalid/unavailable": "true"}}
	deployment := object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata("selected-workload"), "spec": object{"replicas": 1, "selector": object{"matchLabels": labels}, "template": object{"metadata": object{"labels": labels}, "spec": pod}}}
	if err := writeObject(filepath.Join(repo, "selected/deployment.yaml"), deployment); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "selected/kustomization.yaml"), []byte("resources: [config.yaml, deployment.yaml]\n"), 0644); err != nil {
		return err
	}
	converge := func(message string) error {
		revision, err := commit(message)
		if err != nil {
			return err
		}
		return wait(message, func() (bool, error) {
			artifact, err := f.get("externalartifact", "project-selected")
			if err != nil {
				return false, err
			}
			owner, err := f.get("kustomization", "project-selected")
			return strings.HasSuffix(text(artifact, "status", "artifact", "metadata", "org.opencontainers.image.revision"), revision) && ready(owner) && text(owner, "status", "lastAppliedRevision") == text(artifact, "status", "artifact", "revision"), err
		})
	}
	if err := converge("unavailable selected workload"); err != nil {
		return err
	}
	if err := wait("deployment observed generation", func() (bool, error) {
		deployment, err := f.get("deployment", "selected-workload")
		return at(deployment, "status", "observedGeneration") == at(deployment, "metadata", "generation"), err
	}); err != nil {
		return err
	}
	unavailable, _ := f.get("deployment", "selected-workload")
	owner, _ := f.get("kustomization", "project-selected")
	if available, _ := at(unavailable, "status", "availableReplicas").(float64); available != 0 || !ready(owner) {
		return fmt.Errorf("wait=false fixture did not expose unavailable workload behind Ready owner")
	}
	if err := f.record("ready-owner-unavailable-workload", owner, unavailable); err != nil {
		return err
	}
	unrelated := object{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": metadata("project-unrelated"), "spec": object{"interval": "10m", "path": "./missing", "prune": true, "wait": false, "sourceRef": object{"kind": "ExternalArtifact", "name": "project-selected"}}}
	if err := f.apply([]object{unrelated}); err != nil {
		return err
	}
	if err := wait("unrelated failure", func() (bool, error) {
		unrelated, err := f.get("kustomization", "project-unrelated")
		conditions, _ := at(unrelated, "status", "conditions").([]any)
		for _, condition := range conditions {
			value := condition.(map[string]any)
			if value["type"] == "Ready" && value["status"] == "False" && value["observedGeneration"] == at(unrelated, "metadata", "generation") {
				return true, err
			}
		}
		return false, err
	}); err != nil {
		return err
	}
	delete(pod, "nodeSelector")
	if err := writeObject(filepath.Join(repo, "selected/deployment.yaml"), deployment); err != nil {
		return err
	}
	if err := converge("selected workload recovered"); err != nil {
		return err
	}
	if _, err := f.kubectl("rollout", "status", "deployment/selected-workload", "-n", "flux-system", "--timeout=120s"); err != nil {
		return err
	}
	healthy, _ := f.get("deployment", "selected-workload")
	owner, _ = f.get("kustomization", "project-selected")
	failed, _ := f.get("kustomization", "project-unrelated")
	if !ready(owner) || ready(failed) {
		return fmt.Errorf("selected and unrelated owner readiness did not remain independent")
	}
	if err := f.record("selected-recovery-with-unrelated-failure", owner, healthy, failed); err != nil {
		return err
	}
	job := object{"apiVersion": "batch/v1", "kind": "Job", "metadata": metadata("fixture-migration"), "spec": object{"backoffLimit": 0, "template": object{"spec": object{"restartPolicy": "Never", "containers": []any{object{"name": "migration", "image": workloadImage, "command": []any{"sh", "-c", "sleep 8"}}}}}}}
	if err := writeObject(filepath.Join(repo, "selected/migration/job.yaml"), job); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "selected/migration/kustomization.yaml"), []byte("resources: [job.yaml]\n"), 0644); err != nil {
		return err
	}
	application := object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata("fixture-application"), "data": object{"migration": "complete"}}
	if err := writeObject(filepath.Join(repo, "selected/application/config.yaml"), application); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "selected/application/kustomization.yaml"), []byte("resources: [config.yaml]\n"), 0644); err != nil {
		return err
	}
	if err := converge("parser migration and application"); err != nil {
		return err
	}
	child := func(name, path, dependency string) object {
		return object{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": metadata(name), "spec": object{"interval": "10m", "timeout": "30s", "path": "./selected/" + path, "prune": true, "wait": true, "sourceRef": object{"kind": "ExternalArtifact", "name": "project-selected"}, "dependsOn": []any{object{"name": dependency}}}}
	}
	if err := f.apply([]object{child("parser-migration", "migration", "project-selected"), child("parser-application", "application", "parser-migration")}); err != nil {
		return err
	}
	if err := wait("migration started", func() (bool, error) {
		job, err := f.get("job", "fixture-migration")
		return at(job, "status", "active") == float64(1), err
	}); err != nil {
		return err
	}
	pending, _ := f.get("kustomization", "parser-application")
	if ready(pending) {
		return fmt.Errorf("application ready before migration completed")
	}
	if _, err := f.kubectl("get", "configmap/fixture-application", "-n", "flux-system"); err == nil {
		return fmt.Errorf("application applied before migration completed")
	}
	if err := f.record("parser-application-blocked-by-migration", pending); err != nil {
		return err
	}
	if err := wait("parser application", func() (bool, error) {
		application, err := f.get("kustomization", "parser-application")
		return ready(application), err
	}); err != nil {
		return err
	}
	migration, _ := f.get("kustomization", "parser-migration")
	applied, _ := f.get("kustomization", "parser-application")
	completed, _ := f.get("job", "fixture-migration")
	artifact, _ := f.get("externalartifact", "project-selected")
	if at(completed, "status", "succeeded") != float64(1) || !ready(migration) || text(migration, "status", "lastAppliedRevision") != text(artifact, "status", "artifact", "revision") || text(applied, "status", "lastAppliedRevision") != text(artifact, "status", "artifact", "revision") {
		return fmt.Errorf("parser chain did not consume the same artifact after migration")
	}
	return f.record("parser-migration-before-application", migration, applied, completed, artifact)
}

func (f fixture) permissions() error {
	objects, err := readObjects(filepath.Join(f.root, "platform/components/policy/reconciliation.yaml"))
	if err != nil {
		return err
	}
	var policy []object
	for _, obj := range objects {
		if obj["kind"] != "Secret" {
			policy = append(policy, obj)
		}
	}
	if err := f.apply(policy); err != nil {
		return err
	}
	for _, check := range []struct {
		verb, resource, namespace string
		allowed                   bool
	}{
		{"get", "externalartifacts", "flux-system", true},
		{"get", "artifactgenerators/platform-artifacts", "flux-system", true},
		{"get", "artifactgenerators/other", "flux-system", false},
		{"get", "deployments", "not-created", true},
		{"list", "deployments", "flux-system", false},
		{"get", "secrets", "flux-system", false},
		{"patch", "deployments", "flux-system", false},
		{"get", "externalartifacts", "other", false},
	} {
		actual, _ := f.kubectl("auth", "can-i", check.verb, check.resource, "-n", check.namespace, "--as=system:serviceaccount:flux-system:infrastructure-apply")
		if (strings.TrimSpace(string(actual)) == "yes") != check.allowed {
			return fmt.Errorf("unexpected RBAC: %s %s %s: %s", check.verb, check.resource, check.namespace, actual)
		}
	}
	if err := wait("annotation-only admission", func() (bool, error) {
		_, err := f.kubectl("--as=system:serviceaccount:flux-system:infrastructure-apply", "patch", "kustomization/project-selected", "-n", "flux-system", "--type=merge", "--dry-run=server", "-p", `{"spec":{"suspend":true}}`)
		if err != nil && strings.Contains(err.Error(), "without changing desired configuration") {
			return true, nil
		}
		return false, err
	}); err != nil {
		return err
	}
	return f.record("least-read-rbac-and-annotation-only-admission")
}
