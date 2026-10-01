package codexcli

import (
	"context"
	"errors"
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
//
// It is a snapshot and does not track later turns. ReasoningEffort is the
// level the thread started with, or for a resumed thread the level it was
// last left at, which may be a per-turn override from an earlier session.
func (t *Thread) Response() schema.ThreadStartResponse { return t.response }

// StartTurn dispatches turn/start with the given prompt and returns a
// stream of typed events scoped to this turn. The stream ends with
// TurnCompletedEvent (or ErrorEvent on transport/protocol failure).
//
// Call it only when no turn is active. Codex runs one turn per thread: a
// turn/start while a turn is running does not start a new one, it folds
// the input into the running turn and returns that turn's id (observed
// live on codex 0.148 and 0.159.3). The new Stream takes over the thread's
// events, and the earlier Stream stops receiving them and never ends on
// its own. To add input to a
// running turn, use SendMessage, which leaves the existing Stream in
// place. While a review or compaction turn runs, codex refuses turn/start
// and StartTurn's stream fails with ErrTurnNotSteerable.
//
// opts layer over the connection's options, and codex keeps some turn
// settings for later turns. WithEffort documents how the two interact.
func (t *Thread) StartTurn(ctx context.Context, prompt string, opts ...Option) (*Stream, error) {
	return t.StartTurnInput(ctx, []schema.UserInput{schema.TextInput(prompt)}, opts...)
}

// StartTurnInput dispatches turn/start with typed user input blocks and
// returns a stream of typed events scoped to this turn. Use this for image,
// local-image, skill, and mention inputs without relying on raw turn extras.
// Options behave as on StartTurn.
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
		if cerr := classifyTurnInputError(err); errors.Is(cerr, ErrTurnNotSteerable) {
			return nil, fmt.Errorf("turn/start: %w", cerr)
		}
		return nil, t.conn.promoteRPCError("turn/start", err)
	}
	t.setActiveTurn(resp.Turn.ID)
	return &resp, nil
}

// SendMessage injects a user message into the turn already running on t,
// without waiting for it to finish. Unlike StartTurn it neither starts nor
// tracks a turn: codex adds the message to the running turn as a
// userMessage item, the model sees it at its next step, and the events
// keep arriving on the Stream that turn's StartTurn returned. It returns
// the id of the turn the message joined. Mirrors
// claudecli-go.Session.SendMessage.
//
// The turn is the one ActiveTurnID reports, sent as turn/steer's
// expectedTurnId precondition. Errors:
//
//   - ErrNoActiveTurn: no turn is running, or the turn ended (or another
//     replaced it) before codex received the message. Nothing was
//     delivered; start a turn with the message instead.
//   - ErrTurnNotSteerable: the running turn is a review or compaction
//     turn, which codex refuses to steer. Nothing was delivered; buffer
//     the message until the turn completes.
func (t *Thread) SendMessage(ctx context.Context, prompt string) (string, error) {
	return t.SendMessageWithInput(ctx, []schema.UserInput{schema.TextInput(prompt)})
}

// SendMessageWithInput is SendMessage for typed input blocks: images,
// local images, skills and mentions.
func (t *Thread) SendMessageWithInput(ctx context.Context, input []schema.UserInput) (string, error) {
	if err := t.conn.checkExited(); err != nil {
		return "", err
	}
	turnID := t.ActiveTurnID()
	if turnID == "" {
		return "", fmt.Errorf("turn/steer: %w", ErrNoActiveTurn)
	}
	params := schema.TurnSteerParams{ThreadID: t.ID, Input: input, ExpectedTurnID: turnID}
	var resp schema.TurnSteerResponse
	if err := t.conn.rpc.Request(ctx, schema.MethodTurnSteer, params, &resp); err != nil {
		if cerr := classifyTurnInputError(err); cerr != nil {
			return "", fmt.Errorf("turn/steer: %w", cerr)
		}
		return "", t.conn.promoteRPCError("turn/steer", err)
	}
	return resp.TurnID, nil
}

// callOpts returns a slice of Options that re-derive the current
// resolved values. Used so per-call StartTurn and ResumeThread options
// layer on top of connection defaults.
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
	if o.threadConfig != nil {
		out = append(out, WithThreadConfig(o.threadConfig))
	}
	return out
}
