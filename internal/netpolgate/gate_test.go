package netpolgate

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = Serve(t.Context(), listener) }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return listener, port
}

func localNetwork() Network {
	return Network{Connect: TCP, Attempt: 100 * time.Millisecond, Pause: time.Millisecond, MaxPause: 5 * time.Millisecond}
}

func loopback(allowed, denied string) Canary {
	return Canary{Addresses: []string{"127.0.0.1"}, Allowed: allowed, Denied: denied}
}

func wait(t *testing.T, canary Canary, network Network, deadline time.Duration) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	return Wait(ctx, canary, network)
}

func TestWaitReturnsOnceTheDeniedPortIsRefused(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	denied, deniedPort := listen(t)
	time.AfterFunc(80*time.Millisecond, func() { _ = denied.Close() })
	result, err := wait(t, loopback(allowed, deniedPort), localNetwork(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if held := result.Held[deniedPortOpen.String()]; held == 0 || held != result.Attempts-1 {
		t.Fatalf("passed while the denied port was still open: %s", result)
	}
}

func TestWaitHoldsWhileTheDeniedPortAccepts(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	_, denied := listen(t)
	_, err := wait(t, loopback(allowed, denied), localNetwork(), 200*time.Millisecond)
	if !errors.Is(err, ErrNotEnforced) || !strings.Contains(err.Error(), "accepts its denied port") {
		t.Fatalf("expected the gate to hold, got %v", err)
	}
}

func TestWaitHoldsWhenTheDeniedPortOnlyTimesOut(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	_, denied := listen(t)
	network := localNetwork()
	network.Connect = func(ctx context.Context, address string) error {
		if _, port, _ := net.SplitHostPort(address); port == denied {
			<-ctx.Done()
			return ctx.Err()
		}
		return TCP(ctx, address)
	}
	_, err := wait(t, loopback(allowed, denied), network, 300*time.Millisecond)
	if !errors.Is(err, ErrNotEnforced) || !strings.Contains(err.Error(), "no canary refuses its denied port") {
		t.Fatalf("a lost probe is not a refusal, got %v", err)
	}
}

func TestWaitHoldsWhenTheDeniedPortFailsWithAnythingButARefusal(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	_, denied := listen(t)
	for _, failure := range []error{syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ECONNRESET, errors.New("refused")} {
		t.Run(failure.Error(), func(t *testing.T) {
			t.Parallel()
			network := localNetwork()
			network.Connect = func(ctx context.Context, address string) error {
				if _, port, _ := net.SplitHostPort(address); port == denied {
					return &net.OpError{Op: "dial", Err: failure}
				}
				return TCP(ctx, address)
			}
			if _, err := wait(t, loopback(allowed, denied), network, 100*time.Millisecond); !errors.Is(err, ErrNotEnforced) {
				t.Fatalf("%v taken as proof of enforcement", failure)
			}
		})
	}
}

func TestWaitHoldsWithoutALiveCanary(t *testing.T) {
	t.Parallel()
	allowed, allowedPort := listen(t)
	denied, deniedPort := listen(t)
	_ = allowed.Close()
	_ = denied.Close()
	_, err := wait(t, loopback(allowedPort, deniedPort), localNetwork(), 200*time.Millisecond)
	if !errors.Is(err, ErrNotEnforced) || !strings.Contains(err.Error(), "no canary accepts its allowed port") {
		t.Fatalf("a dead canary must never pass as enforcement, got %v", err)
	}
}

func TestWaitHoldsWhenTheCanaryDiesDuringTheProbe(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	_, denied := listen(t)
	network := localNetwork()
	var calls atomic.Int32
	network.Connect = func(ctx context.Context, address string) error {
		if calls.Add(1)%3 == 1 {
			return TCP(ctx, address)
		}
		return &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	}
	_, err := wait(t, loopback(allowed, denied), network, 200*time.Millisecond)
	if !errors.Is(err, ErrNotEnforced) {
		t.Fatalf("a canary that stops answering mid-probe must not pass, got %v", err)
	}
}

func TestWaitHoldsWhileAnyCanaryAcceptsTheDeniedPort(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	_, denied := listen(t)
	network := localNetwork()
	network.Connect = func(ctx context.Context, address string) error {
		if host, port, _ := net.SplitHostPort(address); host == "127.0.0.2" && port == denied {
			return &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		}
		return TCP(ctx, strings.Replace(address, "127.0.0.2", "127.0.0.1", 1))
	}
	canary := Canary{Addresses: []string{"127.0.0.2", "127.0.0.1"}, Allowed: allowed, Denied: denied}
	_, err := wait(t, canary, network, 200*time.Millisecond)
	if !errors.Is(err, ErrNotEnforced) {
		t.Fatalf("one open canary proves the policy is not enforced, got %v", err)
	}
}

func TestWaitPassesThroughASecondCanaryWhenTheFirstIsDown(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	denied, deniedPort := listen(t)
	_ = denied.Close()
	network := localNetwork()
	network.Connect = func(ctx context.Context, address string) error {
		if host, _, _ := net.SplitHostPort(address); host == "127.0.0.2" {
			return &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		}
		return TCP(ctx, address)
	}
	canary := Canary{Addresses: []string{"127.0.0.2", "127.0.0.1"}, Allowed: allowed, Denied: deniedPort}
	if _, err := wait(t, canary, network, time.Second); err != nil {
		t.Fatalf("one live canary suffices, got %v", err)
	}
}

func TestWaitBacksOffAndReportsWhileHeld(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	_, denied := listen(t)
	network := localNetwork()
	network.Pause, network.MaxPause, network.Report = 10*time.Millisecond, 40*time.Millisecond, 100*time.Millisecond
	var mu sync.Mutex
	var reports []time.Duration
	network.Progress = func(held time.Duration, _ Result) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, held)
	}
	result, _ := wait(t, loopback(allowed, denied), network, 350*time.Millisecond)
	if result.Attempts < 9 || result.Attempts > 14 {
		t.Fatalf("expected the pause to double up to its cap, got %d attempts", result.Attempts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) < 2 || len(reports) > 4 || reports[0] < 100*time.Millisecond {
		t.Fatalf("expected a progress report every 100ms, got %v", reports)
	}
}

func TestRunReportsEnforcement(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	denied, deniedPort := listen(t)
	_ = denied.Close()
	var stdout bytes.Buffer
	err := Run(t.Context(), []string{"wait", "--canary", "127.0.0.1", "--allowed", allowed, "--denied", deniedPort}, &stdout)
	if err != nil || !strings.HasPrefix(stdout.String(), "egress policy enforced in ") || !strings.HasSuffix(stdout.String(), " after 1 attempts\n") {
		t.Fatalf("unexpected result %q, %v", stdout.String(), err)
	}
}

func TestRunHoldsUntilThePodGivesUp(t *testing.T) {
	t.Parallel()
	_, allowed := listen(t)
	_, denied := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, cancel)
	started := time.Now()
	err := Run(ctx, []string{"wait", "--canary", "127.0.0.1", "--allowed", allowed, "--denied", denied}, &bytes.Buffer{})
	if !errors.Is(err, ErrNotEnforced) || !errors.Is(err, context.Canceled) || time.Since(started) < 300*time.Millisecond {
		t.Fatalf("the gate must hold until the pod is stopped, got %v after %s", err, time.Since(started))
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

func TestServeListensOnBothPorts(t *testing.T) {
	t.Parallel()
	allowed, denied := freePort(t), freePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, []string{"serve", "--allowed", allowed, "--denied", denied}, &bytes.Buffer{}) }()
	for _, port := range []string{allowed, denied} {
		deadline := time.Now().Add(time.Second)
		for TCP(t.Context(), net.JoinHostPort("127.0.0.1", port)) != nil {
			if time.Now().After(deadline) {
				t.Fatalf("the canary never listens on %s", port)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunRejectsIncompleteInvocations(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"wait"}, {"probe"}, {"wait", "--canary"}, {"wait", "--canary", "canary.netpol-canary.svc.cluster.local."}, {"wait", "--deadline", "1s", "--canary", "127.0.0.1"}} {
		if Run(t.Context(), args, &bytes.Buffer{}) == nil {
			t.Fatalf("%q accepted", args)
		}
	}
}
