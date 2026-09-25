package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/fredrir/infra/internal/process"
)

type StageReport struct {
	Schema          int       `json:"schema"`
	Stage           string    `json:"stage"`
	Revision        string    `json:"revision,omitempty"`
	Started         time.Time `json:"started"`
	DurationSeconds float64   `json:"duration_seconds"`
	BudgetSeconds   float64   `json:"budget_seconds"`
	BudgetExceeded  bool      `json:"budget_exceeded"`
	Success         bool      `json:"success"`
	ExitCode        int       `json:"exit_code"`
	CPUSeconds      float64   `json:"cpu_seconds"`
	PeakMemoryBytes int64     `json:"peak_memory_bytes"`
	Error           string    `json:"error,omitempty"`
}

func Measure(ctx context.Context, runner Runner, stage string, budget time.Duration, directory string, command []string) (StageReport, error) {
	report := StageReport{Schema: 1, Stage: stage, Revision: os.Getenv("GITHUB_SHA"), Started: time.Now().UTC(), BudgetSeconds: budget.Seconds(), ExitCode: -1}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`).MatchString(stage) || budget <= 0 || len(command) == 0 {
		return report, errors.New("stage, positive budget and command required")
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	result, err := runner.execute(ctx, process.Options{Name: command[0], Args: command[1:], Dir: runner.Dir, Env: append(os.Environ(), runner.Env...), Stdout: runner.Stdout, Stderr: runner.Stderr, KillGrace: 10 * time.Second})
	report.DurationSeconds = time.Since(report.Started).Seconds()
	report.ExitCode = result.ExitCode
	report.CPUSeconds, report.PeakMemoryBytes = result.CPUSeconds, result.PeakMemoryBytes
	report.BudgetExceeded = report.DurationSeconds >= budget.Seconds() || errors.Is(ctx.Err(), context.DeadlineExceeded)
	if report.BudgetExceeded {
		err = errors.Join(err, fmt.Errorf("%s exceeded %s budget", stage, budget))
	}
	report.Success = err == nil
	if err != nil {
		report.Error = err.Error()
	}
	if directory != "" {
		if writeErr := os.MkdirAll(directory, 0755); writeErr != nil {
			return report, errors.Join(err, writeErr)
		}
		data, writeErr := json.MarshalIndent(report, "", "  ")
		if writeErr == nil {
			writeErr = os.WriteFile(filepath.Join(directory, stage+".json"), append(data, '\n'), 0600)
		}
		err = errors.Join(err, writeErr)
	}
	return report, err
}
