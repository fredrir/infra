package reconcile

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"regexp"
	"time"
)

var checkpointRevision = regexp.MustCompile(`^[a-f0-9]{40}$`)
var planFormat = regexp.MustCompile(`^1\.[0-9]+$`)

func reusableHosts(status Status, now time.Time) bool {
	if status.HostScope != HostScopeFull || status.Stage != "publish" || status.Desired == status.Applied {
		return false
	}
	if !checkpointRevision.MatchString(status.Desired) || !checkpointRevision.MatchString(status.Applied) {
		return false
	}
	if status.Updated.IsZero() || status.Updated.After(now) || now.Sub(status.Updated) > 90*time.Minute {
		return false
	}
	for _, stage := range []string{"plan", "expand", "hosts"} {
		duration := status.Durations[stage]
		if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
			return false
		}
	}
	return true
}

func (c *Commands) ExpansionUnchanged(ctx context.Context) (bool, error) {
	data, err := c.Runner.Output(ctx, "tofu", "-chdir=tofu", "show", "-json", filepath.Join(c.Work, "expand.tfplan"))
	if err != nil {
		return false, err
	}
	return unchangedExpansion(data), nil
}

func unchangedExpansion(data []byte) bool {
	type change struct {
		Actions   []string        `json:"actions"`
		Importing json.RawMessage `json:"importing"`
	}
	var plan struct {
		FormatVersion   string                     `json:"format_version"`
		PriorState      map[string]json.RawMessage `json:"prior_state"`
		PlannedValues   map[string]json.RawMessage `json:"planned_values"`
		Complete        *bool                      `json:"complete"`
		Errored         bool                       `json:"errored"`
		ResourceDrift   []json.RawMessage          `json:"resource_drift"`
		DeferredChanges []json.RawMessage          `json:"deferred_changes"`
		ResourceChanges []struct {
			Address         string `json:"address"`
			PreviousAddress string `json:"previous_address"`
			Deposed         string `json:"deposed"`
			Change          change `json:"change"`
		} `json:"resource_changes"`
		OutputChanges map[string]change `json:"output_changes"`
		Checks        []struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	if json.Unmarshal(data, &plan) != nil || !planFormat.MatchString(plan.FormatVersion) {
		return false
	}
	if plan.Errored || (plan.Complete != nil && !*plan.Complete) || plan.PriorState == nil || plan.PlannedValues == nil {
		return false
	}
	if len(plan.ResourceChanges) == 0 || len(plan.ResourceDrift) > 0 || len(plan.DeferredChanges) > 0 {
		return false
	}
	unchanged := func(value change) bool {
		return len(value.Actions) == 1 && value.Actions[0] == "no-op" && (len(value.Importing) == 0 || string(value.Importing) == "null")
	}
	for _, resource := range plan.ResourceChanges {
		if resource.Address == "" || resource.PreviousAddress != "" || resource.Deposed != "" || !unchanged(resource.Change) {
			return false
		}
	}
	for _, output := range plan.OutputChanges {
		if !unchanged(output) {
			return false
		}
	}
	for _, check := range plan.Checks {
		if check.Status != "pass" {
			return false
		}
	}
	return true
}
