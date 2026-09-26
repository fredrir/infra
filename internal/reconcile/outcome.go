package reconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

type Difference struct {
	System string `json:"system"`
	Host   string `json:"host,omitempty"`
	Item   string `json:"item"`
}

type Differences []Difference

func (d Differences) Error() string {
	items := make([]string, 0, len(d))
	for _, difference := range d {
		item := difference.System + ": "
		if difference.Host != "" {
			item += difference.Host + ": "
		}
		items = append(items, item+difference.Item)
	}
	return "production differs from its declaration: " + strings.Join(items, "; ")
}

const (
	OutcomeMatches = "matches"
	OutcomeDiffers = "differs"
	OutcomeFailed  = "failed"
)

type Scope string

const (
	ScopeCloud Scope = "cloud"
	ScopeFull  Scope = "full"
)

func ParseScope(value string) (Scope, error) {
	switch scope := Scope(value); scope {
	case ScopeCloud, ScopeFull:
		return scope, nil
	default:
		return "", fmt.Errorf("verification scope %q is not %s or %s", value, ScopeCloud, ScopeFull)
	}
}

type Verification struct {
	Revision    string       `json:"revision,omitempty"`
	Scope       Scope        `json:"scope"`
	Outcome     string       `json:"outcome"`
	Differences []Difference `json:"differences"`
	Errors      []string     `json:"errors"`
	Degraded    []string     `json:"degraded,omitempty"`
}

func VerificationOutcome(revision string, scope Scope, err error) Verification {
	verification := Verification{Revision: revision, Scope: scope, Differences: []Difference{}, Errors: []string{}}
	var degrade func(error)
	degrade = func(err error) {
		switch err := err.(type) {
		case nil:
		case Differences:
			for _, difference := range err {
				verification.Degraded = append(verification.Degraded, Differences{difference}.Error())
			}
		case interface{ Unwrap() []error }:
			for _, inner := range err.Unwrap() {
				degrade(inner)
			}
		default:
			verification.Degraded = append(verification.Degraded, err.Error())
		}
	}
	var visit func(error)
	visit = func(err error) {
		switch err := err.(type) {
		case nil:
		case Degraded:
			degrade(err.Err)
		case Differences:
			for _, difference := range err {
				if !slices.Contains(verification.Differences, difference) {
					verification.Differences = append(verification.Differences, difference)
				}
			}
		case interface{ Unwrap() []error }:
			for _, inner := range err.Unwrap() {
				visit(inner)
			}
		default:
			if !slices.Contains(verification.Errors, err.Error()) {
				verification.Errors = append(verification.Errors, err.Error())
			}
		}
	}
	visit(err)
	switch {
	case len(verification.Differences) > 0:
		verification.Outcome = OutcomeDiffers
	case len(verification.Errors) > 0:
		verification.Outcome = OutcomeFailed
	default:
		verification.Outcome = OutcomeMatches
	}
	return verification
}

type VerificationOperations interface {
	Revision(context.Context) (string, error)
	PublishedRevision(context.Context) (string, error)
	OnMain(context.Context, string) (bool, error)
	VerifyRulesets(context.Context) error
	Select(context.Context, string, bool) (Selection, error)
	VerifyCloud(context.Context, Plan) error
	VerifyFull(context.Context, Plan) error
}

type VerificationStore interface {
	Read(context.Context) (Status, error)
	Unlocked(context.Context) error
}

type Verifier struct {
	Store VerificationStore
	Ops   VerificationOperations
	Host  string
	Scope Scope
}

func (v Verifier) Verify(ctx context.Context) (string, error) {
	if _, err := ParseScope(string(v.Scope)); err != nil {
		return "", err
	}
	if err := v.Store.Unlocked(ctx); err != nil {
		return "", fmt.Errorf("comparisons skipped: %w", err)
	}
	revision, err := v.compare(ctx)
	if locked := v.Store.Unlocked(ctx); locked != nil {
		return "", fmt.Errorf("comparisons discarded: %w", locked)
	}
	return revision, err
}

func (v Verifier) compare(ctx context.Context) (string, error) {
	standing := errors.Join(v.recordedReconciliation(ctx), v.Ops.VerifyRulesets(ctx))
	revision, err := v.Ops.Revision(ctx)
	if err != nil {
		return "", errors.Join(standing, err)
	}
	published, err := v.Ops.PublishedRevision(ctx)
	if err != nil {
		return "", errors.Join(standing, err)
	}
	onMain, err := v.Ops.OnMain(ctx, published)
	if err != nil {
		return "", errors.Join(standing, fmt.Errorf("production ancestry: %w", err))
	}
	if !onMain {
		return "", errors.Join(standing, Differences{{System: "revision", Item: "production at " + published + " is not on main"}})
	}
	if published != revision {
		pending, err := v.Ops.Select(ctx, published, false)
		if err != nil {
			return "", errors.Join(standing, err)
		}
		if pending.Tofu || pending.Kubernetes || pending.Ansible {
			return "", errors.Join(standing, Differences{{System: "revision", Item: "production at " + published + ", checkout at " + revision}})
		}
		revision = published
	}
	plan := Plan{Revision: revision, Affected: All(), Host: v.Host}
	verify := v.Ops.VerifyCloud
	if v.Scope == ScopeFull {
		verify = v.Ops.VerifyFull
	}
	return revision, errors.Join(standing, verify(ctx, plan))
}

func (v Verifier) recordedReconciliation(ctx context.Context) error {
	status, err := v.Store.Read(ctx)
	if err != nil {
		return fmt.Errorf("read reconciliation status: %w", err)
	}
	if !status.NeedsRecovery() {
		return nil
	}
	item := fmt.Sprintf("applied %s, desired %s, stage %s", cmp.Or(status.Applied, "none"), status.Desired, status.Stage)
	if status.Failure != "" {
		item += " failed"
	}
	return Differences{{System: "reconciliation", Item: item}}
}
