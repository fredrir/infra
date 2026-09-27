package netpolgate

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"
)

type addresses []string

func (a *addresses) String() string { return fmt.Sprint(*a) }

func (a *addresses) Set(value string) error {
	address, err := netip.ParseAddr(value)
	if err != nil {
		return fmt.Errorf("canary %q is not an IP address", value)
	}
	*a = append(*a, address.String())
	return nil
}

func Run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: netpol-gate wait|serve [flags]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	allowed := flags.String("allowed", "8080", "")
	denied := flags.String("denied", "8081", "")
	switch args[0] {
	case "wait":
		var canaries addresses
		flags.Var(&canaries, "canary", "")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if len(canaries) == 0 {
			return errors.New("wait requires --canary")
		}
		started := time.Now()
		result, err := Wait(ctx, Canary{Addresses: canaries, Allowed: *allowed, Denied: *denied}, Network{
			Connect:  TCP,
			Attempt:  250 * time.Millisecond,
			Pause:    20 * time.Millisecond,
			MaxPause: time.Second,
			Report:   time.Minute,
			Progress: func(held time.Duration, result Result) {
				fmt.Fprintf(stdout, "held for %s after %s\n", held.Round(time.Second), result)
			},
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "egress policy enforced in %s after %s\n", time.Since(started).Round(time.Millisecond), result)
		return err
	case "serve":
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		var listeners []net.Listener
		for _, port := range []string{*denied, *allowed} {
			listener, err := net.Listen("tcp", net.JoinHostPort("", port))
			if err != nil {
				return err
			}
			listeners = append(listeners, listener)
		}
		return Serve(ctx, listeners...)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
