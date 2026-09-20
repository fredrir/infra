package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"time"
)

func CheckFast(ctx context.Context, opts Options) ([]Report, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	opts.Operation = "test"
	if opts.ReportDir == "" {
		opts.ReportDir = filepath.Join(opts.Root, "dist", "reports")
	}
	report, err := Run(ctx, opts)
	reports := []Report{report}
	if err != nil {
		return reports, err
	}
	opts.Operation = "generate-check"
	opts.Targets = nil
	opts.ReportDir = filepath.Join(opts.ReportDir, "generated")
	report, err = Run(ctx, opts)
	return append(reports, report), errors.Join(err, ctx.Err())
}
