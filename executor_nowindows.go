//go:build !windows

package codexcli

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// platformProc holds per-spawn platform resources. On unix the process group
// created by Setpgid needs no handle, so there is nothing to hold or release.
type platformProc struct{}

// hideConsoleWindow is a no-op on non-Windows platforms, where there is no
// console window to suppress. See executor_windows.go for the Windows
// implementation.
func hideConsoleWindow(*exec.Cmd) {}

// setPlatformAttrs confines the child in its own process group so context
// cancellation can kill the whole tree — codex plus the MCP servers and turn
// shell commands it spawns — not just the codex process. WaitDelay bounds the
// unwind: SIGTERM to the group first, a kill if the child is still there
// after the delay.
func setPlatformAttrs(cmd *exec.Cmd) *platformProc {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second
	return &platformProc{}
}

// setUpdateCancel configures cancellation for the `codex update` spawn:
// SIGINT the whole process group so the installer — and any children doing
// the actual download — can unwind its staged release tree. The caller's
// WaitDelay provides the eventual kill.
func setUpdateCancel(cmd *exec.Cmd) *platformProc {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	}
	return &platformProc{}
}

// afterStart finalizes process-tree confinement once the child is running.
// Nothing to do on unix: Setpgid took effect at spawn.
func (p *platformProc) afterStart(cmd *exec.Cmd) {}

// release frees per-spawn platform resources. Nothing held on unix.
func (p *platformProc) release() {}

// buildPlatformCmd creates the exec.Cmd. No special handling needed off
// Windows; see executor_windows.go for the npm shim bypass.
func buildPlatformCmd(ctx context.Context, binary string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, binary, args...)
}
