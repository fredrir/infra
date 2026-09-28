package platformops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	owner.Unit = "actions.runner.fredrir-a.host-a-1.service"
	current, _, _, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	current.Unit = "actions.runner.fredrir-b.host-b-1.service"
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
	owner.Unit = "actions.runner.fredrir-a.host-a-1.service"
	if err = os.WriteFile(filepath.Join(directory, "leases.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = changeRunnerAdmission(directory, 1, owner, false); err == nil {
		t.Fatal("corrupt admission state accepted")
	}
	stale := owner
	stale.Start, stale.Unit = "0", "actions.runner.fredrir-b.host-b-1.service"
	data, _ := json.Marshal([]runnerLease{stale})
	if err = os.WriteFile(filepath.Join(directory, "leases.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if accepted, err := changeRunnerAdmission(directory, 1, owner, false); !accepted || err != nil {
		t.Fatalf("reused process ID retained capacity: %v", err)
	}
	if _, err = runnerWorker(os.Getpid(), uint32(os.Getuid())); err == nil {
		t.Fatal("admitted a process outside a runner listener unit")
	}
}

func TestRunnerListenerUnitComesFromTheRunnerSlice(t *testing.T) {
	for _, test := range []struct {
		cgroup, unit string
	}{
		{"0::/infra.slice/infra-runners.slice/actions.runner.fredrir-Y.infra-build-09-Y-1.service\n", "actions.runner.fredrir-Y.infra-build-09-Y-1.service"},
		{"0::/infra.slice/infra-runners.slice/actions.runner.fredrir-Y.infra-build-09-Y-1.service/escape\n", ""},
		{"0::/system.slice/actions.runner.fredrir-Y.infra-build-09-Y-1.service\n", ""},
		{"0::/infra.slice/infra-runners.slice/session-1.scope\n", ""},
	} {
		unit, err := parseListenerUnit(test.cgroup)
		if unit != test.unit || (err == nil) != (test.unit != "") {
			t.Errorf("%q resolved to %q, %v; want %q", test.cgroup, unit, err, test.unit)
		}
	}
}

func TestRunnerAdmissionWorkerProcess(t *testing.T) {
	socket := os.Getenv("INFRA_ADMISSION_TEST_SOCKET")
	if socket == "" {
		t.Skip("runs as a process named Runner.Worker inside a fake listener unit")
	}
	if err := os.WriteFile("/proc/self/comm", []byte("Runner.Worker"), 0); err != nil {
		t.Fatal(err)
	}
	request := func(verb string) {
		client := exec.Command(os.Args[0], "-test.run=^TestRunnerAdmissionClientProcess$")
		client.Env = append(os.Environ(), "INFRA_ADMISSION_TEST_VERB="+verb)
		if output, err := client.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n%s", verb, err, output)
			os.Exit(1)
		}
	}
	request("acquire")
	signal := os.Getenv("INFRA_ADMISSION_TEST_SIGNAL")
	for signal != "" {
		if _, err := os.Stat(signal); err == nil {
			request("release")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(time.Minute)
}

func TestRunnerAdmissionClientProcess(t *testing.T) {
	socket := os.Getenv("INFRA_ADMISSION_TEST_SOCKET")
	if socket == "" || os.Getenv("INFRA_ADMISSION_TEST_VERB") == "" {
		t.Skip("runs as a job process of a fake Runner.Worker")
	}
	if err := RequestRunnerAdmission(context.Background(), socket, os.Getenv("INFRA_ADMISSION_TEST_VERB") == "release"); err != nil {
		t.Fatal(err)
	}
}

type testBroker struct {
	t       *testing.T
	socket  string
	sockets []string
	stop    func()
	*admissionBroker
}

func startAdmissionBroker(t *testing.T, capacity int) *testBroker {
	t.Helper()
	return startAdmissionBrokers(t, capacity, 1)
}

func startAdmissionBrokers(t *testing.T, capacity, count int) *testBroker {
	t.Helper()
	previousUnit, previousPoll := listenerUnit, admissionPollInterval
	listenerUnit = func(pid int) (string, error) {
		environment, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			return "", err
		}
		for variable := range bytes.SplitSeq(environment, []byte{0}) {
			if unit, ok := strings.CutPrefix(string(variable), "INFRA_ADMISSION_TEST_UNIT="); ok {
				return unit, nil
			}
		}
		return "", fmt.Errorf("process %d runs outside a runner listener unit", pid)
	}
	admissionPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { listenerUnit, admissionPollInterval = previousUnit, previousPoll })
	broker := &testBroker{t: t, admissionBroker: &admissionBroker{directory: t.TempDir(), capacity: capacity, log: io.Discard, open: map[string]int{}}}
	var listeners []*net.UnixListener
	for index := range count {
		socket := filepath.Join(t.TempDir(), fmt.Sprintf("admission-%d.sock", index))
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		broker.sockets, listeners = append(broker.sockets, socket), append(listeners, listener)
	}
	broker.socket = broker.sockets[0]
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.run(ctx, listeners...) }()
	broker.stop = sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(broker.stop)
	return broker
}

