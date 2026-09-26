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

func convergesPlaybook(selected Selection, playbook string) bool {
	return effectiveHostScope(selected) == HostScopeFull && (len(selected.HostPlaybooks) == 0 || slices.Contains(selected.HostPlaybooks, playbook))
}

func monitorSelected(selected Selection) bool {
	return effectiveHostScope(selected) == HostScopeMonitor || monitorCLIChanged(selected) || convergesPlaybook(selected, monitorPlaybook)
}

func convergencePlaybooks(selected Selection) []string {
	if len(selected.HostPlaybooks) == 0 {
		return []string{convergencePlaybook}
	}
	playbooks := []string{factsPlaybook}
	for _, playbook := range selected.HostPlaybooks {
		if playbook != monitorPlaybook && playbook != volatilePlaybook {
			playbooks = append(playbooks, playbook)
		}
	}
	return playbooks
}

func (c *Commands) PlanHosts(ctx context.Context, plan Plan) error {
	var playbooks [][]string
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		if _, err := LoadRunnerFleet(c.Runner.Dir); err != nil {
			return err
		}
		playbooks = [][]string{{"reconcile.yml"}, {"external.yml"}, {"verify.yml"}, {"verify-runners.yml"}, {volatilePlaybook}}
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
		return c.convergeRunners(ctx, plan, convergencePlaybooks(plan.Affected))
	case HostScopeRunners:
		return c.convergeRunners(ctx, plan, []string{runnerPlaybook})
	default:
		return nil
	}
}

func (c *Commands) VerifyHosts(ctx context.Context, plan Plan) error {
	switch effectiveHostScope(plan.Affected) {
	case HostScopeFull:
		if !convergesPlaybook(plan.Affected, runnerPlaybook) && !c.repairedRunners {
			return c.verifyRunnerHosts(ctx, "verify.yml")
		}
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
	registrations := make(chan error, 1)
	go func() { registrations <- c.verifyRunnerFleet(ctx, fleet) }()
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
	return errors.Join(append(problems, <-registrations)...)
}

func (c *Commands) Monitor(ctx context.Context, plan Plan) error {
	switch {
	case effectiveHostScope(plan.Affected) == HostScopeFull:
		if !convergesPlaybook(plan.Affected, monitorPlaybook) {
			return nil
		}
		return c.ansible(ctx, "external.yml")
	case effectiveHostScope(plan.Affected) == HostScopeMonitor:
		return c.ansible(ctx, "external.yml", monitorTags)
	case monitorCLIChanged(plan.Affected):
		return c.ansible(ctx, "external.yml", monitorCLITags)
	default:
		return nil
	}
}
