package platformops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"syscall"
	"time"
)

var errAdmissionUnanswered = errors.New("admission broker closed the request without an answer")

var admissionMissingLimit = time.Minute

func RequestRunnerAdmission(ctx context.Context, socket string, release bool) error {
	var missingSince time.Time
	for delay := 20 * time.Millisecond; ; delay = min(2*delay, time.Second) {
		err := requestRunnerAdmission(ctx, socket, release)
		missing := errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
		if !missing {
			missingSince = time.Time{}
		} else if missingSince.IsZero() {
			missingSince = time.Now()
		}
		if !missing && !errors.Is(err, errAdmissionUnanswered) && !errors.Is(err, syscall.EAGAIN) || missing && time.Since(missingSince) > admissionMissingLimit {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(delay/2 + rand.N(delay)):
		}
	}
}

func requestRunnerAdmission(ctx context.Context, socket string, release bool) error {
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	verb := "acquire"
	if release {
		verb = "release"
	}
	_, writeErr := fmt.Fprintln(connection, verb)
	reply, err := bufio.NewReader(connection).ReadString('\n')
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch reply = strings.TrimSpace(reply); {
	case reply == "ok" && writeErr == nil:
		return nil
	case reply != "" && reply != "ok":
		return fmt.Errorf("runner admission %s %s", verb, reply)
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET), errors.Is(writeErr, syscall.EPIPE), errors.Is(writeErr, syscall.ECONNRESET):
		return errAdmissionUnanswered
	}
	return fmt.Errorf("runner admission %s: %w", verb, errors.Join(writeErr, err))
}