func TestRunnerAdmissionCanceledCallerProcess(t *testing.T) {
	socket := os.Getenv("INFRA_ADMISSION_TEST_CANCELED_SOCKET")
	if socket == "" {
		t.Skip("runs as a queued worker with a canceled request")
	}
	if err := os.WriteFile("/proc/self/comm", []byte("Runner.Worker"), 0); err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(connection, "acquire"); err != nil {
		t.Fatal(err)
	}
	if err := connection.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if reply, err := bufio.NewReader(connection).ReadString('\n'); reply != "" || !errors.Is(err, io.EOF) {
		t.Fatalf("canceled queue request returned %q, %v instead of closing unanswered", reply, err)
	}
}

func TestRunnerAdmissionCancellationIsNotADenial(t *testing.T) {
	broker := startAdmissionBroker(t, 1)
	worker := broker.worker("actions.runner.fredrir-first.host-first-1.service", "")
	broker.await(func(leases []runnerLease) bool { return len(leases) == 1 }, "first worker was not admitted")
	before := broker.leases()
	caller := exec.Command(os.Args[0], "-test.run=^TestRunnerAdmissionCanceledCallerProcess$")
	caller.Env = append(os.Environ(), "INFRA_ADMISSION_TEST_CANCELED_SOCKET="+broker.socket, "INFRA_ADMISSION_TEST_UNIT=actions.runner.fredrir-queued.host-queued-1.service")
	if output, err := caller.CombinedOutput(); err != nil {
		t.Fatalf("canceled queued caller failed: %v\n%s", err, output)
	}
	if after := broker.leases(); !slices.Equal(before, after) || after[0].PID != worker.Process.Pid {
		t.Fatalf("canceled caller changed the admitted lease: before=%+v after=%+v", before, after)
	}
}

func TestRunnerAdmissionQueuedJobSurvivesBrokerRestart(t *testing.T) {
	broker := startAdmissionBroker(t, 1)
	signal := filepath.Join(t.TempDir(), "release")
	first := broker.worker("actions.runner.fredrir-first.host-first-1.service", signal)
	broker.await(func(leases []runnerLease) bool { return len(leases) == 1 }, "first worker was not admitted")
	before := broker.leases()
	queuedUnit := "actions.runner.fredrir-queued.host-queued-1.service"
	queued := broker.worker(queuedUnit, "")
	for deadline := time.Now().Add(5 * time.Second); broker.openRequests()[queuedUnit] != 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("second worker did not queue behind the admitted worker")
		}
	}
	broker.stop()
	broker.awaitQuiet()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: broker.socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	restarted := &admissionBroker{directory: broker.directory, capacity: broker.capacity, log: io.Discard, open: map[string]int{}}
	go func() { done <- restarted.run(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	if after := broker.leases(); !slices.Equal(before, after) {
		t.Fatalf("broker restart changed an admitted lease: before=%+v after=%+v", before, after)
	}
	if err := first.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("broker restart stopped the admitted worker: %v", err)
	}
	if err := os.WriteFile(signal, nil, 0600); err != nil {
		t.Fatal(err)
	}
	broker.await(func(leases []runnerLease) bool {
		return len(leases) == 1 && leases[0].PID == queued.Process.Pid
	}, "queued worker did not retry and acquire the released slot after restart")
}

func (b *testBroker) on(index int) *testBroker {
	on := *b
	on.socket = b.sockets[index]
	return &on
}

func (b *testBroker) openRequests() map[string]int {
	b.lock.Lock()
	defer b.lock.Unlock()
	return maps.Clone(b.open)
}

func (b *testBroker) worker(unit, signal string) *exec.Cmd {
	b.t.Helper()
	worker := exec.Command(os.Args[0], "-test.run=^TestRunnerAdmissionWorkerProcess$")
	worker.Env = append(os.Environ(), "INFRA_ADMISSION_TEST_SOCKET="+b.socket, "INFRA_ADMISSION_TEST_UNIT="+unit, "INFRA_ADMISSION_TEST_SIGNAL="+signal)
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := worker.Start(); err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() {
		_ = syscall.Kill(-worker.Process.Pid, syscall.SIGKILL)
		_ = worker.Wait()
	})
	return worker
}

func (b *testBroker) leases() []runnerLease {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(b.admissionBroker.directory, "leases.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	var current []runnerLease
	if err := json.Unmarshal(data, &current); err != nil {
		b.t.Fatal(err)
	}
	return current
}

func (b *testBroker) await(condition func([]runnerLease) bool, message string) {
	b.t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !condition(b.leases()); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			b.t.Fatalf("%s: %+v", message, b.leases())
		}
	}
}

