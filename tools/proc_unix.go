//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// killTreeOnCancel runs the command in its own process group and kills the
// whole group on timeout, so children of `sh -c` don't outlive it.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
