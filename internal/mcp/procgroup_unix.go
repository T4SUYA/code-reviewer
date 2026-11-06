//go:build !windows

package mcp

import (
	"os/exec"
	"syscall"
)

// startInOwnProcessGroup makes the child the leader of a new process group, so
// it and everything it spawns can be signalled together.
func startInOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the child's whole process group. It is a no-op when
// the child never started or the group is already gone.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // ESRCH: the group already exited
}
