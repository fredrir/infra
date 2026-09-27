package platformops

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type runnerLease struct {
	Unit  string `json:"unit"`
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

var (
	listenerUnit          = cgroupListenerUnit
	admissionRequestLimit = 5 * time.Second
	admissionPollInterval = 250 * time.Millisecond
	runnerUnitName        = regexp.MustCompile(`^actions\.runner\.[A-Za-z0-9_.-]+\.service$`)
)

const admissionConnectionsPerUnit = 2

const runnerSlice = "/infra.slice/infra-runners.slice/"

func cgroupListenerUnit(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	return parseListenerUnit(string(data))
}

func parseListenerUnit(cgroup string) (string, error) {
	for line := range strings.SplitSeq(strings.TrimSpace(cgroup), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			if unit, ok := strings.CutPrefix(path, runnerSlice); ok && runnerUnitName.MatchString(unit) {
				return unit, nil
			}
			return "", fmt.Errorf("caller runs in %s, outside a runner listener unit", path)
		}
	}
	return "", fmt.Errorf("caller has no unified cgroup")
}

func processIdentity(pid int) (runnerLease, int, string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, syscall.ESRCH) {
		return runnerLease{}, 0, "", os.ErrNotExist
	}
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

func runnerWorker(pid int, uid uint32) (runnerLease, error) {
	unit, err := listenerUnit(pid)
	if err != nil {
		return runnerLease{}, err
	}
	for depth := 0; pid > 1 && depth < 32; depth++ {
		owner, parent, name, err := processIdentity(pid)
		if err != nil {
			return runnerLease{}, err
		}
		if name == "Runner.Worker" {
			info, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
			if err != nil {
				return runnerLease{}, err
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uid {
				return runnerLease{}, fmt.Errorf("Runner.Worker %d belongs to another account", pid)
			}
			owner.Unit = unit
			return owner, nil
		}
		pid = parent
	}
	return runnerLease{}, fmt.Errorf("runner admission requires a Runner.Worker ancestor")
}

type admissionBroker struct {
	directory string
	capacity  int
	log       io.Writer
	lock      sync.Mutex
	open      map[string]int
}

func ServeRunnerAdmissions(ctx context.Context, listeners []*net.UnixListener, directory string, capacity int, log io.Writer) error {
	return (&admissionBroker{directory: directory, capacity: capacity, log: log, open: map[string]int{}}).run(ctx, listeners...)
}

func (b *admissionBroker) run(ctx context.Context, listeners ...*net.UnixListener) error {
	if len(listeners) == 0 {
		return fmt.Errorf("runner admission requires a socket")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failures := make(chan error, len(listeners))
	for _, listener := range listeners {
		stop := context.AfterFunc(ctx, func() { listener.Close() })
		defer stop()
		go func() { failures <- b.accept(ctx, listener) }()
	}
	var err error
	for range listeners {
		if failure := <-failures; failure != nil && err == nil {
			err = failure
			cancel()
		}
	}
	return err
}

func (b *admissionBroker) accept(ctx context.Context, listener *net.UnixListener) error {
	for {
		connection, err := listener.AcceptUnix()
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE), errors.Is(err, syscall.ENOBUFS), errors.Is(err, syscall.ENOMEM):
			time.Sleep(50 * time.Millisecond)
		case err != nil:
			return err
		default:
			go b.serve(ctx, connection)
		}
	}
}

func (b *admissionBroker) claim(unit string) bool {
	b.lock.Lock()
	defer b.lock.Unlock()
	if b.open[unit] >= admissionConnectionsPerUnit {
		return false
	}
	b.open[unit]++
	return true
}

func (b *admissionBroker) unclaim(unit string) {
	b.lock.Lock()
	defer b.lock.Unlock()
	if b.open[unit]--; b.open[unit] == 0 {
		delete(b.open, unit)
	}
}

func (b *admissionBroker) serve(ctx context.Context, connection *net.UnixConn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	defer connection.Close()
	unit, verb, err := b.request(ctx, connection)
	message := "ok"
	if err != nil {
		message = "denied: " + err.Error()
	}
	if unit != "" && verb != "" {
		fmt.Fprintf(b.log, "runner admission %s %s: %s\n", verb, unit, message)
	}
	_, _ = fmt.Fprintln(connection, message)
}

func (b *admissionBroker) request(ctx context.Context, connection *net.UnixConn) (string, string, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return "", "", err
	}
	var peer *syscall.Ucred
	if controlErr := raw.Control(func(fd uintptr) {
		peer, err = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); controlErr != nil {
		return "", "", controlErr
	}
	if err != nil {
		return "", "", err
	}
	unit, err := listenerUnit(int(peer.Pid))
	if err != nil {
		return "", "", err
	}
	if !b.claim(unit) {
		return "", "", fmt.Errorf("%s already has %d admission requests open", unit, admissionConnectionsPerUnit)
	}
	defer b.unclaim(unit)
	if err := connection.SetReadDeadline(time.Now().Add(admissionRequestLimit)); err != nil {
		return unit, "", err
	}
	verb, err := bufio.NewReader(io.LimitReader(connection, 16)).ReadString('\n')
	if err != nil {
		return unit, "", fmt.Errorf("read admission request: %w", err)
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return unit, "", err
	}
	if verb = strings.TrimSpace(verb); verb != "acquire" && verb != "release" {
		return unit, "", fmt.Errorf("unknown admission request %q", verb)
	}
	owner, err := runnerWorker(int(peer.Pid), peer.Uid)
	if err != nil {
		return unit, verb, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, connection)
		cancel()
	}()
	return unit, verb, waitRunnerAdmission(ctx, b.directory, b.capacity, owner, verb == "release")
}

func waitRunnerAdmission(ctx context.Context, directory string, capacity int, owner runnerLease, release bool) error {
	if !filepath.IsAbs(directory) || capacity < 1 || capacity > 8 || owner.Unit == "" || owner.PID <= 1 || owner.Start == "" {
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
		timer := time.NewTimer(admissionPollInterval)
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
	for _, lease := range previous {
		actual, _, _, err := processIdentity(lease.PID)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if actual.Start != lease.Start || lease.Unit == owner.Unit && (!release || lease == owner) {
			continue
		}
		leases = append(leases, lease)
	}
	if !release {
		if len(leases) >= capacity {
			return false, nil
		}
		actual, _, _, err := processIdentity(owner.PID)
		if err != nil || actual.Start != owner.Start {
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
