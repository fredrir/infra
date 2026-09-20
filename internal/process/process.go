package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"
)

type Options struct {
	Name      string
	Args      []string
	Dir       string
	Env       []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	Timeout   time.Duration
	KillGrace time.Duration
}

type Result struct {
	Stdout          []byte
	Stderr          []byte
	Duration        time.Duration
	ExitCode        int
	StdoutTruncated bool
	StderrTruncated bool
}

var ErrOutputLimit = errors.New("captured output exceeds limit; stream command output")

type capture struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *capture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (8 << 20) - b.buffer.Len()
	if n > remaining {
		b.truncated = true
	}
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buffer.Write(p)
	}
	return n, nil
}

func Run(ctx context.Context, o Options) (Result, error) {
	start := time.Now()
	if o.Name == "" {
		return Result{}, errors.New("command required")
	}
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	grace := o.KillGrace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	command := exec.Command(o.Name, o.Args...)
	command.Dir = o.Dir
	command.Env = o.Env
	command.Stdin = o.Stdin
	command.WaitDelay = time.Second
	configureGroup(command)
	var stdout, stderr capture
	command.Stdout = &stdout
	command.Stderr = &stderr
	if o.Stdout != nil {
		command.Stdout = io.MultiWriter(&stdout, o.Stdout)
	}
	if o.Stderr != nil {
		command.Stderr = io.MultiWriter(&stderr, o.Stderr)
	}
	if err := command.Start(); err != nil {
		return Result{Duration: time.Since(start), ExitCode: -1}, fmt.Errorf("start %s: %w", filepath.Base(o.Name), err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	var err error
	select {
	case err = <-wait:
	case <-ctx.Done():
		terminateGroup(command, false)
		timer := time.NewTimer(grace)
		select {
		case err = <-wait:
		case <-timer.C:
			terminateGroup(command, true)
			err = <-wait
		}
		timer.Stop()
		terminateGroup(command, true)
		err = ctx.Err()
	}
	result := Result{Stdout: stdout.buffer.Bytes(), Stderr: stderr.buffer.Bytes(), Duration: time.Since(start), ExitCode: command.ProcessState.ExitCode(), StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated}
	if (stdout.truncated && o.Stdout == nil) || (stderr.truncated && o.Stderr == nil) {
		err = errors.Join(err, ErrOutputLimit)
	}
	if err != nil {
		return result, fmt.Errorf("%s failed: %w", filepath.Base(o.Name), err)
	}
	return result, nil
}
