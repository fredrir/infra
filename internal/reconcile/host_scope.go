package reconcile

import (
	"context"
	"errors"
	"slices"
)

const (
	monitorTags    = "--tags=gatus,verification_trigger"
	monitorCLITags = "--tags=infra_binary"
)

func monitorCLIChanged(selected Selection) bool {
	return effectiveHostScope(selected) == HostScopeRunners && slices.Contains(selected.RunnerInputs, "build/cli-release.json")
}

func (c *Commands) PlanHosts(ctx context.Context, plan Plan) error {
	var playbooks [][]string
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		if _, err := LoadRunnerFleet(c.Runner.Dir); err != nil {
			return err
		}
		playbooks = [][]string{{"reconcile.yml"}, {"external.yml"}, {"verify.yml"}, {"verify-runners.yml"}}
	case HostScopeRunners:
		if _, err := LoadRunnerFleet(c.Runner.Dir); err != nil {
			return err
		}
		playbooks = [][]string{{"build-runners.yml"}, {"verify-runners.yml"}}
		if monitorCLIChanged(plan.Affected) {
			playbooks = append(playbooks, []string{"external.yml", monitorCLITags})
		}
	case HostScopeMonitor:
		playbooks = [][]string{{"external.yml", monitorTags}, {"verify.yml", "--limit=external"}}
	}
	for _, playbook := range playbooks {
		for _, check := range []string{"--syntax-check", "--list-tasks"} {
			if err := c.ansible(ctx, playbook[0], append([]string{check}, playbook[1:]...)...); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Commands) Hosts(ctx context.Context, plan Plan) error {
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		return c.convergeRunners(ctx, plan, "reconcile.yml")
	case HostScopeRunners:
		return c.convergeRunners(ctx, plan, "build-runners.yml")
	default:
		return nil
	}
}

func (c *Commands) VerifyHosts(ctx context.Context, plan Plan) error {
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		return c.verifyRunnerHosts(ctx, "verify.yml", "verify-runners.yml")
	case HostScopeRunners:
		return c.verifyRunnerHosts(ctx, "verify-runners.yml")
	case HostScopeMonitor:
		return c.ansible(ctx, "verify.yml", "--limit=external")
	default:
		return nil
	}
}

var comparedPlaybooks = []string{"reconcile.yml", "external.yml"}

func (c *Commands) compareHosts(ctx context.Context) error {
	var problems []error
	for _, run := range c.recordPlaybooks(ctx, comparedPlaybooks, "--check", "--skip-tags=runners") {
		var differences Differences
		var failed []string
		for _, task := range run.Tasks {
			if task.Failed {
				failed = append(failed, "["+task.Host+"] "+task.Task)
			} else {
				differences = append(differences, Difference{System: "hosts", Host: task.Host, Item: task.Task})
			}
		}
		problems = append(problems, run.outcome(differences, failed))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(problems...)
}

func (c *Commands) verifyRunnerHosts(ctx context.Context, playbooks ...string) error {
	fleet, err := LoadRunnerFleet(c.Runner.Dir)
	if err != nil {
		return err
	}
	var problems []error
	for _, run := range c.recordPlaybooks(ctx, playbooks) {
		var differences Differences
		var failed []string
		for _, task := range run.Tasks {
			switch {
			case run.Playbook == "verify-runners.yml":
				differences = append(differences, Difference{System: "runners", Host: task.Host, Item: task.Task})
			case task.Failed:
				failed = append(failed, "["+task.Host+"] "+task.Task)
			}
		}
		problems = append(problems, run.outcome(differences, failed))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(append(problems, c.verifyRunnerFleet(ctx, fleet))...)
}

func (c *Commands) Monitor(ctx context.Context, plan Plan) error {
	switch {
	case effectiveHostScope(plan.Affected) == HostScopeFull:
		return c.ansible(ctx, "external.yml")
	case effectiveHostScope(plan.Affected) == HostScopeMonitor:
		return c.ansible(ctx, "external.yml", monitorTags)
	case monitorCLIChanged(plan.Affected):
		return c.ansible(ctx, "external.yml", monitorCLITags)
	default:
		return nil
	}
}
