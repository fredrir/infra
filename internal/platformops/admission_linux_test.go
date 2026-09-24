package platformops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRunnerAdmissionBoundsConcurrentJobsAndRecoversAfterExit(t *testing.T) {
	directory := t.TempDir()
	first := exec.Command("sleep", "30")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Process.Kill(); first.Wait() })
	owner, _, _, err := processIdentity(first.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	current, _, _, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = waitRunnerAdmission(context.Background(), directory, 1, owner, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err = waitRunnerAdmission(ctx, directory, 1, current, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second job escaped host capacity: %v", err)
	}
	if err = first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	first.Wait()
	if err = waitRunnerAdmission(context.Background(), directory, 1, current, false); err != nil {
		t.Fatalf("dead worker retained capacity: %v", err)
	}
	if err = waitRunnerAdmission(context.Background(), directory, 1, owner, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "leases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var leases []runnerLease
	if err = json.Unmarshal(data, &leases); err != nil || len(leases) != 1 || leases[0] != current {
		t.Fatalf("late release removed a different worker: %s, %v", data, err)
	}
	if err = waitRunnerAdmission(context.Background(), directory, 1, current, true); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(directory, "leases.json"))
	if string(data) != "[]" {
		t.Fatalf("completed job retained capacity: %s", data)
	}
}

func TestRunnerAdmissionRejectsInvalidStateAndReusedProcessID(t *testing.T) {
	directory := t.TempDir()
	owner, _, _, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "leases.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = changeRunnerAdmission(directory, 1, owner, false); err == nil {
		t.Fatal("corrupt admission state accepted")
	}
	stale := owner
	stale.Start = "0"
	data, _ := json.Marshal([]runnerLease{stale})
	if err = os.WriteFile(filepath.Join(directory, "leases.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if accepted, err := changeRunnerAdmission(directory, 1, owner, false); !accepted || err != nil {
		t.Fatalf("reused process ID retained capacity: %v", err)
	}
	if err = RunnerAdmission(context.Background(), directory, 1, false); err == nil {
		t.Fatal("admitted a process outside the runner")
	}
}
