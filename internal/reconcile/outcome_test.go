package reconcile

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type recordedStatus struct {
	status Status
	err    error
}

func (s recordedStatus) Read(context.Context) (Status, error) { return s.status, s.err }
func (recordedStatus) Write(context.Context, Status) error {
	return errors.New("verification wrote reconciliation status")
}
func (recordedStatus) Lock(context.Context) (func() error, error) {
	return nil, errors.New("verification locked reconciliation status")
}

type verificationOps struct {
	revision, published string
	pending             Selection
	result              error
	compared            []string
}

func (o *verificationOps) Revision(context.Context) (string, error) { return o.revision, nil }
func (o *verificationOps) PublishedRevision(context.Context) (string, error) {
	return o.published, nil
}
func (o *verificationOps) Select(_ context.Context, base string, full bool) (Selection, error) {
	if base != o.published || full {
		return Selection{}, errors.New("pending changes selected from the wrong base")
	}
	return o.pending, nil
}
func (o *verificationOps) VerifyLive(_ context.Context, plan Plan) error {
	o.compared = append(o.compared, "live "+plan.Revision)
	return o.result
}
func (o *verificationOps) VerifyDeep(_ context.Context, plan Plan) error {
	o.compared = append(o.compared, "deep "+plan.Revision)
	return o.result
}

func TestVerificationReportsIncompleteReconciliationAsDifference(t *testing.T) {
	complete := func(revision string) Status { return Status{Desired: revision, Applied: revision, Stage: "complete"} }
	unreachable := errors.New("fredrir-04 unreachable")
	for _, test := range []struct {
		name                string
		status              Status
		readErr             error
		revision, published string
		pending             Selection
		result              error
		deep                bool
		wantCompared        []string
		wantRevision        string
		wantDifferences     []Difference
		wantErrors          []string
	}{
		{name: "matches", status: complete("a"), revision: "a", published: "a", deep: true, wantCompared: []string{"deep a"}, wantRevision: "a"},
		{name: "live verification", status: complete("a"), revision: "a", published: "a", wantCompared: []string{"live a"}, wantRevision: "a"},
		{name: "apply failed before publishing", status: Status{Desired: "c", Applied: "b", Stage: "hosts", Failure: "hosts: failed"}, revision: "c", published: "b", pending: Selection{Ansible: true}, deep: true, wantDifferences: []Difference{{System: "reconciliation", Item: "applied b, desired c, stage hosts failed"}, {System: "revision", Item: "production at b, checkout at c"}}},
		{name: "apply failed after publishing", status: Status{Desired: "c", Applied: "b", Stage: "kubernetes", Failure: "kubernetes: failed"}, revision: "c", published: "c", deep: true, wantCompared: []string{"deep c"}, wantRevision: "c", wantDifferences: []Difference{{System: "reconciliation", Item: "applied b, desired c, stage kubernetes failed"}}},
		{name: "apply interrupted", status: Status{Desired: "c", Applied: "b", Stage: "publish"}, revision: "c", published: "c", deep: true, wantCompared: []string{"deep c"}, wantRevision: "c", wantDifferences: []Difference{{System: "reconciliation", Item: "applied b, desired c, stage publish"}}},
		{name: "first apply failed", status: Status{Desired: "c", Stage: "plan", Failure: "plan: failed"}, revision: "c", published: "c", deep: true, wantCompared: []string{"deep c"}, wantRevision: "c", wantDifferences: []Difference{{System: "reconciliation", Item: "applied none, desired c, stage plan failed"}}},
		{name: "drift verification failed", status: Status{Desired: "a", Applied: "a", Stage: "drift-verification", Failure: "drift-verification: failed"}, revision: "b", published: "a", pending: Selection{Tooling: true}, deep: true, wantCompared: []string{"deep a"}, wantRevision: "a", wantDifferences: []Difference{{System: "reconciliation", Item: "applied a, desired a, stage drift-verification failed"}}},
		{name: "apply superseded before it started", status: complete("b"), revision: "c", published: "b", pending: Selection{Kubernetes: true}, deep: true, wantDifferences: []Difference{{System: "revision", Item: "production at b, checkout at c"}}},
		{name: "unpublished changes deploy nothing", status: complete("b"), revision: "c", published: "b", pending: Selection{Tooling: true}, deep: true, wantCompared: []string{"deep b"}, wantRevision: "b"},
		{name: "verification error", status: complete("a"), revision: "a", published: "a", result: unreachable, deep: true, wantCompared: []string{"deep a"}, wantRevision: "a", wantErrors: []string{unreachable.Error()}},
		{name: "status unreadable", readErr: errors.New("access denied"), revision: "a", published: "a", deep: true, wantCompared: []string{"deep a"}, wantRevision: "a", wantErrors: []string{"read reconciliation status: access denied"}},
		{name: "never reconciled", revision: "a", published: "a", deep: true, wantCompared: []string{"deep a"}, wantRevision: "a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ops := &verificationOps{revision: test.revision, published: test.published, pending: test.pending, result: test.result}
			verified, err := Verifier{Store: recordedStatus{test.status, test.readErr}, Ops: ops, Host: "logs.fredrir.com", Deep: test.deep}.Verify(context.Background())
			if !reflect.DeepEqual(ops.compared, test.wantCompared) {
				t.Errorf("compared %q, want %q", ops.compared, test.wantCompared)
			}
			outcome := VerificationOutcome(verified, test.deep, err)
			want := Verification{Revision: test.wantRevision, Deep: test.deep, Outcome: OutcomeMatches, Differences: []Difference{}, Errors: []string{}}
			if test.wantDifferences != nil {
				want.Outcome, want.Differences = OutcomeDiffers, test.wantDifferences
			}
			if test.wantErrors != nil {
				want.Errors = test.wantErrors
				if test.wantDifferences == nil {
					want.Outcome = OutcomeFailed
				}
			}
			if !reflect.DeepEqual(outcome, want) {
				t.Fatalf("outcome %+v, want %+v", outcome, want)
			}
		})
	}
}
