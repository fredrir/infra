package dev

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/fredrir/infra/internal/ci"
	"github.com/fredrir/infra/internal/process"
	"go.yaml.in/yaml/v3"
)

const (
	ClusterName        = "infra-dev"
	clusterConfigFile  = "dev/cluster/k3d.yaml"
	clusterPatchesFile = "dev/cluster/patches.yaml"
	clusterSecretsDir  = "dev/cluster/secrets"
	fluxComponentsFile = "platform/clusters/production/flux-system/gotk-components.yaml"
	fluxNamespace      = "flux-system"
	sourceName         = "platform-dev"
	registryPush       = "127.0.0.1:5111"
	registryCluster    = ClusterName + "-registry:5000"
	artifactRepository = "platform"
	artifactTag        = "dev"
)

var ErrClusterNotReady = errors.New("cluster kustomizations are not ready")

var ClusterProfiles = map[string][]string{
	"minimal":  {"platform-policy", "platform-projects"},
	"platform": {"platform-policy", "platform-projects", "platform-sources", "platform-ingress", "platform-observability", "platform-cache", "platform-build-cache", "platform-backups", "platform-dns"},
}

var devSettings = map[string]string{"STORAGE_CLASS": "local-path"}

type ClusterOptions struct {
	State   State
	Runner  ci.Runner
	Profile string
	Timeout time.Duration
	Log     io.Writer
}

type KustomizationStatus struct {
	Name     string `json:"name"`
	Ready    bool   `json:"ready"`
	Revision string `json:"revision,omitempty"`
	Message  string `json:"message,omitempty"`
}

type ClusterStatus struct {
	Name           string                `json:"name"`
	Running        bool                  `json:"running"`
	Kubeconfig     string                `json:"kubeconfig,omitempty"`
	Revision       string                `json:"revision,omitempty"`
	Kustomizations []KustomizationStatus `json:"kustomizations,omitempty"`
}

type k3dCluster struct {
	Name           string `json:"name"`
	ServersRunning int    `json:"serversRunning"`
	ServersCount   int    `json:"serversCount"`
}

