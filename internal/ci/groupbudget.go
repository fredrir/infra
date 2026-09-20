package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type CheckGroupReport struct {
	Schema           int           `json:"schema"`
	Accounting       string        `json:"accounting"`
	BudgetSeconds    float64       `json:"budget_seconds"`
	DurationSeconds  float64       `json:"duration_seconds"`
	RemainingSeconds float64       `json:"remaining_seconds"`
	Success          bool          `json:"success"`
	Stages           []StageReport `json:"stages"`
}

func CheckGroupBudget(directory string, budget time.Duration, requireReports bool) (CheckGroupReport, error) {
	report := CheckGroupReport{Schema: 1, Accounting: "sum-of-recorded-check-durations", BudgetSeconds: budget.Seconds(), Stages: []StageReport{}}
	if directory == "" || budget <= 0 || budget > 10*time.Second {
		return report, errors.New("check report directory and aggregate budget up to 10 seconds required")
	}
	seen := map[string]bool{}
	var stageErr error
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) && path == directory && !requireReports {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("check receipt must not be a symlink: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(path, ".json") {
			return fmt.Errorf("unexpected check receipt: %s", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		var stage StageReport
		decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&stage); err != nil {
			return fmt.Errorf("decode check receipt %s: %w", path, err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return fmt.Errorf("check receipt must contain one object: %s", path)
		}
		if stage.Schema != 1 || !regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`).MatchString(stage.Stage) || stage.Started.IsZero() || stage.DurationSeconds < 0 || math.IsNaN(stage.DurationSeconds) || math.IsInf(stage.DurationSeconds, 0) || stage.BudgetSeconds <= 0 || stage.CPUSeconds < 0 || stage.PeakMemoryBytes < 0 {
			return fmt.Errorf("invalid check receipt: %s", path)
		}
		if seen[stage.Stage] {
			return fmt.Errorf("duplicate check stage %q", stage.Stage)
		}
		seen[stage.Stage] = true
		report.Stages = append(report.Stages, stage)
		report.DurationSeconds += stage.DurationSeconds
		if !stage.Success || stage.ExitCode != 0 || stage.BudgetExceeded || stage.DurationSeconds >= stage.BudgetSeconds {
			stageErr = errors.Join(stageErr, fmt.Errorf("check stage %q did not pass within its budget", stage.Stage))
		}
		return nil
	})
	err = errors.Join(err, stageErr)
	slices.SortFunc(report.Stages, func(a, b StageReport) int { return strings.Compare(a.Stage, b.Stage) })
	report.RemainingSeconds = max(0, report.BudgetSeconds-report.DurationSeconds)
	if err == nil && requireReports && len(report.Stages) == 0 {
		err = errors.New("check receipts required")
	}
	if report.DurationSeconds >= report.BudgetSeconds {
		err = errors.Join(err, fmt.Errorf("aggregate checks used %.6fs of %.6fs budget", report.DurationSeconds, report.BudgetSeconds))
	}
	report.Success = err == nil
	return report, err
}

func MeasureCheck(ctx context.Context, runner Runner, stage string, budget time.Duration, directory string, command []string) (CheckGroupReport, error) {
	report, err := CheckGroupBudget(directory, budget, false)
	if err != nil {
		return report, err
	}
	for _, previous := range report.Stages {
		if previous.Stage == stage {
			return report, fmt.Errorf("check stage %q already recorded", stage)
		}
	}
	remaining := time.Duration(report.RemainingSeconds * float64(time.Second))
	_, runErr := Measure(ctx, runner, stage, remaining, directory, command)
	report, checkErr := CheckGroupBudget(directory, budget, true)
	return report, errors.Join(runErr, checkErr)
}
