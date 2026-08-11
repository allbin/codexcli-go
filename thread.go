package codexcli

import (
	"context"
	"fmt"
	"sync"

	"github.com/allbin/codexcli-go/schema"
)

// Thread is a started codex thread. Hold a Thread to dispatch multiple
// turns on the same conversation.
type Thread struct {
	ID       string
	conn     *Conn
	response schema.ThreadStartResponse

	mu         sync.Mutex
	activeTurn string
}

// ActiveTurnID returns the most recently observed in-flight turn id, or
// empty when no turn is active. Updated when turn/start returns and
// cleared on turn/completed.
func (t *Thread) ActiveTurnID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.activeTurn
}

func (t *Thread) setActiveTurn(id string) {
	t.mu.Lock()
	t.activeTurn = id
	t.mu.Unlock()
}

// Interrupt cancels the in-flight turn on this thread. It returns once
// the server acknowledges the turn/interrupt request; the resulting
// `turn/completed` notification arrives on the active Stream with
// status: "interrupted".
//
// Safe to call with no active turn — the server returns success regardless.
func (t *Thread) Interrupt(ctx context.Context) error {
	turnID := t.ActiveTurnID()
	return t.conn.interrupt(ctx, t.ID, turnID)
}

// Response returns the server's thread/start payload (model resolution,
// approval policy, instruction sources, sandbox details).
func (t *Thread) Response() schema.ThreadStartResponse { return t.response }

// StartTurn dispatches turn/start with the given prompt and returns a
// stream of typed events scoped to this turn. The stream ends with
// TurnCompletedEvent (or ErrorEvent on transport/protocol failure).
//
// Multiple concurrent turns on the same thread are not supported by
// codex app-server — call StartTurn sequentially.
func (t *Thread) StartTurn(ctx context.Context, prompt string, opts ...Option) (*Stream, error) {
	return t.StartTurnInput(ctx, []schema.UserInput{schema.TextInput(prompt)}, opts...)
}

// StartTurnInput dispatches turn/start with typed user input blocks and
// returns a stream of typed events scoped to this turn. Use this for image,
// local-image, skill, and mention inputs without relying on raw turn extras.
func (t *Thread) StartTurnInput(ctx context.Context, input []schema.UserInput, opts ...Option) (*Stream, error) {
	events := make(chan Event, 64)
	done := make(chan struct{})
	streamCtx, cancel := context.WithCancel(ctx)
	stream := newStream(events, done, cancel)
	sub := t.conn.subscribe(t.ID)

	go func() {
		defer close(done)
		defer close(events)
		defer t.conn.unsubscribe(t.ID)

		if _, err := t.startTurnInput(streamCtx, input, opts...); err != nil {
			events <- &ErrorEvent{Err: fmt.Errorf("turn/start: %w", err), Fatal: true}
			return
		}
		for {
			select {
			case ev, ok := <-sub:
				if !ok {
					return
				}
				events <- ev
				if _, completed := ev.(*TurnCompletedEvent); completed {
					return
				}
				if e, ok := ev.(*ErrorEvent); ok && e.Fatal {
					return
				}
			case <-streamCtx.Done():
				return
			}
		}
	}()
	return stream, nil
}

// startTurn is the synchronous turn/start request — returns once the
// server acknowledges (the streamed events arrive separately).
func (t *Thread) startTurn(ctx context.Context, prompt string, opts ...Option) (*schema.TurnStartResponse, error) {
	return t.startTurnInput(ctx, []schema.UserInput{schema.TextInput(prompt)}, opts...)
}

func (t *Thread) startTurnInput(ctx context.Context, input []schema.UserInput, opts ...Option) (*schema.TurnStartResponse, error) {
	if err := t.conn.checkExited(); err != nil {
		return nil, err
	}
	resolved := resolveOptions(t.conn.options.callOpts(), opts)
	params := resolved.buildTurnStartParams(t.ID, input)
	var resp schema.TurnStartResponse
	if err := t.conn.rpc.Request(ctx, "turn/start", params, &resp); err != nil {
		return nil, t.conn.promoteRPCError("turn/start", err)
	}
	t.setActiveTurn(resp.Turn.ID)
	return &resp, nil
}

// callOpts returns a slice of Options that re-derive the current
// resolved values. Used so per-call StartTurn options layer on top of
// connection defaults.
func (o *options) callOpts() []Option {
	out := []Option{}
	if o.model != "" {
		out = append(out, WithModel(o.model))
	}
	if o.cwd != "" {
		out = append(out, WithCwd(o.cwd))
	}
	if o.effort != "" {
		out = append(out, WithEffort(o.effort))
	}
	if o.approval != nil {
		out = append(out, WithApprovalPolicy(*o.approval))
	}
	if o.approvalsReviewer != nil {
		out = append(out, WithApprovalsReviewer(*o.approvalsReviewer))
	}
	if o.turnExtra != nil {
		out = append(out, WithTurnExtra(o.turnExtra))
	}
	return out
}