func (opts ClusterOptions) defaults() ClusterOptions {
	if opts.Runner.Dir == "" {
		opts.Runner.Dir = opts.State.Root
	}
	if opts.Profile == "" {
		opts.Profile = "minimal"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	return opts
}

func (opts ClusterOptions) tool(name string) string {
	if path, err := opts.State.toolPath(name); err == nil {
		return path
	}
	return name
}

func (opts ClusterOptions) kube() (ci.Runner, error) {
	kubeconfig, err := filepath.Abs(opts.State.Kubeconfig())
	if err != nil {
		return ci.Runner{}, err
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		return ci.Runner{}, fmt.Errorf("cluster kubeconfig missing; run infra dev cluster up")
	}
	runner := opts.Runner
	runner.Env = append(append([]string{}, runner.Env...), "KUBECONFIG="+kubeconfig)
	return runner, nil
}

func (opts ClusterOptions) stream(ctx context.Context, runner ci.Runner, name string, args ...string) error {
	_, err := execute(ctx, runner, process.Options{Name: name, Args: args, Stdout: opts.Log, Stderr: opts.Log})
	return err
}

func (opts ClusterOptions) inspect(ctx context.Context) (k3dCluster, bool, error) {
	output, err := capture(ctx, opts.Runner, opts.tool("k3d"), "cluster", "list", "--output", "json")
	if err != nil {
		return k3dCluster{}, false, fmt.Errorf("list clusters: %w", err)
	}
	var clusters []k3dCluster
	if output != "" {
		if err := json.Unmarshal([]byte(output), &clusters); err != nil {
			return k3dCluster{}, false, fmt.Errorf("cluster list: %w", err)
		}
	}
	for _, cluster := range clusters {
		if cluster.Name == ClusterName {
			return cluster, true, nil
		}
	}
	return k3dCluster{}, false, nil
}

func ClusterUp(ctx context.Context, opts ClusterOptions) (ClusterStatus, error) {
	opts = opts.defaults()
	status := ClusterStatus{Name: ClusterName}
	if _, ok := ClusterProfiles[opts.Profile]; !ok {
		return status, fmt.Errorf("unknown cluster profile %q", opts.Profile)
	}
	cluster, exists, err := opts.inspect(ctx)
	if err != nil {
		return status, err
	}
	if !exists {
		config, err := filepath.Abs(filepath.Join(opts.State.Root, clusterConfigFile))
		if err != nil {
			return status, err
		}
		if err := opts.stream(ctx, opts.Runner, opts.tool("k3d"), "cluster", "create", "--config", config); err != nil {
			return status, fmt.Errorf("create cluster: %w", err)
		}
		fmt.Fprintln(opts.Log, "Created:", ClusterName)
	} else if cluster.ServersRunning < cluster.ServersCount {
		if err := opts.stream(ctx, opts.Runner, opts.tool("k3d"), "cluster", "start", ClusterName); err != nil {
			return status, fmt.Errorf("start cluster: %w", err)
		}
	}
	if err := os.MkdirAll(opts.State.Cluster(), 0o755); err != nil {
		return status, err
	}
	kubeconfig, err := capture(ctx, opts.Runner, opts.tool("k3d"), "kubeconfig", "get", ClusterName)
	if err != nil {
		return status, fmt.Errorf("read kubeconfig: %w", err)
	}
	if err := os.WriteFile(opts.State.Kubeconfig(), []byte(kubeconfig+"\n"), 0o600); err != nil {
		return status, err
	}
	return ClusterSync(ctx, opts)
}

func ClusterSync(ctx context.Context, opts ClusterOptions) (ClusterStatus, error) {
	opts = opts.defaults()
	status := ClusterStatus{Name: ClusterName}
	selected, ok := ClusterProfiles[opts.Profile]
	if !ok {
		return status, fmt.Errorf("unknown cluster profile %q", opts.Profile)
	}
	if _, exists, err := opts.inspect(ctx); err != nil {
		return status, err
	} else if !exists {
		return status, fmt.Errorf("cluster %s missing; run infra dev cluster up", ClusterName)
	}
	kube, err := opts.kube()
	if err != nil {
		return status, err
	}
	if err := opts.installFlux(ctx, kube); err != nil {
		return status, err
	}
	recipient, err := opts.ensureAgeKey(ctx, kube)
	if err != nil {
		return status, err
	}
	revision, err := capture(ctx, opts.Runner, "git", "rev-parse", "HEAD")
	if err != nil {
		return status, err
	}
	status.Revision = revision
	artifact, err := buildArtifact(ctx, opts, recipient)
	if err != nil {
		return status, err
	}
	source, err := capture(ctx, opts.Runner, "git", "remote", "get-url", "origin")
	if err != nil {
		source = "local"
	}
	if err := opts.stream(ctx, kube, opts.tool("flux"), "push", "artifact", "oci://"+registryPush+"/"+artifactRepository+":"+artifactTag, "--path="+artifact, "--source="+source, "--revision="+artifactTag+"@sha1:"+revision, "--insecure-registry", "--provider=generic"); err != nil {
		return status, fmt.Errorf("push artifact: %w", err)
	}
	root, err := generateRoot(opts.State.Root, selected)
	if err != nil {
		return status, err
	}
	rootFile, err := filepath.Abs(filepath.Join(opts.State.Cluster(), "root.yaml"))
	if err != nil {
		return status, err
	}
	if err := os.WriteFile(rootFile, root, 0o644); err != nil {
		return status, err
	}
	if err := opts.stream(ctx, kube, opts.tool("kubectl"), "apply", "--server-side", "--force-conflicts", "-f", rootFile); err != nil {
		return status, fmt.Errorf("apply dev root: %w", err)
	}
	if err := opts.stream(ctx, kube, opts.tool("flux"), "reconcile", "source", "oci", sourceName, "--timeout="+opts.Timeout.String()); err != nil {
		return status, fmt.Errorf("reconcile source: %w", err)
	}
	for _, name := range selected {
		if err := opts.stream(ctx, kube, opts.tool("kubectl"), "wait", "kustomization/"+name, "--namespace="+fluxNamespace, "--for=condition=Ready", "--timeout="+opts.Timeout.String()); err != nil {
			fmt.Fprintln(opts.Log, "Not ready:", name)
		}
	}
	status.Kubeconfig = opts.State.Kubeconfig()
	status.Running = true
	if status.Kustomizations, err = opts.kustomizations(ctx, kube); err != nil {
		return status, err
	}
	for _, name := range selected {
		index := slices.IndexFunc(status.Kustomizations, func(k KustomizationStatus) bool { return k.Name == name })
		if index < 0 || !status.Kustomizations[index].Ready {
			return status, ErrClusterNotReady
		}
	}
	return status, nil
}

func (opts ClusterOptions) installFlux(ctx context.Context, kube ci.Runner) error {
	components, err := filepath.Abs(filepath.Join(opts.State.Root, fluxComponentsFile))
	if err != nil {
		return err
	}
	if err := opts.stream(ctx, kube, opts.tool("kubectl"), "apply", "--server-side", "--force-conflicts", "-f", components); err != nil {
		return fmt.Errorf("install Flux: %w", err)
	}
	if err := opts.stream(ctx, kube, opts.tool("kubectl"), "wait", "--for=condition=Established", "--timeout=2m", "crd/kustomizations.kustomize.toolkit.fluxcd.io", "crd/ocirepositories.source.toolkit.fluxcd.io"); err != nil {
		return fmt.Errorf("Flux CRDs: %w", err)
	}
	for _, controller := range []string{"source-controller", "kustomize-controller", "helm-controller"} {
		if err := opts.stream(ctx, kube, opts.tool("kubectl"), "rollout", "status", "deployment/"+controller, "--namespace="+fluxNamespace, "--timeout=3m"); err != nil {
			return fmt.Errorf("%s: %w", controller, err)
		}
	}
	return nil
}

func (opts ClusterOptions) ensureAgeKey(ctx context.Context, kube ci.Runner) (string, error) {
	key := filepath.Join(opts.State.Cluster(), "age.key")
	if _, err := os.Stat(key); os.IsNotExist(err) {
		if _, err := capture(ctx, opts.Runner, opts.tool("age-keygen"), "-o", key); err != nil {
			return "", fmt.Errorf("generate dev age key: %w", err)
		}
		fmt.Fprintln(opts.Log, "Generated:", key)
	} else if err != nil {
		return "", err
	}
	recipient, err := capture(ctx, opts.Runner, opts.tool("age-keygen"), "-y", key)
	if err != nil {
		return "", fmt.Errorf("read dev age recipient: %w", err)
	}
	data, err := os.ReadFile(key)
	if err != nil {
		return "", err
	}
	secret, err := yaml.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "sops-age", "namespace": fluxNamespace}, "type": "Opaque", "data": map[string]string{"age.agekey": base64.StdEncoding.EncodeToString(data)}})
	if err != nil {
		return "", err
	}
	if _, err := execute(ctx, kube, process.Options{Name: opts.tool("kubectl"), Args: []string{"apply", "--server-side", "--force-conflicts", "-f", "-"}, Stdin: bytes.NewReader(secret), Stdout: opts.Log, Stderr: opts.Log}); err != nil {
		return "", fmt.Errorf("install dev decryption key: %w", err)
	}
	return recipient, nil
}

