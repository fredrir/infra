package reconciler

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/reconcile"
)

func TestDecideRunsForNewTipsRetriesRequestsAndCappedRepairs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	tip, previous := strings.Repeat("b", 40), strings.Repeat("a", 40)
	checked, earlier := now.Add(-time.Hour), now.Add(-2*time.Hour)
	applied := Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked}
	repair := map[string]Request{RequestRepair: {Kind: RequestRepair, Revision: tip, Full: true, Reason: "1 differences", Requested: now}}
	consumedRepair := Consumed{Repair: now}
	operator := map[string]Request{RequestApply: {Kind: RequestApply, Full: true, Reason: "operator", Requested: now}}
	for _, test := range []struct {
		name     string
		ledger   Ledger
		tip      string
		requests map[string]Request
		want     Decision
	}{
		{name: "new tip", ledger: Ledger{Revision: previous, Outcome: OutcomeApplied, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Advanced: true, Reason: "main at " + tip}},
		{name: "first run", tip: tip, want: Decision{Run: true, Apply: true, Advanced: true, Reason: "main at " + tip}},
		{name: "applied tip", ledger: applied, tip: tip},
		{name: "evaluated tip", ledger: Ledger{Revision: tip, Outcome: OutcomeEvaluated, Checked: checked}, tip: tip},
		{name: "failed tip waits", ledger: Ledger{Revision: tip, Outcome: reconcile.OutcomeFailed, Checked: checked}, tip: tip},
		{name: "deferred tip retries", ledger: Ledger{Revision: tip, Outcome: OutcomeDeferred, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Reason: "retry deferred " + tip}},
		{name: "superseded tip retries", ledger: Ledger{Revision: tip, Outcome: OutcomeSuperseded, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Reason: "retry superseded " + tip}},
		{name: "crashed run retries", ledger: Ledger{Revision: tip, Outcome: OutcomeRunning, Full: true, Repair: true, RetryAt: now, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "retry running " + tip}},
		{name: "transient failure retries when due", ledger: Ledger{Revision: tip, Outcome: OutcomeRetry, RetryAt: now, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Reason: "retry retry " + tip}},
		{name: "transient failure backs off", ledger: Ledger{Revision: tip, Outcome: OutcomeRetry, RetryAt: now.Add(time.Minute), Checked: checked}, tip: tip},
		{name: "deferred full apply keeps full", ledger: Ledger{Revision: tip, Outcome: OutcomeDeferred, Full: true, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Full: true, Reason: "retry deferred " + tip}},
		{name: "deferred full apply keeps full on a new tip", ledger: Ledger{Revision: previous, Outcome: OutcomeDeferred, Full: true, Repair: true, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Advanced: true, Full: true, Reason: "main at " + tip}},
		{name: "settled full apply does not carry", ledger: Ledger{Revision: previous, Outcome: OutcomeApplied, Full: true, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Advanced: true, Reason: "main at " + tip}},
		{name: "unknown tip", ledger: applied},
		{name: "readiness", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: now.Add(-readinessInterval)}, tip: tip, want: Decision{Run: true, Reason: "readiness"}},
		{name: "operator request", ledger: applied, tip: tip, requests: operator, want: Decision{Run: true, Apply: true, Full: true, Reason: "requested: operator", Consumed: Consumed{Apply: now}}},
		{name: "operator request without a tip", ledger: applied, requests: operator, want: Decision{Run: true, Apply: true, Full: true, Reason: "requested: operator", Consumed: Consumed{Apply: now}}},
		{name: "consumed operator request", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked, Consumed: Consumed{Apply: now}}, tip: tip, requests: operator, want: Decision{Consumed: Consumed{Apply: now}}},
		{name: "repair", ledger: applied, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: 1 differences", Consumed: consumedRepair}},
		{name: "consumed repair", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked, Consumed: consumedRepair}, tip: tip, requests: repair, want: Decision{Consumed: consumedRepair}},
		{name: "repair of a failed apply", ledger: Ledger{Revision: tip, Outcome: reconcile.OutcomeFailed, Checked: checked}, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: 1 differences", Consumed: consumedRepair}},
		{name: "repair after a failed repair", ledger: Ledger{Revision: tip, Outcome: reconcile.OutcomeFailed, Checked: checked, LastRepair: &Attempt{Revision: tip, Started: now.Add(-7 * time.Hour), Outcome: reconcile.OutcomeFailed}}, tip: tip, requests: repair, want: Decision{Consumed: consumedRepair}},
		{name: "repair within six hours", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked, LastRepair: &Attempt{Revision: tip, Started: now.Add(-5 * time.Hour), Outcome: OutcomeApplied}}, tip: tip, requests: repair, want: Decision{Consumed: consumedRepair}},
		{name: "repair after six hours", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked, LastRepair: &Attempt{Revision: tip, Started: now.Add(-repairCooldown), Outcome: OutcomeApplied}}, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: 1 differences", Consumed: consumedRepair}},
		{name: "repair cap resets on a new commit", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked, LastRepair: &Attempt{Revision: previous, Started: now.Add(-time.Hour), Outcome: reconcile.OutcomeFailed}}, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: 1 differences", Consumed: consumedRepair}},
		{name: "repair of a tip not yet handled", ledger: Ledger{Revision: previous, Outcome: OutcomeApplied, Checked: checked}, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: 1 differences", Consumed: consumedRepair}},
		{name: "repair of an older main", ledger: Ledger{Revision: previous, Outcome: OutcomeApplied, Checked: checked}, tip: tip, requests: map[string]Request{RequestRepair: {Kind: RequestRepair, Revision: previous, Full: true, Reason: "drift", Requested: earlier}}, want: Decision{Run: true, Apply: true, Advanced: true, Reason: "main at " + tip, Consumed: Consumed{Repair: earlier}}},
		{name: "deferred repair keeps full and repair", ledger: Ledger{Revision: tip, Outcome: OutcomeDeferred, Full: true, Repair: true, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "retry deferred " + tip}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := decide(test.ledger, test.tip, test.requests, nil, now); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("decided %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestDecideIgnoresStaleRequestsAndQuarantinesInvalidOnesOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	tip := strings.Repeat("b", 40)
	applied := Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: now.Add(-time.Hour)}
	stale := map[string]Request{
		RequestApply:  {Kind: RequestApply, Full: true, Reason: "operator", Requested: now.Add(-requestLifetime)},
		RequestRepair: {Kind: RequestRepair, Revision: tip, Full: true, Reason: "drift", Requested: now.Add(-requestLifetime - time.Minute)},
	}
	if got := decide(applied, tip, stale, nil, now); !reflect.DeepEqual(got, Decision{}) {
		t.Fatalf("stale requests decided %+v", got)
	}
	invalid := map[string]invalidRequest{RequestApply: {Fingerprint: strings.Repeat("f", 64), Reason: "apply request: unexpected EOF"}}
	got := decide(applied, tip, nil, invalid, now)
	if want := (Decision{Run: true, Reason: "quarantine an invalid apply request", Quarantine: invalid}); !reflect.DeepEqual(got, want) {
		t.Fatalf("decided %+v, want %+v", got, want)
	}
	quarantined := applied.quarantine(got.Quarantine)
	if got := decide(quarantined, tip, nil, invalid, now); !reflect.DeepEqual(got, Decision{}) {
		t.Fatalf("a quarantined request decided %+v", got)
	}
	if applied.Quarantined != nil || quarantined.Quarantined[RequestApply] != strings.Repeat("f", 64) {
		t.Fatalf("quarantine changed the original ledger or missed the fingerprint: %+v", quarantined)
	}
	replaced := map[string]invalidRequest{RequestApply: {Fingerprint: strings.Repeat("e", 64), Reason: "apply request: unexpected EOF"}}
	if got := decide(quarantined, tip, nil, replaced, now); !got.Run || got.Quarantine == nil {
		t.Fatalf("a replaced invalid request decided %+v", got)
	}
	if got := decide(Ledger{Revision: strings.Repeat("a", 40), Outcome: OutcomeApplied, Checked: now}, tip, nil, invalid, now); !got.Apply || got.Reason != "main at "+tip || got.Quarantine == nil {
		t.Fatalf("a new tip with an invalid request decided %+v", got)
	}
}

func TestLedgerRecordsTheAttemptAndItsOutcome(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	tip := strings.Repeat("c", 40)
	repair := Decision{Run: true, Apply: true, Full: true, Repair: true, Consumed: Consumed{Repair: now}}
	started := Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: now.Add(-time.Hour)}.begin(repair, tip, now)
	if started.Outcome != OutcomeRunning || !started.Full || !started.Repair || started.Attempts != 1 || !started.RetryAt.Equal(now) || started.Consumed != repair.Consumed {
		t.Fatalf("started %+v", started)
	}
	for _, test := range []struct {
		name       string
		outcome    string
		wantRepair bool
		wantFull   bool
	}{
		{name: "applied", outcome: OutcomeApplied, wantRepair: true},
		{name: "evaluated", outcome: OutcomeEvaluated, wantRepair: true},
		{name: "failed", outcome: reconcile.OutcomeFailed, wantRepair: true},
		{name: "deferred", outcome: OutcomeDeferred, wantFull: true},
		{name: "superseded", outcome: OutcomeSuperseded, wantFull: true},
		{name: "retry", outcome: OutcomeRetry, wantFull: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ended := started.end(applyRun{Run: Run{Revision: tip, Outcome: test.outcome, Stage: "hosts", Error: "fredrir-09 unreachable"}}, now.Add(time.Minute))
			if (ended.LastRepair != nil) != test.wantRepair || ended.Full != test.wantFull || ended.Repair != test.wantFull {
				t.Fatalf("ended %+v", ended)
			}
			failure := ended.failure()
			switch test.outcome {
			case reconcile.OutcomeFailed:
				if failure == nil || failure.Error() != "reconciliation of "+tip+" failed at hosts: fredrir-09 unreachable" || ended.Attempts != 0 {
					t.Errorf("failure %v, attempts %d", failure, ended.Attempts)
				}
			case OutcomeRetry:
				if failure == nil || !strings.Contains(failure.Error(), "will retry after hosts failed") || ended.Attempts != 1 || !ended.RetryAt.Equal(now.Add(time.Minute)) {
					t.Errorf("failure %v, attempts %d, retry at %s", failure, ended.Attempts, ended.RetryAt)
				}
			default:
				if failure != nil {
					t.Errorf("failure %v", failure)
				}
			}
		})
	}
	retried := started.end(applyRun{Run: Run{Revision: tip, Outcome: OutcomeRetry, Stage: "build", Error: "proxy.golang.org: 503"}}, now)
	for attempt, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		retried = retried.begin(decide(retried, tip, nil, nil, retried.RetryAt), tip, retried.RetryAt)
		if retried.Attempts != attempt+2 {
			t.Fatalf("attempt %d recorded as %d", attempt+2, retried.Attempts)
		}
		finished := retried.Started
		retried = retried.end(applyRun{Run: Run{Revision: tip, Outcome: OutcomeRetry, Stage: "build", Error: "proxy.golang.org: 503"}}, finished)
		if got := retried.RetryAt.Sub(finished); got != delay {
			t.Fatalf("attempt %d backs off %s, want %s", retried.Attempts, got, delay)
		}
	}
	if settled := retried.end(applyRun{Run: Run{Revision: tip, Outcome: OutcomeApplied}}, now); settled.Attempts != 0 || !settled.RetryAt.IsZero() || settled.Failure != "" {
		t.Fatalf("a success keeps the retry state: %+v", settled)
	}
}

