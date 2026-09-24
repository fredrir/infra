package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const productionOverlay = "build/rollout/flux-artifacts/cutover"

type kubernetesState struct {
	owners      map[string]resource
	definitions map[string]map[string]any
	generator   resource
	workloads   map[string][]resource
	baseline    map[string]string
	requested   map[string]string
}

func decodeDocuments(data []byte) ([]map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var result []map[string]any
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			if err == io.EOF {
				return result, nil
			}
			return nil, err
		}
		if len(document) > 0 {
			result = append(result, document)
		}
	}
}

func documentResource(document map[string]any) (resource, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return resource{}, err
	}
	var item resource
	err = json.Unmarshal(data, &item)
	return item, err
}

func (c *Commands) loadKubernetes(ctx context.Context) error {
	if c.kubernetes != nil {
		return nil
	}
	data, err := c.Runner.Output(ctx, "kubectl", "kustomize", productionOverlay)
	if err != nil {
		return err
	}
	documents, err := decodeDocuments(data)
	if err != nil {
		return err
	}
	state := &kubernetesState{owners: map[string]resource{}, definitions: map[string]map[string]any{}, workloads: map[string][]resource{}, baseline: map[string]string{}}
	for _, document := range documents {
		item, err := documentResource(document)
		if err != nil {
			return err
		}
		switch item.Kind {
		case "Kustomization":
			if item.APIVersion != "kustomize.toolkit.fluxcd.io/v1" {
				continue
			}
			state.owners[item.Metadata.Name] = item
			state.definitions[item.Metadata.Name] = document
		case "ArtifactGenerator":
			state.generator = item
		}
	}
	root, ok := state.owners["flux-system"]
	if !ok || root.Spec.Path != "./"+productionOverlay || state.generator.Metadata.Name != "platform-artifacts" {
		return fmt.Errorf("unsupported production Flux topology")
	}
	c.kubernetes = state
	return nil
}

func projectOwners(project string) []string {
	switch project {
	case "llunde", "portfolio", "y":
		return []string{"project-" + project}
	case "llunde-pyparser":
		return []string{"project-llunde-pyparser", "llunde-pyparser-migration", "llunde-pyparser-application"}
	default:
		return nil
	}
}

func supportedProjects(projects []string) bool {
	for _, project := range projects {
		if len(projectOwners(project)) == 0 {
			return false
		}
	}
	return true
}

