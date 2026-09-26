package reconciler

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/reconcile"
)

func TestDecideRunsForNewTipsRetriesAndCappedRepairs(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	tip, previous := strings.Repeat("b", 40), strings.Repeat("a", 40)
	checked := now.Add(-time.Hour)
	applied := Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked}
	repair := map[string]Request{RequestRepair: {Kind: RequestRepair, Revision: tip, Full: true, Reason: "1 differences", Requested: now}}
	for _, test := range []struct {
		name     string
		ledger   Ledger
		tip      string
		requests map[string]Request
		want     Decision
	}{
		{name: "new tip", ledger: Ledger{Revision: previous, Outcome: OutcomeApplied, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Advanced: true, Reason: "main at " + tip}},
		{name: "first run", tip: tip, want: Decision{Run: true, Apply: true, Advanced: true, Reason: "main at " + tip}},
		{name: "applied tip", ledger: applied, tip: tip, want: Decision{}},
		{name: "evaluated tip", ledger: Ledger{Revision: tip, Outcome: OutcomeEvaluated, Checked: checked}, tip: tip, want: Decision{}},
		{name: "failed tip waits", ledger: Ledger{Revision: tip, Outcome: reconcile.OutcomeFailed, Checked: checked}, tip: tip, want: Decision{}},
		{name: "deferred tip retries", ledger: Ledger{Revision: tip, Outcome: OutcomeDeferred, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Reason: "retry deferred " + tip}},
		{name: "superseded tip retries", ledger: Ledger{Revision: tip, Outcome: OutcomeSuperseded, Checked: checked}, tip: tip, want: Decision{Run: true, Apply: true, Reason: "retry superseded " + tip}},
		{name: "unknown tip", ledger: applied, want: Decision{}},
		{name: "readiness", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: now.Add(-readinessInterval)}, tip: tip, want: Decision{Run: true, Reason: "readiness"}},
		{name: "operator request", ledger: applied, tip: tip, requests: map[string]Request{RequestApply: {Kind: RequestApply, Full: true, Reason: "operator", Requested: now}}, want: Decision{Run: true, Apply: true, Full: true, Reason: "requested: operator", Consumed: []string{RequestApply}}},
		{name: "operator request without a tip", ledger: applied, requests: map[string]Request{RequestApply: {Kind: RequestApply, Reason: "operator", Requested: now}}, want: Decision{Run: true, Apply: true, Reason: "requested: operator", Consumed: []string{RequestApply}}},
		{name: "repair", ledger: applied, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: 1 differences", Consumed: []string{RequestRepair}}},
		{name: "repair of a failed apply", ledger: Ledger{Revision: tip, Outcome: reconcile.OutcomeFailed, Checked: checked}, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: 1 differences", Consumed: []string{RequestRepair}}},
		{name: "repair after a failed repair", ledger: Ledger{Revision: tip, Outcome: reconcile.OutcomeFailed, Checked: checked, Repair: &Attempt{Revision: tip, Started: now.Add(-7 * time.Hour), Outcome: reconcile.OutcomeFailed}}, tip: tip, requests: repair, want: Decision{Consumed: []string{RequestRepair}}},
		{name: "repair within six hours", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked, Repair: &Attempt{Revision: tip, Started: now.Add(-5 * time.Hour), Outcome: OutcomeApplied}}, tip: tip, requests: repair, want: Decision{Consumed: []string{RequestRepair}}},
		{name: "repair after six hours", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked, Repair: &Attempt{Revision: tip, Started: now.Add(-repairCooldown), Outcome: OutcomeApplied}}, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: 1 differences", Consumed: []string{RequestRepair}}},
		{name: "repair cap resets on a new commit", ledger: Ledger{Revision: tip, Outcome: OutcomeApplied, Checked: checked, Repair: &Attempt{Revision: previous, Started: now.Add(-time.Hour), Outcome: reconcile.OutcomeFailed}}, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: 1 differences", Consumed: []string{RequestRepair}}},
		{name: "repair of an older main", ledger: Ledger{Revision: previous, Outcome: OutcomeApplied, Checked: checked}, tip: tip, requests: repair, want: Decision{Run: true, Apply: true, Advanced: true, Reason: "main at " + tip, Consumed: []string{RequestRepair}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := decide(test.ledger, test.tip, test.requests, now); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("decided %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestLedgerSurvivesARoundTripAndStartsEmpty(t *testing.T) {
	state := t.TempDir()
	if ledger, err := loadLedger(state); err != nil || !reflect.DeepEqual(ledger, Ledger{}) {
		t.Fatalf("empty state loaded %+v, %v", ledger, err)
	}
	want := Ledger{Revision: strings.Repeat("c", 40), Outcome: reconcile.OutcomeFailed, Failure: "hosts: unreachable", Finished: time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC), Checked: time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC), Repair: &Attempt{Revision: strings.Repeat("c", 40), Started: time.Date(2026, 9, 26, 0, 30, 0, 0, time.UTC), Outcome: reconcile.OutcomeFailed}}
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
	if !slices.Contains([]string{OutcomeApplied, OutcomeEvaluated, reconcile.OutcomeFailed}, want.Outcome) || !want.settled() {
		t.Fatal("a failed reconciliation is not settled")
	}
}
