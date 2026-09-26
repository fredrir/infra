package reconciler

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/fredrir/infra/internal/reconcile"
)

const (
	OutcomeRunning    = "running"
	OutcomeApplied    = "applied"
	OutcomeEvaluated  = "evaluated"
	OutcomeRetry      = "retry"
	OutcomeDeferred   = "deferred"
	OutcomeSuperseded = "superseded"
	readinessInterval = 24 * time.Hour
	repairCooldown    = 6 * time.Hour
	retryCeiling      = 30 * time.Minute
)

type Ledger struct {
	Revision   string    `json:"revision,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
	Failure    string    `json:"failure,omitempty"`
	Full       bool      `json:"full,omitempty"`
	Repair     bool      `json:"repair,omitempty"`
	Attempts   int       `json:"attempts,omitempty"`
	RetryAt    time.Time `json:"retry_at,omitzero"`
	Started    time.Time `json:"started_at,omitzero"`
	Finished   time.Time `json:"finished_at,omitzero"`
	Checked    time.Time `json:"checked_at,omitzero"`
	Consumed   Consumed  `json:"consumed,omitzero"`
	LastRepair *Attempt  `json:"last_repair,omitempty"`
}

type Consumed struct {
	Apply  time.Time `json:"apply,omitzero"`
	Repair time.Time `json:"repair,omitzero"`
}

type Attempt struct {
	Revision string    `json:"revision"`
	Started  time.Time `json:"started_at"`
	Outcome  string    `json:"outcome"`
}

type Decision struct {
	Run      bool
	Apply    bool
	Advanced bool
	Full     bool
	Repair   bool
	Reason   string
	Consumed Consumed
}

func (l Ledger) settled() bool {
	return slices.Contains([]string{OutcomeApplied, OutcomeEvaluated, reconcile.OutcomeFailed}, l.Outcome)
}

func (l Ledger) repairCapped(revision string, now time.Time) bool {
	last := l.LastRepair
	return last != nil && last.Revision == revision && (last.Outcome == reconcile.OutcomeFailed || now.Sub(last.Started) < repairCooldown)
}

func decide(ledger Ledger, tip string, requests map[string]Request, now time.Time) Decision {
	decision := Decision{Consumed: ledger.Consumed}
	if request, ok := requests[RequestApply]; ok && request.Requested.After(ledger.Consumed.Apply) {
		decision.Consumed.Apply = request.Requested
		decision.Run, decision.Apply, decision.Full, decision.Reason = true, true, request.Full, "requested: "+request.Reason
	}
	if request, ok := requests[RequestRepair]; ok && request.Requested.After(ledger.Consumed.Repair) {
		decision.Consumed.Repair = request.Requested
		if tip != "" && request.Revision == tip && !ledger.repairCapped(tip, now) {
			decision.Run, decision.Apply, decision.Full, decision.Repair, decision.Reason = true, true, true, true, "repair: "+request.Reason
		}
	}
	unsettled := ledger.Outcome != "" && !ledger.settled()
	if unsettled {
		decision.Full = decision.Full || ledger.Full
		decision.Repair = decision.Repair || (ledger.Repair && ledger.Revision == tip)
	}
	switch {
	case decision.Apply:
	case tip == "":
	case tip != ledger.Revision:
		decision.Run, decision.Apply, decision.Advanced, decision.Reason = true, true, true, "main at "+tip
	case unsettled && !now.Before(ledger.RetryAt):
		decision.Run, decision.Apply, decision.Reason = true, true, "retry "+ledger.Outcome+" "+tip
	case now.Sub(ledger.Checked) >= readinessInterval:
		decision.Run, decision.Reason = true, "readiness"
	}
	if !decision.Apply {
		decision.Full, decision.Repair = false, false
	}
	return decision
}

func (l Ledger) begin(decision Decision, tip string, now time.Time) Ledger {
	attempts := 1
	if l.Outcome != "" && !l.settled() && l.Revision == tip {
		attempts = l.Attempts + 1
	}
	return Ledger{
		Revision: tip, Outcome: OutcomeRunning, Failure: l.Failure, Full: decision.Full, Repair: decision.Repair,
		Attempts: attempts, RetryAt: now.Add(retryDelay(attempts)), Started: now, Finished: l.Finished,
		Checked: l.Checked, Consumed: decision.Consumed, LastRepair: l.LastRepair,
	}
}

func (l Ledger) end(run applyRun, now time.Time) Ledger {
	l.Revision, l.Outcome, l.Finished, l.Checked = cmp.Or(run.Revision, l.Revision), run.Outcome, now, now
	switch run.Outcome {
	case OutcomeApplied, OutcomeEvaluated:
		l.Failure, l.Attempts, l.RetryAt = "", 0, time.Time{}
	case reconcile.OutcomeFailed:
		l.Failure, l.Attempts, l.RetryAt = fmt.Sprintf("reconciliation of %s failed at %s: %s", l.Revision, run.Stage, run.Error), 0, time.Time{}
	case OutcomeRetry:
		l.Failure, l.RetryAt = fmt.Sprintf("reconciliation of %s will retry after %s failed: %s", l.Revision, run.Stage, run.Error), now.Add(retryDelay(l.Attempts))
	default:
		l.RetryAt = now.Add(retryDelay(l.Attempts))
	}
	if l.Repair && l.settled() {
		l.LastRepair = &Attempt{Revision: l.Revision, Started: l.Started, Outcome: l.Outcome}
	}
	if l.settled() {
		l.Full, l.Repair = false, false
	}
	return l
}

func retryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	return min(time.Minute<<min(attempt-2, 5), retryCeiling)
}

func (l Ledger) failure() error {
	if l.Outcome != reconcile.OutcomeFailed && l.Outcome != OutcomeRetry {
		return nil
	}
	return errors.New(l.Failure)
}

func ledgerPath(state string) string {
	return filepath.Join(state, "ledger.json")
}

func loadLedger(state string) (Ledger, error) {
	data, err := os.ReadFile(ledgerPath(state))
	if errors.Is(err, os.ErrNotExist) {
		return Ledger{}, nil
	}
	if err != nil {
		return Ledger{}, err
	}
	var ledger Ledger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return Ledger{}, fmt.Errorf("%s: %w", ledgerPath(state), err)
	}
	return ledger, nil
}

func saveLedger(state string, ledger Ledger) error {
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(state, ".ledger-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(append(data, '\n'))
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), ledgerPath(state))
}