func TestPushIgnoredCommitKeepsTheFailedRevisionInItsFailure(t *testing.T) {
	t.Parallel()
	failed := Ledger{Revision: strings.Repeat("d", 40), Outcome: reconcile.OutcomeFailed, Failure: "reconciliation of " + strings.Repeat("d", 40) + " failed at apply: hosts"}
	failed.Revision = strings.Repeat("e", 40)
	if err := failed.failure(); err == nil || !strings.Contains(err.Error(), strings.Repeat("d", 40)) {
		t.Fatalf("failure %v", err)
	}
}

func TestLedgerSurvivesARoundTripAndStartsEmpty(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	if ledger, err := loadLedger(state); err != nil || !reflect.DeepEqual(ledger, Ledger{}) {
		t.Fatalf("empty state loaded %+v, %v", ledger, err)
	}
	at := time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)
	want := Ledger{Revision: strings.Repeat("c", 40), Outcome: OutcomeRetry, Failure: "retry", Full: true, Repair: true, Attempts: 3, RetryAt: at, Started: at, Finished: at, Checked: at, Consumed: Consumed{Apply: at, Repair: at}, LastRepair: &Attempt{Revision: strings.Repeat("c", 40), Started: at, Outcome: reconcile.OutcomeFailed}}
	if err := saveLedger(state, want); err != nil {
		t.Fatal(err)
	}
	if got, err := loadLedger(state); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded %+v, %v; want %+v", got, err, want)
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 1 || entries[0].Name() != "ledger.json" {
		t.Fatalf("state holds %v: %v", entries, err)
	}
	if err := os.WriteFile(ledgerPath(state), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLedger(state); err == nil {
		t.Fatal("a corrupt ledger loaded")
	}
}