func generateRoot(root string, selected []string) ([]byte, error) {
	settingsData, err := os.ReadFile(filepath.Join(root, settingsFile))
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	if err := yaml.Unmarshal(settingsData, &settings); err != nil {
		return nil, err
	}
	data, _ := settings["data"].(map[string]any)
	if data == nil {
		return nil, fmt.Errorf("%s declares no settings", settingsFile)
	}
	for key, value := range devSettings {
		data[key] = value
	}
	patchesData, err := os.ReadFile(filepath.Join(root, clusterPatchesFile))
	if err != nil {
		return nil, err
	}
	var patches []any
	if err := yaml.Unmarshal(patchesData, &patches); err != nil {
		return nil, fmt.Errorf("%s: %w", clusterPatchesFile, err)
	}
	rootData, err := os.ReadFile(filepath.Join(root, clusterKustomizations))
	if err != nil {
		return nil, err
	}
	kustomizations := make(map[string]map[string]any)
	decoder := yaml.NewDecoder(bytes.NewReader(rootData))
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", clusterKustomizations, err)
		}
		metadata, _ := document["metadata"].(map[string]any)
		if document["kind"] == "Kustomization" && metadata != nil {
			kustomizations[fmt.Sprint(metadata["name"])] = document
		}
	}
	documents := []any{
		map[string]any{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository", "metadata": map[string]any{"name": sourceName, "namespace": fluxNamespace}, "spec": map[string]any{"interval": "1m", "url": "oci://" + registryCluster + "/" + artifactRepository, "ref": map[string]any{"tag": artifactTag}, "insecure": true}},
		settings,
	}
	for _, name := range selected {
		kustomization, ok := kustomizations[name]
		if !ok {
			return nil, fmt.Errorf("%s lacks Kustomization %s", clusterKustomizations, name)
		}
		spec, _ := kustomization["spec"].(map[string]any)
		if spec == nil {
			return nil, fmt.Errorf("Kustomization %s has no spec", name)
		}
		spec["sourceRef"] = map[string]any{"kind": "OCIRepository", "name": sourceName}
		spec["interval"] = "1m"
		spec["timeout"] = "2m"
		spec["wait"] = false
		delete(spec, "healthChecks")
		var dependencies []any
		if declared, ok := spec["dependsOn"].([]any); ok {
			for _, dependency := range declared {
				entry, _ := dependency.(map[string]any)
				if entry != nil && slices.Contains(selected, fmt.Sprint(entry["name"])) {
					dependencies = append(dependencies, dependency)
				}
			}
		}
		delete(spec, "dependsOn")
		if len(dependencies) > 0 {
			spec["dependsOn"] = dependencies
		}
		existing, _ := spec["patches"].([]any)
		spec["patches"] = append(existing, patches...)
		documents = append(documents, kustomization)
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	for _, document := range documents {
		if err := encoder.Encode(document); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), encoder.Close()
}

func (opts ClusterOptions) kustomizations(ctx context.Context, kube ci.Runner) ([]KustomizationStatus, error) {
	output, err := capture(ctx, kube, opts.tool("kubectl"), "get", "kustomizations.kustomize.toolkit.fluxcd.io", "--namespace="+fluxNamespace, "--output=json")
	if err != nil {
		return nil, fmt.Errorf("read kustomizations: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct{ Name string }
			Status   struct {
				LastAppliedRevision string `json:"lastAppliedRevision"`
				Conditions          []struct{ Type, Status, Message string }
			}
		}
	}
	if err := json.Unmarshal([]byte(output), &list); err != nil {
		return nil, fmt.Errorf("kustomization list: %w", err)
	}
	var statuses []KustomizationStatus
	for _, item := range list.Items {
		status := KustomizationStatus{Name: item.Metadata.Name, Revision: item.Status.LastAppliedRevision}
		for _, condition := range item.Status.Conditions {
			if condition.Type == "Ready" {
				status.Ready, status.Message = condition.Status == "True", condition.Message
			}
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func InspectCluster(ctx context.Context, opts ClusterOptions) (ClusterStatus, error) {
	opts = opts.defaults()
	status := ClusterStatus{Name: ClusterName}
	cluster, exists, err := opts.inspect(ctx)
	if err != nil || !exists {
		return status, err
	}
	status.Running = cluster.ServersCount > 0 && cluster.ServersRunning == cluster.ServersCount
	kube, err := opts.kube()
	if err != nil || !status.Running {
		return status, nil
	}
	status.Kubeconfig = opts.State.Kubeconfig()
	status.Kustomizations, _ = opts.kustomizations(ctx, kube)
	return status, nil
}

func ClusterDown(ctx context.Context, opts ClusterOptions) error {
	opts = opts.defaults()
	_, exists, err := opts.inspect(ctx)
	if err != nil {
		return err
	}
	if exists {
		if err := opts.stream(ctx, opts.Runner, opts.tool("k3d"), "cluster", "delete", ClusterName); err != nil {
			return fmt.Errorf("delete cluster: %w", err)
		}
		fmt.Fprintln(opts.Log, "Deleted:", ClusterName)
	}
	if err := os.Remove(opts.State.Kubeconfig()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
