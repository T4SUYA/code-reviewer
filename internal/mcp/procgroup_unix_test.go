//go:build !windows

package mcp

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClient_CloseKillsProcessesTheServerStarted(t *testing.T) {
	c, err := Connect(context.Background(), fakeConfig(t, "spawn"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	out, err := c.Call(context.Background(), "spawn", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	pid, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("spawn returned %q", out)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("grandchild %d is not running before Close: %v", pid, err)
	}

	_ = c.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// A killed grandchild is reaped by init; until then it can linger as a zombie.
		if err := syscall.Kill(pid, 0); err != nil || isZombie(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("grandchild %d still running after Close", pid)
}

// isZombie reports whether the process has exited and only awaits reaping.
func isZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
