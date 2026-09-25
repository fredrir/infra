package ci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func TestMeasureBudgetFailureWritesReport(t *testing.T) {
	dir := t.TempDir()
	runner := Runner{Execute: func(ctx context.Context, _ process.Options) (process.Result, error) {
		<-ctx.Done()
		return process.Result{ExitCode: -1}, ctx.Err()
	}}
	report, err := Measure(context.Background(), runner, "checks", time.Millisecond, dir, []string{"check"})
	if err == nil || report.Success || !report.BudgetExceeded {
		t.Fatalf("deadline passed: %+v %v", report, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checks.json")); err != nil {
		t.Fatal(err)
	}
}

func TestMeasureBudgetLetsTheCommandFinishItsReport(t *testing.T) {
	reported := filepath.Join(t.TempDir(), "report")
	script := `trap 'sleep 0.5; echo done > "$0"; exit 1' TERM; while :; do sleep 0.1; done`
	report, err := Measure(context.Background(), Runner{}, "drift-verification", time.Second, "", []string{"sh", "-c", script, reported})
	if err == nil || !report.BudgetExceeded {
		t.Fatalf("deadline passed: %+v %v", report, err)
	}
	if _, err := os.Stat(reported); err != nil {
		t.Fatalf("command was killed before writing its report: %v", err)
	}
}

func TestMeasureCommandFailureDoesNotBecomeSuccess(t *testing.T) {
	runner := Runner{Execute: func(context.Context, process.Options) (process.Result, error) {
		return process.Result{ExitCode: 7}, errors.New("failed check")
	}}
	report, err := Measure(context.Background(), runner, "checks", time.Second, "", []string{"check"})
	if err == nil || report.Success || report.ExitCode != 7 || report.BudgetExceeded {
		t.Fatalf("lost failure: %+v %v", report, err)
	}
}
