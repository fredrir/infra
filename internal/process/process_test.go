package process_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fredrir/infra/internal/process"
)

func TestProcessHelper(t *testing.T) {
	switch os.Getenv("INFRA_PROCESS_TEST") {
	case "output":
		fmt.Print("output")
		fmt.Fprint(os.Stderr, "diagnostic")
		os.Exit(0)
	case "fail":
		fmt.Fprint(os.Stderr, "private credential")
		os.Exit(7)
	case "large":
		fmt.Print(strings.Repeat("x", (8<<20)+1))
		os.Exit(0)
	case "wait":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func TestCapturedOutputCannotSilentlyTruncate(t *testing.T) {
	o := helper(t, "large")
	r, err := process.Run(context.Background(), o)
	if !errors.Is(err, process.ErrOutputLimit) || !r.StdoutTruncated || len(r.Stdout) != 8<<20 {
		t.Fatalf("output limit was hidden: %v", err)
	}
	o.Stdout = io.Discard
	r, err = process.Run(context.Background(), o)
	if err != nil || !r.StdoutTruncated {
		t.Fatalf("streaming failed: %v", err)
	}
}

func helper(t *testing.T, mode string) process.Options {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return process.Options{Name: path, Args: []string{"-test.run=^TestProcessHelper$"}, Env: append(os.Environ(), "INFRA_PROCESS_TEST="+mode), KillGrace: 20 * time.Millisecond}
}

func TestCapturesSeparateStreams(t *testing.T) {
	r, err := process.Run(context.Background(), helper(t, "output"))
	if err != nil || string(r.Stdout) != "output" || string(r.Stderr) != "diagnostic" || r.ExitCode != 0 {
		t.Fatalf("result=%+v error=%v", r, err)
	}
}

func TestErrorsDoNotExposeCapturedSecrets(t *testing.T) {
	r, err := process.Run(context.Background(), helper(t, "fail"))
	if err == nil || r.ExitCode != 7 || strings.Contains(err.Error(), "private credential") {
		t.Fatalf("result=%+v error=%v", r, err)
	}
}

func TestTimeoutKillsUncooperativeCommand(t *testing.T) {
	o := helper(t, "wait")
	o.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := process.Run(context.Background(), o)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %v", err)
	}
}