func (c *Commands) selectedOwners(plan Plan) []string {
	if len(plan.Affected.Projects) > 0 {
		var names []string
		for _, project := range plan.Affected.Projects {
			names = append(names, projectOwners(project)...)
		}
		return names
	}
	var names []string
	for name, owner := range c.kubernetes.owners {
		if !owner.Spec.Suspend {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func (c *Commands) Preflight(ctx context.Context, plan Plan) (Plan, error) {
	if !c.VerifyArtifacts {
		plan.Affected.Projects = nil
		return plan, nil
	}
	if err := c.loadKubernetes(ctx); err != nil {
		return plan, err
	}
	if !c.ScopeProjects {
		plan.Affected.Projects = nil
	}
	if len(plan.Affected.Projects) > 0 {
		valid := true
		for _, project := range plan.Affected.Projects {
			names := projectOwners(project)
			valid = valid && len(names) > 0
			for _, name := range names {
				owner, ok := c.kubernetes.owners[name]
				valid = valid && ok && !owner.Spec.Suspend && owner.Spec.SourceRef.Kind == "ExternalArtifact" && owner.Spec.SourceRef.Name == "project-"+project
			}
		}
		if !valid {
			plan.Affected.Reasons = append(plan.Affected.Reasons, "full Kubernetes: unsupported project topology")
			plan.Affected.Projects = nil
		}
	}
	for _, query := range [][]string{{"externalartifacts.source.toolkit.fluxcd.io", "-n=flux-system"}, {"artifactgenerators.source.extensions.fluxcd.io", "platform-artifacts", "-n=flux-system"}} {
		if _, err := c.Runner.Output(ctx, "kubectl", append([]string{"get"}, append(query, "-o=json", "--request-timeout=30s")...)...); err != nil {
			return plan, fmt.Errorf("artifact preflight: %w", err)
		}
	}
	for _, kind := range []string{"deployments.apps", "statefulsets.apps", "daemonsets.apps", "jobs.batch"} {
		for _, verb := range []string{"get", "list"} {
			answer, err := c.Runner.Output(ctx, "kubectl", "auth", "can-i", verb, kind, "--all-namespaces")
			if err != nil || strings.TrimSpace(string(answer)) != "yes" {
				return plan, fmt.Errorf("workload preflight requires %s %s: %v", verb, kind, err)
			}
		}
	}
	if len(plan.Affected.Projects) > 0 {
		current, err := c.resources(ctx, "kustomizations.kustomize.toolkit.fluxcd.io")
		if err != nil {
			return plan, err
		}
		indexed := map[string]resource{}
		for _, owner := range current {
			if owner.Metadata.Namespace == "flux-system" {
				indexed[owner.Metadata.Name] = owner
			}
		}
		for _, name := range append(c.selectedOwners(plan), "flux-system", "platform-policy") {
			expected := c.kubernetes.owners[name]
			actual, ok := indexed[name]
			if !ok || actual.Spec.Suspend || actual.Spec.Path != expected.Spec.Path || !reflect.DeepEqual(actual.Spec.SourceRef, expected.Spec.SourceRef) || !reflect.DeepEqual(actual.Spec.DependsOn, expected.Spec.DependsOn) {
				plan.Affected.Projects = nil
				plan.Affected.Reasons = append(plan.Affected.Reasons, "full Kubernetes: live project topology differs")
				break
			}
		}
		artifacts, err := c.artifacts(ctx)
		if err != nil {
			return plan, err
		}
		for name, artifact := range artifacts {
			c.kubernetes.baseline[name] = artifact.Status.Artifact.Digest
		}
	}
	return plan, nil
}

func (c *Commands) RenderKubernetes(ctx context.Context, plan Plan) error {
	for _, project := range plan.Affected.Projects {
		if len(projectOwners(project)) == 0 {
			return fmt.Errorf("unsupported selected project %s", project)
		}
	}
	if err := c.loadKubernetes(ctx); err != nil {
		return err
	}
	settings, err := os.ReadFile(filepath.Join(c.Runner.Dir, "platform/clusters/production/settings.yaml"))
	if err != nil {
		return err
	}
	var config struct {
		Data map[string]string `yaml:"data"`
	}
	if err = yaml.Unmarshal(settings, &config); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(c.Work, "flux-render-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	for _, name := range c.selectedOwners(plan) {
		owner := c.kubernetes.owners[name]
		document := c.kubernetes.definitions[name]
		data, err := json.Marshal(document)
		if err != nil {
			return err
		}
		var local map[string]any
		if err = json.Unmarshal(data, &local); err != nil {
			return err
		}
		spec := local["spec"].(map[string]any)
		if post, ok := spec["postBuild"].(map[string]any); ok {
			if references, ok := post["substituteFrom"].([]any); ok {
				for _, reference := range references {
					ref := reference.(map[string]any)
					if ref["kind"] != "ConfigMap" || ref["name"] != "platform-settings" {
						return fmt.Errorf("unsupported substitution source in %s", name)
					}
				}
				values := map[string]any{}
				for key, value := range config.Data {
					values[key] = value
				}
				if inline, ok := post["substitute"].(map[string]any); ok {
					for key, value := range inline {
						values[key] = value
					}
				}
				post["substitute"] = values
				delete(post, "substituteFrom")
			}
		}
		content, err := yaml.Marshal(local)
		if err != nil {
			return err
		}
		path := filepath.Join(directory, name+".yaml")
		if err = os.WriteFile(path, content, 0600); err != nil {
			return err
		}
		rendered, err := c.Runner.Output(ctx, "flux", "build", "kustomization", name, "--path="+owner.Spec.Path, "--kustomization-file="+path, "--dry-run", "--strict-substitute")
		if err != nil {
			return err
		}
		documents, err := decodeDocuments(rendered)
		if err != nil {
			return err
		}
		var workloads []resource
		for _, document := range documents {
			item, err := documentResource(document)
			if err != nil {
				return err
			}
			switch item.Kind {
			case "Deployment", "StatefulSet", "DaemonSet", "Job", "HelmRelease":
				workloads = append(workloads, item)
			}
		}
		c.kubernetes.workloads[name] = workloads
	}
	return nil
}

func (c *Commands) requestSelected(ctx context.Context, plan Plan) error {
	if c.kubernetes.requested == nil {
		c.kubernetes.requested = map[string]string{}
	}
	token := time.Now().UTC().Format(time.RFC3339Nano)
	request := func(kind, namespace, name string) error {
		if err := c.Runner.Run(ctx, "kubectl", "annotate", kind, name, "-n="+namespace, "--overwrite", "--field-manager=flux-client-side-apply", "reconcile.fluxcd.io/requestedAt="+token, "--request-timeout=30s"); err != nil {
			return err
		}
		c.kubernetes.requested[kind+"/"+namespace+"/"+name] = token
		return nil
	}
	for _, name := range c.selectedOwners(plan) {
		if err := request("kustomizations.kustomize.toolkit.fluxcd.io", "flux-system", name); err != nil {
			return err
		}
		for _, item := range c.kubernetes.workloads[name] {
			if item.Kind == "HelmRelease" {
				if err := request("helmreleases.helm.toolkit.fluxcd.io", item.Metadata.Namespace, item.Metadata.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