func units(leases []runnerLease) []string {
	var names []string
	for _, lease := range leases {
		names = append(names, lease.Unit)
	}
	return names
}

func TestRunnerAdmissionFakeWorkersOfOneListenerHoldOneSlot(t *testing.T) {
	broker := startAdmissionBroker(t, 2)
	for range 3 {
		fake := broker.worker("actions.runner.fredrir-attacker.host-attacker-1.service", "")
		broker.await(func(leases []runnerLease) bool { return len(leases) == 1 && leases[0].PID == fake.Process.Pid }, "another fake worker of the listener took a second slot")
	}
	broker.worker("actions.runner.fredrir-other.host-other-1.service", "")
	broker.await(func(leases []runnerLease) bool {
		return len(leases) == 2 && strings.Join(units(leases), " ") == "actions.runner.fredrir-attacker.host-attacker-1.service actions.runner.fredrir-other.host-other-1.service"
	}, "fake workers of one listener starved another repository")
}

func TestRunnerAdmissionLateReleaseKeepsTheListenersNewerJob(t *testing.T) {
	broker := startAdmissionBroker(t, 1)
	signal := filepath.Join(t.TempDir(), "release")
	earlier := broker.worker("actions.runner.fredrir-a.host-a-1.service", signal)
	broker.await(func(leases []runnerLease) bool { return len(leases) == 1 && leases[0].PID == earlier.Process.Pid }, "first job not admitted")
	later := broker.worker("actions.runner.fredrir-a.host-a-1.service", "")
	broker.await(func(leases []runnerLease) bool { return len(leases) == 1 && leases[0].PID == later.Process.Pid }, "the listener's next job did not take over its slot")
	if err := os.WriteFile(signal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	broker.awaitQuiet()
	if leases := broker.leases(); len(leases) != 1 || leases[0].PID != later.Process.Pid {
		t.Fatalf("late release of the earlier job removed the newer lease: %+v", leases)
	}
}

func (b *testBroker) awaitQuiet() {
	b.t.Helper()
	time.Sleep(100 * time.Millisecond)
	for deadline := time.Now().Add(5 * time.Second); len(b.openRequests()) > 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			b.t.Fatalf("requests still open: %v", b.openRequests())
		}
	}
}

func (b *testBroker) job(unit string, environment ...string) (string, error) {
	b.t.Helper()
	job := exec.Command(os.Args[0], "-test.run=^TestRunnerAdmissionJobProcess$")
	job.Env = append(append(os.Environ(), "INFRA_ADMISSION_TEST_SOCKET="+b.socket, "INFRA_ADMISSION_TEST_UNIT="+unit), environment...)
	output, err := job.CombinedOutput()
	return string(output), err
}

func TestRunnerAdmissionJobProcess(t *testing.T) {
	socket := os.Getenv("INFRA_ADMISSION_TEST_SOCKET")
	if socket == "" {
		t.Skip("runs as a job process inside a fake listener unit")
	}
	if seconds, _ := strconv.Atoi(os.Getenv("INFRA_ADMISSION_TEST_FLOOD")); seconds > 0 {
		var flood sync.WaitGroup
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		for range 16 {
			flood.Go(func() {
				for time.Now().Before(deadline) {
					if connection, err := net.Dial("unix", socket); err == nil {
						connection.Close()
					}
				}
			})
		}
		flood.Wait()
		return
	}
	count, _ := strconv.Atoi(os.Getenv("INFRA_ADMISSION_TEST_IDLE"))
	var idle []net.Conn
	for range count {
		connection, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		idle = append(idle, connection)
	}
	if os.Getenv("INFRA_ADMISSION_TEST_VERB") == "" {
		reply, _ := io.ReadAll(idle[0])
		fmt.Print(string(reply))
		return
	}
	time.Sleep(100 * time.Millisecond)
	if err := RequestRunnerAdmission(context.Background(), socket, false); err != nil {
		fmt.Println(err)
	}
}

