//go:build !windows

package codexcli

import (
	"errors"
	"os/exec"
	"syscall"
)

// extractExitDetails returns the signal name and exit code from a Wait()
// error. ("", 0) on a clean exit and ("", -1) for non-ExitError errors
// (start failure, IO error, ctx cancel before process started).
func extractExitDetails(err error) (signal string, exitCode int) {
	if err == nil {
		return "", 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "", -1
	}
	code := exitErr.ExitCode()
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return signalName(ws.Signal()), code
	}
	return "", code
}

func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGBUS:
		return "SIGBUS"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	}
	return s.String()
}
