package pipeline

import (
	"context"
	"errors"
	"time"
)

func CheckFast(ctx context.Context, opts Options) (Report, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	opts.Operation, opts.GeneratedBuildCheck = "test", true
	report, err := Run(ctx, opts)
	return report, errors.Join(err, ctx.Err())
}
