package ci

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func writeCheckReceipt(t *testing.T, root, path, stage string, duration float64) {
	t.Helper()
	report := StageReport{Schema: 1, Stage: stage, Started: time.Unix(1800000000, 0), DurationSeconds: duration, BudgetSeconds: 10, Success: true}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckGroupAddsCachedInlineChecksAndCurrentScan(t *testing.T) {
	directory := t.TempDir()
	writeCheckReceipt(t, directory, "inline/unit.json", "unit", 4)
	writeCheckReceipt(t, directory, "scan.json", "scan", 5)
	report, err := CheckGroupBudget(directory, 10*time.Second, true)
	if err != nil || !report.Success || report.DurationSeconds != 9 || report.RemainingSeconds != 1 || len(report.Stages) != 2 {
		t.Fatalf("aggregate=%+v error=%v", report, err)
	}
	writeCheckReceipt(t, directory, "provenance.json", "provenance", 1)
	if report, err := CheckGroupBudget(directory, 10*time.Second, true); err == nil || report.Success {
		t.Fatalf("exact budget exhaustion passed: %+v %v", report, err)
	}
}

func TestCheckGroupRejectsMissingInvalidAndConflictingReceipts(t *testing.T) {
	for _, scenario := range []string{"missing", "empty", "duplicate", "negative", "failed", "malformed", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			switch scenario {
			case "missing":
				directory = filepath.Join(directory, "missing")
			case "duplicate":
				writeCheckReceipt(t, directory, "one.json", "unit", 1)
				writeCheckReceipt(t, directory, "two.json", "unit", 1)
			case "negative":
				writeCheckReceipt(t, directory, "unit.json", "unit", -1)
			case "failed":
				writeCheckReceipt(t, directory, "unit.json", "unit", 11)
			case "malformed":
				if err := os.WriteFile(filepath.Join(directory, "unit.json"), []byte(`{"success":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(directory, "outside")); err != nil {
					t.Fatal(err)
				}
			}
			if report, err := CheckGroupBudget(directory, 10*time.Second, true); err == nil || report.Success {
				t.Fatalf("invalid receipts passed: %+v %v", report, err)
			}
		})
	}
}

func TestMeasureCheckUsesOnlyRemainingAggregateBudget(t *testing.T) {
	directory := t.TempDir()
	writeCheckReceipt(t, directory, "inline/unit.json", "unit", 7)
	called := false
	runner := Runner{Execute: func(ctx context.Context, _ process.Options) (process.Result, error) {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second {
			t.Fatal("check received a fresh budget instead of the remaining budget")
		}
		return process.Result{}, nil
	}}
	report, err := MeasureCheck(context.Background(), runner, "scan", 10*time.Second, directory, []string{"scan"})
	if err != nil || !called || !report.Success || len(report.Stages) != 2 {
		t.Fatalf("check group=%+v error=%v called=%v", report, err, called)
	}
	called = false
	if _, err := MeasureCheck(context.Background(), runner, "scan", 10*time.Second, directory, []string{"scan"}); err == nil || called {
		t.Fatal("existing receipt overwritten")
	}
}

func TestMeasureCheckPreservesDeadlineFailure(t *testing.T) {
	directory := t.TempDir()
	writeCheckReceipt(t, directory, "unit.json", "unit", 9.999)
	runner := Runner{Execute: func(ctx context.Context, _ process.Options) (process.Result, error) {
		<-ctx.Done()
		return process.Result{ExitCode: -1}, ctx.Err()
	}}
	report, err := MeasureCheck(context.Background(), runner, "scan", 10*time.Second, directory, []string{"scan"})
	if !errors.Is(err, context.DeadlineExceeded) || report.Success {
		t.Fatalf("deadline lost: %+v %v", report, err)
	}
}

func TestCheckGroupCountsLaterReceiptsAfterFailedStage(t *testing.T) {
	directory := t.TempDir()
	writeCheckReceipt(t, directory, "frontend.json", "frontend", 4.75)
	writeCheckReceipt(t, directory, "vulnerability.json", "vulnerability-scan", 4.5)
	failed := StageReport{Schema: 1, Stage: "provenance", Started: time.Unix(1800000000, 0), DurationSeconds: 0.8, BudgetSeconds: 0.75, BudgetExceeded: true, Success: false, ExitCode: -1}
	data, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "provenance.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := CheckGroupBudget(directory, 10*time.Second, true)
	if err == nil || report.Success || len(report.Stages) != 3 || report.DurationSeconds != 10.05 || report.RemainingSeconds != 0 {
		t.Fatalf("failed check lost later durations: %+v %v", report, err)
	}
	if !strings.Contains(err.Error(), `check stage "provenance"`) || !strings.Contains(err.Error(), "aggregate checks used 10.050000s") {
		t.Fatalf("stage or aggregate failure was lost: %v", err)
	}
	called := false
	runner := Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		called = true
		return process.Result{}, nil
	}}
	if _, err := MeasureCheck(context.Background(), runner, "extra", 10*time.Second, directory, []string{"extra"}); err == nil || called {
		t.Fatal("failed group allowed another check")
	}
}
