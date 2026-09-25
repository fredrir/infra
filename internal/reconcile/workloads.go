package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

func workloadReady(expected, actual resource) error {
	name := actual.Metadata.Namespace + "/" + actual.Metadata.Name
	if expected.Kind == "HelmRelease" {
		if actual.Spec.Suspend {
			return fmt.Errorf("HelmRelease %s is suspended", name)
		}
		return ready(actual)
	}
	if !reflect.DeepEqual(expected.Spec.Template.Spec, actual.Spec.Template.Spec) {
		return kubernetesDifference("%s %s does not run the declared container images", expected.Kind, name)
	}
	if expected.Kind == "Job" {
		completions := int64(1)
		if expected.Spec.Completions != nil {
			completions = *expected.Spec.Completions
		}
		if actual.Spec.Suspend || actual.Status.Succeeded < completions {
			return fmt.Errorf("%s job has not completed", name)
		}
		for _, condition := range actual.Status.Conditions {
			if condition.Type == "Failed" && condition.Status == "True" {
				return fmt.Errorf("%s job failed", name)
			}
		}
		for _, condition := range actual.Status.Conditions {
			if condition.Type == "Complete" && condition.Status == "True" {
				return nil
			}
		}
		return fmt.Errorf("%s job completion is not confirmed", name)
	}
	if actual.Status.ObservedGeneration != actual.Metadata.Generation {
		return fmt.Errorf("%s rollout has unobserved configuration", name)
	}
	count := int64(1)
	if expected.Spec.Replicas != nil {
		count = *expected.Spec.Replicas
	}
	if actual.Spec.Replicas != nil && *actual.Spec.Replicas != count {
		if expected.Spec.Replicas == nil {
			return fmt.Errorf("%s %s runs %d replicas, want the default %d", expected.Kind, name, *actual.Spec.Replicas, count)
		}
		return kubernetesDifference("%s %s runs %d replicas, want %d", expected.Kind, name, *actual.Spec.Replicas, count)
	}
	switch expected.Kind {
	case "Deployment":
		if actual.Spec.Paused || actual.Status.UpdatedReplicas != count || actual.Status.AvailableReplicas != count || actual.Status.Replicas != count {
			return fmt.Errorf("%s deployment rollout is incomplete", name)
		}
	case "StatefulSet":
		if actual.Status.ReadyReplicas != count || actual.Status.Replicas != count || actual.Status.UpdatedReplicas != count || actual.Status.CurrentRevision != actual.Status.UpdateRevision || count > 0 && actual.Status.CurrentRevision == "" {
			return fmt.Errorf("%s stateful rollout is incomplete", name)
		}
	case "DaemonSet":
		if actual.Status.DesiredNumberScheduled == 0 || actual.Status.UpdatedNumberScheduled != actual.Status.DesiredNumberScheduled || actual.Status.NumberAvailable != actual.Status.DesiredNumberScheduled {
			return fmt.Errorf("%s daemon rollout is incomplete", name)
		}
	default:
		return fmt.Errorf("unsupported workload %s", expected.Kind)
	}
	return nil
}

func (c *Commands) verifyOwnedWorkloads(ctx context.Context, owner resource, snapshot resourceSnapshot) error {
	var inventory struct{ Entries []struct{ ID string } }
	if err := json.Unmarshal(owner.Status.Inventory, &inventory); err != nil {
		return fmt.Errorf("%s inventory: %w", owner.Metadata.Name, err)
	}
	owned := map[string]bool{}
	for _, entry := range inventory.Entries {
		owned[entry.ID] = true
	}
	var problems []error
	for _, expected := range c.kubernetes.workloads[owner.Metadata.Name] {
		group := "apps"
		kind := ""
		switch expected.Kind {
		case "Deployment":
			kind = "deployments.apps"
		case "StatefulSet":
			kind = "statefulsets.apps"
		case "DaemonSet":
			kind = "daemonsets.apps"
		case "Job":
			kind = "jobs.batch"
			group = "batch"
		case "HelmRelease":
			kind = "helmreleases.helm.toolkit.fluxcd.io"
			group = "helm.toolkit.fluxcd.io"
		}
		id := strings.Join([]string{expected.Metadata.Namespace, expected.Metadata.Name, group, expected.Kind}, "_")
		if !owned[id] {
			problems = append(problems, kubernetesDifference("Kustomization %s/%s does not own %s", owner.Metadata.Namespace, owner.Metadata.Name, id))
			continue
		}
		actual, err := c.snapshotResource(ctx, snapshot, kind, expected.Metadata.Namespace, expected.Metadata.Name)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if actual.Metadata.Labels["kustomize.toolkit.fluxcd.io/name"] != owner.Metadata.Name || actual.Metadata.Labels["kustomize.toolkit.fluxcd.io/namespace"] != owner.Metadata.Namespace {
			problems = append(problems, kubernetesDifference("%s is not labelled as owned by Kustomization %s/%s", id, owner.Metadata.Namespace, owner.Metadata.Name))
			continue
		}
		if token := c.kubernetes.requested[kind+"/"+expected.Metadata.Namespace+"/"+expected.Metadata.Name]; token != "" && actual.Status.LastHandledReconcileAt != token {
			problems = append(problems, fmt.Errorf("%s has not handled its requested reconciliation", id))
			continue
		}
		problems = append(problems, workloadReady(expected, actual))
	}
	return errors.Join(problems...)
}
