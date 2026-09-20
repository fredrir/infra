//go:build unix

package process

import (
	"os/exec"
	"syscall"
)

func configureGroup(command *exec.Cmd) { command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func terminateGroup(command *exec.Cmd, force bool) {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	_ = syscall.Kill(-command.Process.Pid, signal)
}
