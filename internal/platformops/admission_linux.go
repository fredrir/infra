package platformops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type runnerLease struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

func processIdentity(pid int) (runnerLease, int, string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return runnerLease{}, 0, "", err
	}
	text := string(data)
	first, last := strings.Index(text, "("), strings.LastIndex(text, ") ")
	if first < 0 || last <= first {
		return runnerLease{}, 0, "", fmt.Errorf("invalid process identity")
	}
	fields := strings.Fields(text[last+2:])
	if len(fields) < 20 {
		return runnerLease{}, 0, "", fmt.Errorf("incomplete process identity")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return runnerLease{}, 0, "", os.ErrNotExist
	}
	parent, err := strconv.Atoi(fields[1])
	return runnerLease{PID: pid, Start: fields[19]}, parent, text[first+1 : last], err
}

func RunnerAdmission(ctx context.Context, directory string, capacity int, release bool) error {
	for pid, depth := os.Getppid(), 0; pid > 1 && depth < 32; depth++ {
		owner, parent, name, err := processIdentity(pid)
		if err != nil {
			return err
		}
		if name == "Runner.Worker" {
			return waitRunnerAdmission(ctx, directory, capacity, owner, release)
		}
		pid = parent
	}
	return fmt.Errorf("runner admission requires a Runner.Worker ancestor")
}

func waitRunnerAdmission(ctx context.Context, directory string, capacity int, owner runnerLease, release bool) error {
	if !filepath.IsAbs(directory) || capacity < 1 || capacity > 8 || owner.PID <= 1 || owner.Start == "" {
		return fmt.Errorf("invalid runner admission configuration")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		admitted, err := changeRunnerAdmission(directory, capacity, owner, release)
		if err != nil || admitted {
			return err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func changeRunnerAdmission(directory string, capacity int, owner runnerLease, release bool) (bool, error) {
	lock, err := os.OpenFile(filepath.Join(directory, "gate.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(directory, "leases.json")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	var previous []runnerLease
	if err == nil {
		if err = json.Unmarshal(data, &previous); err != nil {
			return false, fmt.Errorf("read runner leases: %w", err)
		}
	}
	leases := make([]runnerLease, 0, len(previous)+1)
	present := false
	for _, lease := range previous {
		actual, _, _, err := processIdentity(lease.PID)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if actual != lease {
			continue
		}
		if lease == owner {
			present = true
			if release {
				continue
			}
		}
		leases = append(leases, lease)
	}
	if !release && !present {
		if len(leases) >= capacity {
			return false, nil
		}
		actual, _, _, err := processIdentity(owner.PID)
		if err != nil || actual != owner {
			return false, fmt.Errorf("runner identity changed before admission: %v", err)
		}
		leases = append(leases, owner)
	}
	data, err = json.Marshal(leases)
	if err != nil {
		return false, err
	}
	temporary, err := os.CreateTemp(directory, ".leases-")
	if err != nil {
		return false, err
	}
	defer os.Remove(temporary.Name())
	_, writeErr := temporary.Write(data)
	if err = errors.Join(writeErr, temporary.Close()); err != nil {
		return false, err
	}
	return true, os.Rename(temporary.Name(), path)
}
