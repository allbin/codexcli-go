package codexcli

import (
	"context"
	"errors"
	"sync"

	"github.com/allbin/codexcli-go/schema"
)

// State represents the lifecycle of a Stream.
type State int

const (
	StateStarting State = iota
	StateRunning
	StateDone
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateDone:
		return "done"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ErrNoTurn is returned by Stream.Wait when the stream closed before a
// TurnCompletedEvent ever arrived.
var ErrNoTurn = errors.New("codexcli: stream closed before turn/completed")

// Stream is the channel-based event stream from Client.Run. Iterate
// Events() for typed events, or call Wait() to block to terminal state.
type Stream struct {
	events <-chan Event
	done   <-chan struct{}
	cancel context.CancelFunc

	mu        sync.Mutex
	state     State
	finalTurn *schema.Turn
	err       error
	waited    bool
}

func newStream(events <-chan Event, done <-chan struct{}, cancel context.CancelFunc) *Stream {
	tracked := make(chan Event, 64)
	s := &Stream{
		events: tracked,
		done:   done,
		cancel: cancel,
		state:  StateStarting,
	}
	go func() {
		defer close(tracked)
		for ev := range events {
			s.track(ev)
			tracked <- ev
		}
	}()
	return s
}

// Events returns a channel of typed events. Closed when the stream ends.
func (s *Stream) Events() <-chan Event { return s.events }

// State returns the current lifecycle state.
func (s *Stream) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Wait drains remaining events and returns the final turn. Idempotent —
// repeated calls return the same value.
func (s *Stream) Wait() (*schema.Turn, error) {
	s.mu.Lock()
	if s.waited {
		t, err := s.finalTurn, s.err
		s.mu.Unlock()
		return t, err
	}
	s.mu.Unlock()
	for range s.events {
	}
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waited = true
	if s.finalTurn == nil && s.err == nil {
		s.err = ErrNoTurn
	}
	return s.finalTurn, s.err
}

// Close cancels the underlying process and drains the events channel.
func (s *Stream) Close() error {
	s.cancel()
	for range s.events {
	}
	<-s.done
	return nil
}

func (s *Stream) track(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e := ev.(type) {
	case *TurnStartedEvent:
		s.state = StateRunning
	case *TurnCompletedEvent:
		s.finalTurn = &e.Turn
		switch e.Turn.Status {
		case schema.TurnFailed:
			s.state = StateFailed
			if e.Turn.Error != nil {
				s.err = wrapTurnError(e.Turn.Error)
			} else {
				s.err = ErrTurnFailed
			}
		default:
			s.state = StateDone
		}
	case *ErrorEvent:
		if e.Fatal {
			s.state = StateFailed
			if s.err == nil {
				s.err = e.Err
			}
		}
	case *ProcessExitEvent:
		// Promote to StateFailed only on a non-clean exit. A normal exit
		// after turn/completed is just shutdown ordering and shouldn't
		// poison Stream.Wait() with an error.
		if e.Err != nil && e.Err.Reason != ExitReasonNormal && e.Err.Reason != ExitReasonContextCanceled {
			s.state = StateFailed
			if s.err == nil {
				s.err = e.Err
			}
		}
	}
}

func wrapTurnError(te *schema.TurnError) error {
	if te == nil {
		return ErrTurnFailed
	}
	return &TurnFailure{Message: te.Message, CodexErrorInfo: te.CodexErrorInfo, Details: te.AdditionalDetails}
}

// TurnFailure is the typed error returned via Stream.Wait when a turn
// completes with status: failed. CodexErrorInfo holds the raw JSON form
// of the discriminated union — callers can json.Unmarshal it into a
// custom shape until codexcli ships a typed representation.
type TurnFailure struct {
	Message        string
	CodexErrorInfo []byte
	Details        *string
}

func (e *TurnFailure) Error() string { return "codexcli: turn failed: " + e.Message }
func (e *TurnFailure) Is(target error) bool {
	return target == ErrTurnFailed
}
