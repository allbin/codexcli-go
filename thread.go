package codexcli

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// lastCompleted is the most recent turn/completed id, so a turn/start
	// response read after that turn already finished does not revive it.
	lastCompleted string
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

// startedTurn records the turn a turn/start response names. The reader
// can dispatch that turn's turn/completed before the requester resumes;
// the turn is then over and stays cleared.
func (t *Thread) startedTurn(id string) {
	t.mu.Lock()
	if id != t.lastCompleted {
		t.activeTurn = id
	}
	t.mu.Unlock()
}

func (t *Thread) completedTurn(id string) {
	t.mu.Lock()
	t.activeTurn = ""
	t.lastCompleted = id
	t.mu.Unlock()
}

// Interrupt cancels the in-flight turn on this thread and every subagent
// turn running under it. It returns once the server acknowledges the
// thread's own turn/interrupt; the resulting `turn/completed` arrives on
// the active Stream with status: "interrupted".
//
// Codex does not stop subagents when their parent's turn is interrupted:
// on codex 0.159.3 the child kept running after the parent's turn ended
// interrupted (observed live twice). So Interrupt first interrupts each
// descendant's running turn, concurrently and each bounded by 3s so a
// wedged child cannot hold up the rest, then the thread's own. Children
// are the ones Children reports.
//
// With no active turn on the thread it sends nothing for the thread itself
// and returns nil; codex 0.159.3 rejects a turn/interrupt without a turn id.
//
// Interrupt returns by ctx's deadline even if codex has stopped reading
// stdin; a request write stuck that way stays blocked in a goroutine until
// the connection closes.
func (t *Thread) Interrupt(ctx context.Context) error {
	t.conn.interruptDescendants(ctx, t.ID)
	turnID := t.ActiveTurnID()
	if turnID == "" {
		return t.conn.checkExited()
	}
	return t.conn.interruptBounded(ctx, t.ID, turnID)
}

// SetName sets the thread's name over thread/name/set, the title codex
// shows in its own thread lists. Codex confirms with thread/name/updated,
// which arrives as ThreadNameUpdatedEvent on an open Stream. It works with
// or without a turn running; on a new thread that has not run a turn,
// codex 0.159.3 answers at once but announces the name only when the
// first turn starts. A blank name is rejected before sending, as
// codex rejects an empty one; a thread codex has no record of returns
// ErrThreadNotFound, and an ephemeral thread (WithEphemeralThread), which
// codex keeps no metadata for, returns ErrThreadEphemeral.
func (t *Thread) SetName(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("codexcli: thread name must not be blank")
	}
	if err := t.conn.checkExited(); err != nil {
		return err
	}
	params := schema.ThreadSetNameParams{ThreadID: t.ID, Name: name}
	if err := t.conn.rpc.Request(ctx, schema.MethodThreadNameSet, params, nil); err != nil {
		if isThreadNotFoundError(err) {
			return fmt.Errorf("%s %s: %w", schema.MethodThreadNameSet, t.ID, ErrThreadNotFound)
		}
		if strings.Contains(err.Error(), "ephemeral thread does not support") {
			return fmt.Errorf("%s %s: %w", schema.MethodThreadNameSet, t.ID, ErrThreadEphemeral)
		}
		return t.conn.promoteRPCError(schema.MethodThreadNameSet, err)
	}
	return nil
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
// events, and the earlier Stream ends without a TurnCompletedEvent (Wait
// returns ErrNoTurn). To add input to a
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
		defer t.conn.unsubscribe(t.ID, sub)

		if _, err := t.startTurnInput(streamCtx, input, opts...); err != nil {
			events <- &ErrorEvent{Err: fmt.Errorf("turn/start: %w", err), Fatal: true}
			return
		}
		for {
			select {
			case ev, ok := <-sub.out:
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
	t.startedTurn(resp.Turn.ID)
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
//   - ErrNoActiveTurn: no turn is running, or the turn ended before codex
//     received the message. Nothing was delivered; start a turn with the
//     message instead. If codex names a different active turn and this
//     Thread has seen it start by then, SendMessage retries once against
//     it; if it has not, that is ErrNoActiveTurn too.
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
	for attempt := 0; ; attempt++ {
		turnID := t.ActiveTurnID()
		if turnID == "" {
			return "", fmt.Errorf("turn/steer: %w", ErrNoActiveTurn)
		}
		params := schema.TurnSteerParams{ThreadID: t.ID, Input: input, ExpectedTurnID: turnID}
		var resp schema.TurnSteerResponse
		err := t.conn.rpc.Request(ctx, schema.MethodTurnSteer, params, &resp)
		if err == nil {
			return resp.TurnID, nil
		}
		cerr := classifyTurnInputError(err)
		// A mismatch while ActiveTurnID has since moved on means the
		// turn/started for the new turn landed during the request: steer
		// that one. Once, so a thread nobody keeps current cannot loop.
		if attempt == 0 && errors.Is(cerr, ErrNoActiveTurn) && t.ActiveTurnID() != turnID {
			continue
		}
		if cerr != nil {
			return "", fmt.Errorf("turn/steer: %w", cerr)
		}
		return "", t.conn.promoteRPCError("turn/steer", err)
	}
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
