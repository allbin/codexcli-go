package codexcli

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ExitReason classifies why the codex subprocess terminated. Carried by
// ProcessExitError so consumers can give users actionable messages.
type ExitReason string

const (
	// ExitReasonNormal indicates a clean exit (status 0, no signal).
	ExitReasonNormal ExitReason = "normal"
	// ExitReasonKilled indicates the process was terminated by a signal
	// (SIGKILL, SIGTERM, OOM kill, etc.).
	ExitReasonKilled ExitReason = "killed"
	// ExitReasonCrashed indicates a non-zero exit without a signal — a
	// codex bug, panic, or fatal startup error.
	ExitReasonCrashed ExitReason = "crashed"
	// ExitReasonContextCanceled indicates the consumer canceled the
	// session context, so the SDK initiated termination.
	ExitReasonContextCanceled ExitReason = "context_canceled"
	// ExitReasonUnknown is used when no other category fits.
	ExitReasonUnknown ExitReason = "unknown"
)

// ProcessExitError is returned by I/O calls on a Conn whose subprocess
// has died, and is carried by ProcessExitEvent on the event stream.
//
// The shape mirrors claudecli-go's Error: a structured handle on the
// most useful diagnostic bits without coupling to anything codex-specific.
// LastStderr is best-effort — we capture the trailing bytes of stderr
// to give consumers something to chew on when the process dies silently.
type ProcessExitError struct {
	Reason     ExitReason
	ExitCode   int
	Signal     string
	LastStderr string
	WaitErr    error
	At         time.Time
}

// Error implements the error interface.
func (e *ProcessExitError) Error() string {
	switch e.Reason {
	case ExitReasonNormal:
		return "codexcli: process exited cleanly"
	case ExitReasonKilled:
		if e.Signal != "" {
			return fmt.Sprintf("codexcli: process killed by %s", e.Signal)
		}
		return "codexcli: process killed by signal"
	case ExitReasonContextCanceled:
		return "codexcli: process terminated (context canceled)"
	case ExitReasonCrashed:
		tail := truncateStderr(e.LastStderr)
		if tail != "" {
			return fmt.Sprintf("codexcli: process crashed with exit code %d: %s", e.ExitCode, tail)
		}
		return fmt.Sprintf("codexcli: process crashed with exit code %d", e.ExitCode)
	default:
		tail := truncateStderr(e.LastStderr)
		if tail != "" {
			return fmt.Sprintf("codexcli: process exit (unknown cause): %s", tail)
		}
		return "codexcli: process exit (unknown cause)"
	}
}

// Unwrap returns the underlying wait error.
func (e *ProcessExitError) Unwrap() error { return e.WaitErr }

// Is allows errors.Is(err, ErrProcessExited) to succeed for any exit reason.
func (e *ProcessExitError) Is(target error) bool {
	return target == ErrProcessExited
}

// ErrProcessExited is the umbrella sentinel that matches every
// ProcessExitError regardless of reason. Use this with errors.Is when the
// caller cares about the failure category, not the specifics.
var ErrProcessExited = errors.New("codexcli: process exited")

// ProcessExitEvent is emitted on the event channel after the codex
// subprocess terminates, before the channel closes. Consumers can rely
// on this as the terminal event when the process dies mid-turn (no
// turn/completed will follow).
type ProcessExitEvent struct {
	Err *ProcessExitError
}

func (*ProcessExitEvent) event() {}

func (e *ProcessExitEvent) String() string {
	return fmt.Sprintf("ProcessExitEvent{%s}", e.Err.Error())
}

func truncateStderr(s string) string {
	const max = 240
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

// stderrDrainGracePeriod is how long classifyExit waits for the
// drainStderr goroutine to finish capturing the subprocess's final
// stderr lines before snapshotting LastStderr. 1s is enough to absorb
// pipe-flush jitter on common Linux/macOS kernels without making the
// reaper feel sluggish.
const stderrDrainGracePeriod = 1 * time.Second

// classifyExit maps a Wait error + context state to a structured
// ProcessExitError. Adapted from claudecli-go's classifyExit; the
// context-cancellation branch wins because SDK-initiated kills should
// surface as "we did this" rather than "the process crashed".
func classifyExit(waitErr error, ctxErr error, lastStderr string) *ProcessExitError {
	signal, code := extractExitDetails(waitErr)
	ev := &ProcessExitError{
		ExitCode:   code,
		Signal:     signal,
		LastStderr: lastStderr,
		WaitErr:    waitErr,
		At:         time.Now(),
	}
	switch {
	case errors.Is(ctxErr, context.Canceled), errors.Is(ctxErr, context.DeadlineExceeded):
		ev.Reason = ExitReasonContextCanceled
	case waitErr == nil:
		ev.Reason = ExitReasonNormal
	case signal != "":
		ev.Reason = ExitReasonKilled
	case code > 0:
		ev.Reason = ExitReasonCrashed
	default:
		ev.Reason = ExitReasonUnknown
	}
	return ev
}
