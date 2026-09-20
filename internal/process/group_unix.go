//go:build unix

package process

import (
	"os/exec"
	"runtime"
	"syscall"
)

func configureGroup(command *exec.Cmd) { command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func peakMemory(command *exec.Cmd) int64 {
	usage, ok := command.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return usage.Maxrss
	}
	return usage.Maxrss * 1024
}

func terminateGroup(command *exec.Cmd, force bool) {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	_ = syscall.Kill(-command.Process.Pid, signal)
}
