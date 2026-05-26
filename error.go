package codexcli

import (
	"errors"
	"fmt"
)

// Sentinel errors. Use errors.Is to test from any error in the chain.
var (
	// ErrNotInitialized is returned when a request hits the server
	// before the initialize/initialized handshake completes.
	ErrNotInitialized = errors.New("codexcli: not initialized")
	// ErrAlreadyInitialized is returned if initialize is sent twice on
	// the same connection.
	ErrAlreadyInitialized = errors.New("codexcli: already initialized")
	// ErrTurnFailed wraps a turn that ended with status: failed.
	ErrTurnFailed = errors.New("codexcli: turn failed")
	// ErrThreadNotFound is returned by ResumeThread when the server
	// cannot locate the requested thread (deleted, never existed, etc.).
	ErrThreadNotFound = errors.New("codexcli: thread not found")
)

// ExitError carries a non-zero process exit and any captured stderr.
type ExitError struct {
	ExitCode int
	Stderr   string
	Err      error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("codexcli: process exit %d: %s", e.ExitCode, e.Err)
	}
	if e.Stderr != "" {
		s := e.Stderr
		if len(s) > 256 {
			s = s[:256] + "..."
		}
		return fmt.Sprintf("codexcli: process exit %d: %s", e.ExitCode, s)
	}
	return fmt.Sprintf("codexcli: process exit %d", e.ExitCode)
}

func (e *ExitError) Unwrap() error { return e.Err }
