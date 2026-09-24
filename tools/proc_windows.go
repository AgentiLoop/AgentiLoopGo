//go:build windows

package tools

import (
	"os/exec"
	"strconv"
)

// killTreeOnCancel kills the whole process tree on timeout, so children of
// `cmd /C` don't outlive it (and keep the working directory locked).
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
