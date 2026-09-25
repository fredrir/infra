package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

type resourceSnapshot map[string]resourceList

type resourceList struct {
	items map[string]resource
	err   error
}

func (c *Commands) snapshotResource(ctx context.Context, snapshot resourceSnapshot, kind, namespace, name string) (resource, error) {
	key := kind + "/" + namespace
	list, ok := snapshot[key]
	if !ok {
		list.items, list.err = c.namespacedResources(ctx, kind, namespace)
		snapshot[key] = list
	}
	if list.err != nil {
		return resource{}, list.err
	}
	item, ok := list.items[name]
	if !ok {
		return resource{}, kubernetesDifference("%s %s/%s is missing", kind, namespace, name)
	}
	return item, nil
}

func (c *Commands) namespacedResources(ctx context.Context, kind, namespace string) (map[string]resource, error) {
	data, err := c.Runner.Output(ctx, "kubectl", "get", kind, "-n="+namespace, "-o=json", "--request-timeout=30s")
	if err != nil {
		return nil, err
	}
	var result struct{ Items []resource }
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	items := map[string]resource{}
	for _, item := range result.Items {
		if item.Metadata.Namespace != namespace {
			return nil, fmt.Errorf("%s returned an unexpected namespace", kind)
		}
		items[item.Metadata.Name] = item
	}
	return items, nil
}

func (c *Commands) getResource(ctx context.Context, kind, namespace, name string) (resource, error) {
	data, err := c.Runner.Output(ctx, "kubectl", "get", kind, name, "-n="+namespace, "-o=json", "--request-timeout=30s")
	if err != nil {
		return resource{}, err
	}
	var item resource
	err = json.Unmarshal(data, &item)
	return item, err
}

func conditionReady(item resource) error {
	if item.Spec.Suspend {
		return fmt.Errorf("%s %s/%s is suspended", item.Kind, item.Metadata.Namespace, item.Metadata.Name)
	}
	for _, condition := range item.Status.Conditions {
		if (condition.Type == "Reconciling" || condition.Type == "Stalled") && condition.Status == "True" {
			return fmt.Errorf("%s: %s", item.Metadata.Name, condition.Message)
		}
	}
	for _, condition := range item.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" && condition.ObservedGeneration == item.Metadata.Generation {
			return nil
		}
	}
	return fmt.Errorf("%s is not ready at generation %d", item.Metadata.Name, item.Metadata.Generation)
}

func (c *Commands) artifacts(ctx context.Context) (map[string]resource, error) {
	data, err := c.Runner.Output(ctx, "kubectl", "get", "externalartifacts.source.toolkit.fluxcd.io", "-n=flux-system", "-o=json", "--request-timeout=30s")
	if err != nil {
		return nil, err
	}
	var result struct{ Items []resource }
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	artifacts := map[string]resource{}
	for _, item := range result.Items {
		artifacts[item.Metadata.Name] = item
	}
	return artifacts, nil
}

func artifactPaths(generator resource, name string) ([]string, error) {
	if len(generator.Spec.Sources) != 1 || generator.Spec.Sources[0].Alias != "repo" || generator.Spec.Sources[0].Kind != "GitRepository" || generator.Spec.Sources[0].Name != "flux-system" {
		return nil, fmt.Errorf("unsupported artifact sources")
	}
	for _, artifact := range generator.Spec.Artifacts {
		if artifact.Name != name {
			continue
		}
		if artifact.OriginRevision != "@repo" || len(artifact.Copy) == 0 {
			return nil, fmt.Errorf("%s has unsupported artifact provenance", name)
		}
		paths := []string{productionOverlay, "build/rollout/flux-artifacts/bootstrap", "build/rollout/flux-artifacts/pause", "platform/clusters/production/flux-system", ".sourceignore", ".gitmodules"}
		for _, copy := range artifact.Copy {
			if !strings.HasPrefix(copy.From, "@repo/") {
				return nil, fmt.Errorf("unsupported artifact input %s", copy.From)
			}
			path := strings.TrimPrefix(copy.From, "@repo/")
			path = strings.TrimSuffix(path, "/**")
			if path == "" || strings.ContainsAny(path, "*?[") || strings.Contains(path, "..") || copy.To != "@artifact/"+path && copy.To != "@artifact/"+path+"/" {
				return nil, fmt.Errorf("unsupported artifact copy %s", copy.From)
			}
			paths = append(paths, path)
		}
		return paths, nil
	}
	return nil, fmt.Errorf("artifact %s missing from desired generator", name)
}

