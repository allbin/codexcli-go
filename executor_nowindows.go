//go:build !windows

package codexcli

import "os/exec"

// hideConsoleWindow is a no-op on non-Windows platforms, where there is no
// console window to suppress. See executor_windows.go for the Windows
// implementation.
func hideConsoleWindow(*exec.Cmd) {}
