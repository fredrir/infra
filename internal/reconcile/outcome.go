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

type Verification struct {
	Revision    string       `json:"revision,omitempty"`
	Deep        bool         `json:"deep"`
	Outcome     string       `json:"outcome"`
	Differences []Difference `json:"differences"`
	Errors      []string     `json:"errors"`
}

func VerificationOutcome(revision string, deep bool, err error) Verification {
	verification := Verification{Revision: revision, Deep: deep, Differences: []Difference{}, Errors: []string{}}
	var visit func(error)
	visit = func(err error) {
		switch err := err.(type) {
		case nil:
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
	Select(context.Context, string, bool) (Selection, error)
	VerifyLive(context.Context, Plan) error
	VerifyDeep(context.Context, Plan) error
}

type Verifier struct {
	Store Store
	Ops   VerificationOperations
	Host  string
	Deep  bool
}

func (v Verifier) Verify(ctx context.Context) (string, error) {
	recorded := v.recordedReconciliation(ctx)
	revision, err := v.Ops.Revision(ctx)
	if err != nil {
		return "", errors.Join(recorded, err)
	}
	published, err := v.Ops.PublishedRevision(ctx)
	if err != nil {
		return "", errors.Join(recorded, err)
	}
	if published != revision {
		pending, err := v.Ops.Select(ctx, published, false)
		if err != nil {
			return "", errors.Join(recorded, err)
		}
		if pending.Tofu || pending.Kubernetes || pending.Ansible {
			return "", errors.Join(recorded, Differences{{System: "revision", Item: "production at " + published + ", checkout at " + revision}})
		}
		revision = published
	}
	plan := Plan{Revision: revision, Affected: All(), Host: v.Host}
	compare := v.Ops.VerifyLive
	if v.Deep {
		compare = v.Ops.VerifyDeep
	}
	return revision, errors.Join(recorded, compare(ctx, plan))
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
