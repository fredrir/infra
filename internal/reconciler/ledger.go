package reconciler

import (
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
	OutcomeApplied    = "applied"
	OutcomeEvaluated  = "evaluated"
	OutcomeDeferred   = "deferred"
	OutcomeSuperseded = "superseded"
	readinessInterval = 24 * time.Hour
	repairCooldown    = 6 * time.Hour
)

type Ledger struct {
	Revision string    `json:"revision,omitempty"`
	Outcome  string    `json:"outcome,omitempty"`
	Failure  string    `json:"failure,omitempty"`
	Finished time.Time `json:"finished_at,omitzero"`
	Checked  time.Time `json:"checked_at,omitzero"`
	Repair   *Attempt  `json:"repair,omitempty"`
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
	Consumed []string
}

func (l Ledger) settled() bool {
	return slices.Contains([]string{OutcomeApplied, OutcomeEvaluated, reconcile.OutcomeFailed}, l.Outcome)
}

func decide(ledger Ledger, tip string, requests map[string]Request, now time.Time) Decision {
	var decision Decision
	if request, ok := requests[RequestApply]; ok {
		decision = Decision{Run: true, Apply: true, Full: request.Full, Reason: "requested: " + request.Reason}
		decision.Consumed = append(decision.Consumed, RequestApply)
	}
	if request, ok := requests[RequestRepair]; ok {
		decision.Consumed = append(decision.Consumed, RequestRepair)
		previous := ledger.Repair
		capped := previous != nil && previous.Revision == request.Revision && (previous.Outcome == reconcile.OutcomeFailed || now.Sub(previous.Started) < repairCooldown)
		if tip != "" && request.Revision == tip && ledger.Revision == tip && !capped {
			decision = Decision{Run: true, Apply: true, Full: true, Repair: true, Reason: "repair: " + request.Reason, Consumed: decision.Consumed}
		}
	}
	switch {
	case decision.Apply:
	case tip != "" && tip != ledger.Revision:
		decision.Run, decision.Apply, decision.Advanced, decision.Reason = true, true, true, "main at "+tip
	case tip != "" && !ledger.settled():
		decision.Run, decision.Apply, decision.Reason = true, true, "retry "+ledger.Outcome+" "+tip
	case now.Sub(ledger.Checked) >= readinessInterval:
		decision.Run, decision.Reason = true, "readiness"
	}
	return decision
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
