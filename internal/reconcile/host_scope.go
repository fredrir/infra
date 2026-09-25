package reconcile

import "context"

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
		if !plan.RunnersUnchanged {
			return c.convergeRunners(ctx, plan, "reconcile.yml")
		}
		if err := c.ansible(ctx, "reconcile.yml", "--skip-tags=runners"); err != nil {
			return err
		}
		return c.convergeRunners(ctx, plan, "build-runners.yml")
	case HostScopeRunners:
		return c.convergeRunners(ctx, plan, "build-runners.yml")
	default:
		return nil
	}
}

func (c *Commands) VerifyHosts(ctx context.Context, plan Plan) error {
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		if c.runnersVerified {
			return c.ansible(ctx, "verify.yml")
		}
		return c.verifyRunnerHosts(ctx, "verify.yml", "verify-runners.yml")
	case HostScopeRunners:
		if c.runnersVerified {
			return nil
		}
		return c.verifyRunnerHosts(ctx, "verify-runners.yml")
	case HostScopeMonitor:
		return c.ansible(ctx, "verify.yml", "--limit=external")
	default:
		return nil
	}
}

func (c *Commands) verifyRunnerHosts(ctx context.Context, playbook string, extra ...string) error {
	fleet, err := LoadRunnerFleet(c.Runner.Dir)
	if err != nil {
		return err
	}
	if err := c.ansible(ctx, playbook, extra...); err != nil {
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
