package reconcile

import "context"

func (c *Commands) PlanHosts(ctx context.Context, plan Plan) error {
	var playbooks []string
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		playbooks = []string{"reconcile.yml", "external.yml", "verify.yml", "verify-runners.yml"}
	case HostScopeRunners:
		playbooks = []string{"build-runners.yml", "verify-runners.yml"}
	case HostScopeMonitor:
		playbooks = []string{"external.yml", "verify.yml"}
	}
	if scope := effectiveHostScope(plan.Affected); scope == HostScopeFull || scope == HostScopeRunners {
		if _, err := LoadRunnerFleet(c.Runner.Dir); err != nil {
			return err
		}
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
		return c.ansible(ctx, "reconcile.yml")
	case HostScopeRunners:
		return c.ansible(ctx, "build-runners.yml")
	default:
		return nil
	}
}

func (c *Commands) VerifyHosts(ctx context.Context, plan Plan) error {
	var err error
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		err = c.ansible(ctx, "verify.yml", "verify-runners.yml")
	case HostScopeRunners:
		err = c.ansible(ctx, "verify-runners.yml")
	case HostScopeMonitor:
		return c.ansible(ctx, "verify.yml", "--limit=external")
	default:
		return nil
	}
	if err != nil {
		return err
	}
	fleet, err := LoadRunnerFleet(c.Runner.Dir)
	if err != nil {
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
