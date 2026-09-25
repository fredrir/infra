package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const volatilePlaybook = "volatile.yml"

type Degraded struct{ Err error }

func (d Degraded) Error() string { return d.Err.Error() }

func (d Degraded) Unwrap() error { return d.Err }

func WithoutDegraded(err error) error {
	switch err := err.(type) {
	case nil, Degraded:
		return nil
	case interface{ Unwrap() []error }:
		var kept []error
		for _, inner := range err.Unwrap() {
			if inner = WithoutDegraded(inner); inner != nil {
				kept = append(kept, inner)
			}
		}
		return errors.Join(kept...)
	default:
		return err
	}
}

func (c *Commands) Volatile(ctx context.Context, plan Plan) error {
	if effectiveHostScope(plan.Affected) != HostScopeFull {
		return nil
	}
	run := c.recordPlaybook(ctx, volatilePlaybook)
	var failed []string
	for _, task := range run.Tasks {
		if task.Failed {
			failed = append(failed, "["+task.Host+"] "+task.Task)
		}
	}
	var problems []error
	if len(failed) > 0 {
		problems = append(problems, fmt.Errorf("%s host tasks failed: %s", volatilePlaybook, strings.Join(failed, "; ")))
	}
	if len(run.Unreachable) > 0 {
		problems = append(problems, fmt.Errorf("%s unreachable hosts: %s", volatilePlaybook, strings.Join(run.Unreachable, ", ")))
	}
	if run.Err != nil && len(problems) == 0 {
		problems = append(problems, fmt.Errorf("%s: %w", volatilePlaybook, run.Err))
	}
	return errors.Join(problems...)
}

func (c *Commands) compareVolatile(ctx context.Context) error {
	run := c.recordPlaybook(ctx, volatilePlaybook, "--check")
	if !run.Reported && run.Err == nil {
		return nil
	}
	var differences Differences
	var failed []string
	for _, task := range run.Tasks {
		if task.Failed {
			failed = append(failed, "["+task.Host+"] "+task.Task)
		} else {
			differences = append(differences, Difference{System: "volatile", Host: task.Host, Item: task.Task})
		}
	}
	if err := run.outcome(differences, failed); err != nil {
		return Degraded{err}
	}
	return nil
}
