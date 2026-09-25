package reconcile

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

var comparedPlaybooks = []string{"reconcile.yml", "external.yml"}

func (c *Commands) compareHosts(ctx context.Context) error {
	reports, err := os.MkdirTemp(c.Work, "host-comparison-")
	if err != nil {
		return err
	}
	compare := Commands{Runner: c.Runner}
	compare.Runner.Env = append(slices.Clone(c.Runner.Env), "ANSIBLE_CALLBACKS_ENABLED=ansible.builtin.junit", "JUNIT_OUTPUT_DIR="+reports, "JUNIT_FAIL_ON_CHANGE=true", "JUNIT_HIDE_TASK_ARGUMENTS=true")
	run := compare.ansible(ctx, comparedPlaybooks[0], append(slices.Clone(comparedPlaybooks[1:]), "--check", "--skip-tags=runners")...)
	if err := ctx.Err(); err != nil {
		return err
	}
	differences, err := hostDifferences(reports)
	if err != nil {
		return errors.Join(run, err)
	}
	if len(differences) > 0 {
		return errors.Join(run, fmt.Errorf("hosts differ from their declarations: %s", strings.Join(differences, "; ")))
	}
	return run
}

func hostDifferences(reports string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(reports, "*.xml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("host comparison recorded no task results")
	}
	var differences []string
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var report struct {
			Suites []struct {
				Cases []struct {
					Name     string     `xml:"name,attr"`
					Failures []xml.Name `xml:"failure"`
					Errors   []xml.Name `xml:"error"`
				} `xml:"testcase"`
			} `xml:"testsuite"`
		}
		if err := xml.Unmarshal(data, &report); err != nil {
			return nil, fmt.Errorf("host comparison report %s: %w", filepath.Base(file), err)
		}
		for _, suite := range report.Suites {
			for _, task := range suite.Cases {
				if len(task.Failures) > 0 || len(task.Errors) > 0 {
					differences = append(differences, task.Name)
				}
			}
		}
	}
	slices.Sort(differences)
	return slices.Compact(differences), nil
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