var originPattern = regexp.MustCompile(`^(production|main)@sha1:([a-f0-9]{40})$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func (c *Commands) verifyArtifactOrigin(ctx context.Context, generator, artifact resource, revision string) error {
	origin := originPattern.FindStringSubmatch(artifact.Status.Artifact.Metadata["org.opencontainers.image.revision"])
	if len(origin) != 3 {
		return fmt.Errorf("%s has invalid source provenance", artifact.Metadata.Name)
	}
	if _, err := artifactPaths(generator, artifact.Metadata.Name); err != nil {
		return err
	}
	if origin[2] != revision {
		return kubernetesDifference("ExternalArtifact %s/%s provenance has not advanced from %s to %s", artifact.Metadata.Namespace, artifact.Metadata.Name, origin[2], revision)
	}
	return nil
}

func (c *Commands) verifyArtifacts(ctx context.Context, plan Plan, owners []resource) error {
	generator, err := c.getResource(ctx, "artifactgenerators.source.extensions.fluxcd.io", "flux-system", "platform-artifacts")
	if err != nil {
		return err
	}
	var problems []error
	problems = append(problems, conditionReady(generator))
	observed := generator.Spec
	observed.Suspend = c.kubernetes.generator.Spec.Suspend
	expected, _ := json.Marshal(c.kubernetes.generator.Spec)
	actual, _ := json.Marshal(observed)
	if string(expected) != string(actual) {
		problems = append(problems, kubernetesDifference("ArtifactGenerator flux-system/platform-artifacts differs from its declaration"))
	}
	if generator.Spec.Suspend {
		return errors.Join(problems...)
	}
	var inventory []struct{ Name, Namespace, Digest string }
	if err = json.Unmarshal(generator.Status.Inventory, &inventory); err != nil {
		return errors.Join(append(problems, err)...)
	}
	artifacts, err := c.artifacts(ctx)
	if err != nil {
		return errors.Join(append(problems, err)...)
	}
	required := map[string]bool{}
	for _, owner := range owners {
		if owner.Spec.SourceRef.Kind == "ExternalArtifact" {
			required[owner.Spec.SourceRef.Name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(required)) {
		problems = append(problems, c.verifyArtifact(ctx, plan, generator, inventory, artifacts, name))
	}
	if len(plan.Affected.Projects) > 0 {
		for _, name := range slices.Sorted(maps.Keys(c.kubernetes.baseline)) {
			if slices.Contains(plan.Affected.Projects, strings.TrimPrefix(name, "project-")) {
				continue
			}
			artifact, ok := artifacts[name]
			if !ok || artifact.Status.Artifact.Digest != c.kubernetes.baseline[name] {
				problems = append(problems, fmt.Errorf("unselected artifact %s changed during scoped deployment", name))
			}
		}
	}
	for _, owner := range owners {
		if artifact, ok := artifacts[owner.Spec.SourceRef.Name]; owner.Spec.SourceRef.Kind == "ExternalArtifact" && ok && owner.Status.LastAppliedRevision != artifact.Status.Artifact.Revision {
			problems = append(problems, kubernetesDifference("Kustomization %s/%s has not applied its current artifact %s", owner.Metadata.Namespace, owner.Metadata.Name, artifact.Status.Artifact.Revision))
		}
	}
	return errors.Join(problems...)
}

func (c *Commands) verifyArtifact(ctx context.Context, plan Plan, generator resource, inventory []struct{ Name, Namespace, Digest string }, artifacts map[string]resource, name string) error {
	artifact, ok := artifacts[name]
	if !ok {
		return fmt.Errorf("missing artifact %s", name)
	}
	if err := conditionReady(artifact); err != nil {
		return err
	}
	if artifact.Spec.SourceRef.Kind != "ArtifactGenerator" || artifact.Spec.SourceRef.Name != generator.Metadata.Name || artifact.Spec.SourceRef.Namespace != "flux-system" || generator.Metadata.UID == "" || artifact.Metadata.Labels["source.extensions.fluxcd.io/generator"] != generator.Metadata.UID {
		return fmt.Errorf("%s has unexpected generator ownership", name)
	}
	digest := artifact.Status.Artifact.Digest
	if !digestPattern.MatchString(digest) || artifact.Status.Artifact.Revision != "latest@"+digest {
		return fmt.Errorf("%s has inconsistent content revision", name)
	}
	if !slices.ContainsFunc(inventory, func(entry struct{ Name, Namespace, Digest string }) bool {
		return entry.Name == name && entry.Namespace == "flux-system" && entry.Digest == digest
	}) {
		return fmt.Errorf("%s does not match generator inventory", name)
	}
	return c.verifyArtifactOrigin(ctx, generator, artifact, plan.Revision)
}
