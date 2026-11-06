//go:build windows

package mcp

import "os/exec"

// startInOwnProcessGroup is a no-op on Windows: only the server process itself
// is stopped on Close.
func startInOwnProcessGroup(*exec.Cmd) {}

// killProcessGroup kills the server process.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill() //nolint:errcheck // the process may already have exited
}