func TestRunnerAdmissionBrokerDropsIdleAndAbandonedCallers(t *testing.T) {
	previous := admissionRequestLimit
	admissionRequestLimit = 200 * time.Millisecond
	t.Cleanup(func() { admissionRequestLimit = previous })
	broker := startAdmissionBroker(t, 1)
	started := time.Now()
	if reply, err := broker.job("actions.runner.fredrir-a.host-a-1.service", "INFRA_ADMISSION_TEST_IDLE=1"); err != nil || !strings.Contains(reply, "read admission request") || time.Since(started) > 3*time.Second {
		t.Fatalf("an idle caller got %q, %v after %s instead of the request deadline", reply, err, time.Since(started))
	}
	broker.worker("actions.runner.fredrir-a.host-a-1.service", "")
	broker.await(func(leases []runnerLease) bool { return len(leases) == 1 }, "holder not admitted")
	waiting := broker.worker("actions.runner.fredrir-b.host-b-1.service", "")
	for deadline := time.Now().Add(5 * time.Second); broker.openRequests()["actions.runner.fredrir-b.host-b-1.service"] == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("waiter never reached the broker")
		}
	}
	if err := syscall.Kill(-waiting.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	broker.awaitQuiet()
	if leases := broker.leases(); len(leases) != 1 || leases[0].Unit != "actions.runner.fredrir-a.host-a-1.service" {
		t.Fatalf("abandoned waiter changed the leases: %+v", leases)
	}
	if err := RequestRunnerAdmission(context.Background(), broker.socket, false); err == nil || !strings.Contains(err.Error(), "outside a runner listener unit") {
		t.Fatalf("a caller outside any listener was answered with %v", err)
	}
}

func TestRunnerAdmissionBrokerBoundsOpenRequestsPerListener(t *testing.T) {
	broker := startAdmissionBroker(t, 1)
	output, err := broker.job("actions.runner.fredrir-a.host-a-1.service", "INFRA_ADMISSION_TEST_IDLE=2", "INFRA_ADMISSION_TEST_VERB=acquire")
	if err != nil || !strings.Contains(output, "already has 2 admission requests open") {
		t.Fatalf("a listener held more than two broker requests: %v\n%s", err, output)
	}
}

func TestRunnerAdmissionFloodOnOneRepositorySocketDoesNotDelayAnother(t *testing.T) {
	if os.Getenv("INFRA_ADMISSION_FLOOD_TEST") != "1" {
		t.Skip("INFRA_ADMISSION_FLOOD_TEST=1 floods a socket from sixteen threads")
	}
	broker := startAdmissionBrokers(t, 2, 2)
	flood := make(chan error, 1)
	go func() {
		_, err := broker.on(0).job("actions.runner.fredrir-flood.host-flood-1.service", "INFRA_ADMISSION_TEST_FLOOD=1")
		flood <- err
	}()
	time.Sleep(300 * time.Millisecond)
	for index := range 3 {
		started := time.Now()
		worker := broker.on(1).worker(fmt.Sprintf("actions.runner.fredrir-other.host-other-%d.service", index), "")
		for !slices.ContainsFunc(broker.leases(), func(lease runnerLease) bool { return lease.PID == worker.Process.Pid }) {
			if time.Since(started) > 2*time.Second {
				t.Fatalf("another repository waited %s for admission during a connection flood", time.Since(started))
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = syscall.Kill(-worker.Process.Pid, syscall.SIGKILL)
		_ = worker.Wait()
	}
	if err := <-flood; err != nil {
		t.Fatal(err)
	}
	broker.awaitQuiet()
}

func TestRunnerAdmissionRequestsRetryUntilTheBrokerAnswers(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "admission.sock")
	answered := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			answered <- err
			return
		}
		defer listener.Close()
		for _, reply := range []string{"", "ok\n", "denied: listener has no Runner.Worker\n"} {
			connection, err := listener.AcceptUnix()
			if err != nil {
				answered <- err
				return
			}
			if _, err := bufio.NewReader(connection).ReadString('\n'); err != nil {
				answered <- err
				return
			}
			_, _ = connection.Write([]byte(reply))
			connection.Close()
		}
		answered <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := RequestRunnerAdmission(ctx, socket, false); err != nil {
		t.Fatalf("a request to a restarting broker failed: %v", err)
	}
	previous := admissionMissingLimit
	admissionMissingLimit = 200 * time.Millisecond
	t.Cleanup(func() { admissionMissingLimit = previous })
	started := time.Now()
	if err := RequestRunnerAdmission(ctx, filepath.Join(t.TempDir(), "absent.sock"), false); !errors.Is(err, syscall.ENOENT) || time.Since(started) > 2*time.Second {
		t.Fatalf("a missing socket returned %v after %s instead of failing after its limit", err, time.Since(started))
	}
	if err := RequestRunnerAdmission(ctx, socket, false); err == nil || !strings.Contains(err.Error(), "denied: listener has no Runner.Worker") {
		t.Fatalf("a denial was retried or lost: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatal(err)
	}
}

func TestRunnerAdmissionDenialBeforeTheRequestReachesTheCaller(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "admission.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			_, _ = connection.Write([]byte("denied: caller runs outside a runner listener unit\n"))
			connection.Close()
		}
	}()
	for range 200 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := RequestRunnerAdmission(ctx, socket, false)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "outside a runner listener unit") {
			t.Fatalf("an immediate denial reached the caller as %v", err)
		}
	}
}
