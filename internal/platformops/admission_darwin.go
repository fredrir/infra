package platformops

import (
	"context"
	"fmt"
)

func RunnerAdmission(context.Context, string, int, bool) error {
	return fmt.Errorf("runner admission requires Linux")
}
