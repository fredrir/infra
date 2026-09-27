package ci

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func TestRustFastChecksFormatAlongsideUnitTests(t *testing.T) {
	for _, failing := range []string{"", "fmt", "nextest", "fmt nextest"} {
		t.Run(cmp.Or(failing, "passing"), func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("RUSTC_WRAPPER", "")
			t.Setenv("FAST_TEST_ARGS", "")
			succeed := process.Runner{Execute: func(context.Context, process.Options) (process.Result, error) { return process.Result{}, nil }}
			if err := PrepareRust(context.Background(), succeed, directory, "", ""); err != nil {
				t.Fatal(err)
			}
			tested := make(chan struct{})
			var output bytes.Buffer
			runner := process.Runner{Stdout: &output, Stderr: &output, Execute: func(_ context.Context, options process.Options) (process.Result, error) {
				tool := options.Args[0]
				switch tool {
				case "nextest":
					fmt.Fprintln(options.Stdout, "unit tests")
					close(tested)
				case "fmt":
					select {
					case <-tested:
					case <-time.After(5 * time.Second):
						return process.Result{ExitCode: 1}, errors.New("formatting waited for the unit tests")
					}
					fmt.Fprintln(options.Stdout, "formatting")
				}
				if strings.Contains(failing, tool) {
					return process.Result{ExitCode: 1}, errors.New(tool + " failed")
				}
				return process.Result{}, nil
			}}
			err := CheckRust(context.Background(), runner, directory, "fast")
			if output.String() != "formatting\nunit tests\n" {
				t.Errorf("output %q, want formatting before unit tests", output.String())
			}
			for _, tool := range strings.Fields(failing) {
				if err == nil || !strings.Contains(err.Error(), tool+" failed") {
					t.Errorf("error %v does not report the %s failure", err, tool)
				}
			}
			if failing == "" && err != nil {
				t.Fatal(err)
			}
		})
	}
}
