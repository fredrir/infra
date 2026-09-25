package reconcile

import (
	"context"
	"fmt"
)

func (c *Commands) PlanHosts(ctx context.Context, plan Plan) error {
	var playbooks []string
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		if _, err := LoadRunnerFleet(c.Runner.Dir); err != nil {
			return err
		}
		playbooks = []string{"reconcile.yml", "external.yml", "verify.yml", "verify-runners.yml"}
	case HostScopeRunners:
		if _, err := LoadRunnerFleet(c.Runner.Dir); err != nil {
			return err
		}
		playbooks = []string{"build-runners.yml", "verify-runners.yml"}
	case HostScopeMonitor:
		playbooks = []string{"external.yml", "verify.yml"}
	}
	for _, playbook := range playbooks {
		for _, check := range []string{"--syntax-check", "--list-tasks"} {
			args := []string{check}
			if effectiveHostScope(plan.Affected) == HostScopeMonitor {
				if playbook == "external.yml" {
					args = append(args, "--tags=gatus")
				} else {
					args = append(args, "--limit=external")
				}
			}
			if err := c.ansible(ctx, playbook, args...); err != nil {
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
	run, err := c.recordPlaybooks(ctx, comparedPlaybooks, "--check", "--skip-tags=runners")
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err == nil && !run.Reported {
		err = fmt.Errorf("host comparison recorded no task results")
	}
	var differences Differences
	var failed []string
	for _, task := range run.Tasks {
		if task.Failed {
			failed = append(failed, "["+task.Host+"] "+task.Task)
		} else {
			differences = append(differences, Difference{System: "hosts", Host: task.Host, Item: task.Task})
		}
	}
	return run.outcome(differences, failed, err)
}

func (c *Commands) verifyRunnerHosts(ctx context.Context, playbooks ...string) error {
	fleet, err := LoadRunnerFleet(c.Runner.Dir)
	if err != nil {
		return err
	}
	run, err := c.recordPlaybooks(ctx, playbooks)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var differences Differences
	var failed []string
	for _, task := range run.Tasks {
		switch {
		case task.Playbook == "verify-runners":
			differences = append(differences, Difference{System: "runners", Host: task.Host, Item: task.Task})
		case task.Failed:
			failed = append(failed, "["+task.Host+"] "+task.Task)
		}
	}
	if err := run.outcome(differences, failed, err); err != nil {
		return err
	}
	return c.verifyRunnerFleet(ctx, fleet)
}

func (c *Commands) Monitor(ctx context.Context, plan Plan) error {
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		return c.ansible(ctx, "external.yml")
	case HostScopeMonitor:
		return c.ansible(ctx, "external.yml", "--tags=gatus")
	default:
		return nil
	}
}
