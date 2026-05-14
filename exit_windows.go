//go:build windows

package codexcli

import (
	"errors"
	"os/exec"
)

// extractExitDetails on Windows surfaces only the exit code — signals
// are unix-specific. SIGKILL-equivalent termination shows up as a
// non-zero exit code with empty Signal, which classifyExit categorizes
// as Crashed; that's acceptable until someone needs richer detail.
func extractExitDetails(err error) (signal string, exitCode int) {
	if err == nil {
		return "", 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "", -1
	}
	return "", exitErr.ExitCode()
}
