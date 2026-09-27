package netpolgate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"syscall"
	"time"
)

type Canary struct {
	Addresses []string
	Allowed   string
	Denied    string
}

type Network struct {
	Connect  func(ctx context.Context, address string) error
	Attempt  time.Duration
	Pause    time.Duration
	MaxPause time.Duration
	Report   time.Duration
	Progress func(held time.Duration, result Result)
}

type state int

const (
	canaryUnreachable state = iota
	deniedPortSilent
	enforced
	deniedPortOpen
)

func (s state) String() string {
	switch s {
	case deniedPortSilent:
		return "no canary refuses its denied port"
	case enforced:
		return "enforced"
	case deniedPortOpen:
		return "a canary accepts its denied port"
	default:
		return "no canary accepts its allowed port"
	}
}

var ErrNotEnforced = errors.New("egress policy not proven")

type Result struct {
	Attempts int
	Held     map[string]int
}

func (r Result) String() string {
	summary := fmt.Sprintf("%d attempts", r.Attempts)
	for _, reason := range slices.Sorted(maps.Keys(r.Held)) {
		summary += fmt.Sprintf("; held %d× because %s", r.Held[reason], reason)
	}
	return summary
}

func Wait(ctx context.Context, canary Canary, network Network) (Result, error) {
	result := Result{Held: map[string]int{}}
	started, reported := time.Now(), time.Now()
	pause := network.Pause
	for {
		result.Attempts++
		observed := network.observe(context.WithoutCancel(ctx), canary)
		if observed == enforced {
			return result, nil
		}
		result.Held[observed.String()]++
		if network.Progress != nil && time.Since(reported) >= network.Report {
			network.Progress(time.Since(started), result)
			reported = time.Now()
		}
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("%w after %d attempts: %s: %w", ErrNotEnforced, result.Attempts, observed, ctx.Err())
		case <-time.After(pause):
		}
		pause = min(2*pause, network.MaxPause)
	}
}

func (n Network) observe(ctx context.Context, canary Canary) state {
	observed := canaryUnreachable
	for _, address := range canary.Addresses {
		allowed := net.JoinHostPort(address, canary.Allowed)
		if n.connect(ctx, allowed) != nil {
			continue
		}
		switch err := n.connect(ctx, net.JoinHostPort(address, canary.Denied)); {
		case err == nil:
			return deniedPortOpen
		case !errors.Is(err, syscall.ECONNREFUSED):
			observed = max(observed, deniedPortSilent)
		case n.connect(ctx, allowed) == nil:
			observed = enforced
		}
	}
	return observed
}

func (n Network) connect(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, n.Attempt)
	defer cancel()
	return n.Connect(ctx, address)
}

func TCP(ctx context.Context, address string) error {
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	return connection.Close()
}

func Serve(ctx context.Context, listeners ...net.Listener) error {
	errs := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func() {
			for {
				connection, err := listener.Accept()
				if err != nil {
					errs <- err
					return
				}
				_ = connection.Close()
			}
		}()
	}
	select {
	case <-ctx.Done():
		for _, listener := range listeners {
			_ = listener.Close()
		}
		return nil
	case err := <-errs:
		return err
	}
}
