package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// connectFake connects to fix with opts and starts thread thr_1 on it. The
// caller's server goroutine must answer initialize and thread/start first
// (serveThreadStart).
func connectFake(t *testing.T, fix *BidiFixtureExecutor, opts ...Option) (*Conn, *Thread) {
	t.Helper()
	conn, err := NewWithExecutor(fix, append([]Option{WithEphemeralThread()}, opts...)...).Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	th, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	return conn, th
}

// blockingApprover records each approval's ctx and blocks until it is
// cancelled or release closes.
type blockingApprover struct {
	ctxs    chan context.Context
	release chan struct{}
}

func newBlockingApprover() *blockingApprover {
	return &blockingApprover{ctxs: make(chan context.Context, 4), release: make(chan struct{})}
}

func (b *blockingApprover) approve(ctx context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
	b.ctxs <- ctx
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return Accept{}, nil
	}
}

func (b *blockingApprover) serve(ctx context.Context, _ ServerRequest) (json.RawMessage, error) {
	b.ctxs <- ctx
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return json.RawMessage(`{"answers":{}}`), nil
	}
}

func (b *blockingApprover) next(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-b.ctxs:
		return ctx
	case <-time.After(3 * time.Second):
		t.Fatal("handler never called")
		return nil
	}
}

func requireCancelled(t *testing.T, ctx context.Context, why string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatalf("handler ctx not cancelled %s", why)
	}
}

// requireNoFrame fails if the client writes anything within a short window.
func requireNoFrame(t *testing.T, frames <-chan rpcFrame, why string) {
	t.Helper()
	select {
	case f := <-frames:
		t.Errorf("client sent %s id=%s error=%+v %s", f.Method, f.ID, f.Error, why)
	case <-time.After(150 * time.Millisecond):
	}
}

func readFrames(fix *BidiFixtureExecutor) <-chan rpcFrame {
	frames := make(chan rpcFrame, 16)
	go func() {
		for {
			f, err := fix.ReadFrame()
			if err != nil {
				return
			}
			frames <- f
		}
	}()
	return frames
}

// TestServerRequest_WithdrawnByResolved: codex withdraws a pending
// approval (observed live on 0.159.3 after turn/interrupt: turn/completed,
// then serverRequest/resolved). The handler's ctx is cancelled and no
// answer is sent for the withdrawn request.
func TestServerRequest_WithdrawnByResolved(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requestID json.RawMessage
		method    string
		params    map[string]any
	}{
		{"approval, numeric id", json.RawMessage(`0`), schema.MethodCommandExecutionRequestApproval, commandApprovalParams("thr_1", "turn_1", "item_1", "touch x")},
		{"user input, string id", json.RawMessage(`"req-7"`), schema.MethodToolRequestUserInput, map[string]any{"threadId": "thr_1", "turnId": "turn_1", "itemId": "item_2", "questions": []any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix := NewBidiFixtureExecutor()
			b := newBlockingApprover()
			go serveThreadStart(t, fix, "thr_1")
			_, _ = connectFake(t, fix, WithApprovalHandler(b.approve), WithServerRequestHandler(b.serve))
			frames := readFrames(fix)

			_ = fix.SendRequest(tc.requestID, tc.method, tc.params)
			ctx := b.next(t)
			_ = fix.SendNotification("serverRequest/resolved", map[string]any{"threadId": "thr_1", "requestId": tc.requestID})
			requireCancelled(t, ctx, "after serverRequest/resolved")
			requireNoFrame(t, frames, "for a withdrawn request")
		})
	}
}

// TestServerRequest_CancelledOnTurnEnd: a request belonging to a turn is
// withdrawn when that turn completes, even before serverRequest/resolved,
// and a request for another turn is left alone.
func TestServerRequest_CancelledOnTurnEnd(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	b := newBlockingApprover()
	go serveThreadStart(t, fix, "thr_1")
	_, _ = connectFake(t, fix, WithApprovalHandler(b.approve))
	frames := readFrames(fix)

	_ = fix.SendRequest(json.RawMessage(`1`), schema.MethodCommandExecutionRequestApproval, commandApprovalParams("thr_1", "turn_1", "item_1", "a"))
	ended := b.next(t)
	_ = fix.SendRequest(json.RawMessage(`2`), schema.MethodCommandExecutionRequestApproval, commandApprovalParams("thr_1", "turn_2", "item_2", "b"))
	other := b.next(t)

	_ = fix.SendNotification("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": "turn_1", "status": "interrupted", "items": []any{}}})
	requireCancelled(t, ended, "after its turn completed")
	if other.Err() != nil {
		t.Fatal("a request for another turn was cancelled")
	}
	requireNoFrame(t, frames, "for the withdrawn request")

	close(b.release)
	select {
	case f := <-frames:
		if string(f.ID) != "2" || f.Error != nil {
			t.Errorf("answer = id %s error %+v, want a result for request 2", f.ID, f.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the live request was never answered")
	}
}

// TestPing_RoundTrips: Ping is a request codex must answer. A peer that
// answers, even with an error, is alive; one that reads and never answers
// fails Ping within the timeout.
func TestPing_RoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(fix *BidiFixtureExecutor, id json.RawMessage)
		alive bool
	}{
		{"result", func(fix *BidiFixtureExecutor, id json.RawMessage) {
			_ = fix.SendResponse(id, map[string]any{"data": []string{}, "nextCursor": nil})
		}, true},
		{"error response", func(fix *BidiFixtureExecutor, id json.RawMessage) {
			_ = fix.SendErrorResponse(id, rpcCodeInvalidRequest, "unknown variant `thread/loaded/list`")
		}, true},
		{"silence", func(*BidiFixtureExecutor, json.RawMessage) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix := NewBidiFixtureExecutor()
			methods := make(chan string, 1)
			go func() {
				serveThreadStart(t, fix, "thr_1")
				f, err := fix.ReadFrame()
				if err != nil {
					return
				}
				methods <- f.Method
				tc.reply(fix, f.ID)
				drainStrayFrames(fix)
			}()
			conn, _ := connectFake(t, fix)
			start := time.Now()
			err := conn.Ping(context.Background(), 200*time.Millisecond)
			if tc.alive && err != nil {
				t.Fatalf("Ping = %v, want nil", err)
			}
			if !tc.alive {
				if !errors.Is(err, ErrPingTimeout) {
					t.Fatalf("Ping = %v, want ErrPingTimeout", err)
				}
				if d := time.Since(start); d > time.Second {
					t.Errorf("Ping took %s with a 200ms timeout", d)
				}
			}
			select {
			case m := <-methods:
				if m != "thread/loaded/list" {
					t.Errorf("Ping sent %q", m)
				}
			case <-time.After(time.Second):
				t.Fatal("Ping sent nothing")
			}
		})
	}
}

// TestDelivery_Lossless: a burst far larger than any channel buffer, read
// slowly, arrives whole and in order, ending with turn/completed.
func TestDelivery_Lossless(t *testing.T) {
	const n = 2000
	fix := NewBidiFixtureExecutor()
	go func() {
		serveThreadStart(t, fix, "thr_1")
		id, _ := expectRequest(t, fix, "turn/start")
		turn := map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}}
		_ = fix.SendResponse(id, map[string]any{"turn": turn})
		_ = fix.SendNotification("turn/started", map[string]any{"threadId": "thr_1", "turn": turn})
		for i := range n {
			_ = fix.SendNotification("item/agentMessage/delta", map[string]any{"threadId": "thr_1", "turnId": "turn_1", "itemId": "m", "delta": fmt.Sprint(i)})
		}
		_ = fix.SendNotification("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": "turn_1", "status": "completed", "items": []any{}}})
		drainStrayFrames(fix)
	}()
	_, th := connectFake(t, fix)
	stream, err := th.StartTurn(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	time.Sleep(300 * time.Millisecond) // let the reader outrun the consumer
	next := 0
	completed := false
	for ev := range stream.Events() {
		switch e := ev.(type) {
		case *AgentMessageDeltaEvent:
			if e.Delta != fmt.Sprint(next) {
				t.Fatalf("delta %q, want %d", e.Delta, next)
			}
			next++
		case *TurnCompletedEvent:
			completed = true
		}
	}
	if next != n || !completed {
		t.Fatalf("got %d of %d deltas, completed=%v", next, n, completed)
	}
}

// TestDelivery_ExitIsLast: a process exit while a subscriber is backed up
// still delivers ProcessExitEvent, after everything queued before it.
func TestDelivery_ExitIsLast(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	go func() {
		serveThreadStart(t, fix, "thr_1")
		id, _ := expectRequest(t, fix, "turn/start")
		turn := map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}}
		_ = fix.SendResponse(id, map[string]any{"turn": turn})
		for i := range 500 {
			_ = fix.SendNotification("item/agentMessage/delta", map[string]any{"threadId": "thr_1", "turnId": "turn_1", "itemId": "m", "delta": fmt.Sprint(i)})
		}
		fix.FailFromServer(errors.New("signal: killed"))
	}()
	conn, th := connectFake(t, fix)
	stream, err := th.StartTurn(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	select {
	case <-conn.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done never closed")
	}
	if conn.ExitError() == nil {
		t.Fatal("Done closed before ExitError was set")
	}
	deltas := 0
	var last Event
	for ev := range stream.Events() {
		if _, ok := ev.(*AgentMessageDeltaEvent); ok {
			deltas++
		}
		last = ev
	}
	if deltas != 500 {
		t.Errorf("got %d of 500 deltas", deltas)
	}
	if _, ok := last.(*ProcessExitEvent); !ok {
		t.Errorf("last event %T, want *ProcessExitEvent", last)
	}
}

// TestResumeThread_NoRolloutIsNotFound: codex 0.159.3 answers thread/resume
// for an unknown or deleted thread with "no rollout found".
func TestResumeThread_NoRolloutIsNotFound(t *testing.T) {
	const msg = "no rollout found for thread id 01a0f6f0-2dae-7cc3-b640-08b7c810a0f5"
	fix := NewBidiFixtureExecutor()
	go func() {
		id, _ := expectRequest(t, fix, "initialize")
		_ = fix.SendResponse(id, basicInitResponse())
		expectNotification(t, fix, "initialized")
		id, _ = expectRequest(t, fix, "thread/resume")
		_ = fix.SendErrorResponse(id, rpcCodeInvalidRequest, msg)
		drainStrayFrames(fix)
	}()
	conn, err := NewWithExecutor(fix).Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	_, err = conn.ResumeThread(context.Background(), "01a0f6f0-2dae-7cc3-b640-08b7c810a0f5")
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("ResumeThread = %v, want ErrThreadNotFound", err)
	}
}

// TestServerRequest_ResolvedRightBehindRequest: codex can withdraw a request
// in the frame right after it, before its handler goroutine runs. The
// handler still sees a cancelled ctx and nothing is answered.
func TestServerRequest_ResolvedRightBehindRequest(t *testing.T) {
	for range 20 {
		fix := NewBidiFixtureExecutor()
		b := newBlockingApprover()
		go serveThreadStart(t, fix, "thr_1")
		_, _ = connectFake(t, fix, WithApprovalHandler(b.approve))
		frames := readFrames(fix)
		_ = fix.SendRequest(json.RawMessage(`3`), schema.MethodCommandExecutionRequestApproval, commandApprovalParams("thr_1", "turn_1", "item_1", "a"))
		_ = fix.SendNotification("serverRequest/resolved", map[string]any{"threadId": "thr_1", "requestId": 3})
		requireCancelled(t, b.next(t), "when resolved arrived before the handler ran")
		requireNoFrame(t, frames, "for a withdrawn request")
	}
}

// TestServerRequest_LegacyCancelledOnTurnEnd: a legacy v1 approval carries
// a call id, not a turn id, so any completion of its thread's turn
// withdraws it.
func TestServerRequest_LegacyCancelledOnTurnEnd(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	b := newBlockingApprover()
	go serveThreadStart(t, fix, "thr_1")
	_, _ = connectFake(t, fix, WithApprovalHandler(b.approve))
	frames := readFrames(fix)
	_ = fix.SendRequest(json.RawMessage(`4`), schema.MethodExecCommandApproval, map[string]any{
		"conversationId": "thr_1", "callId": "call_1", "command": []string{"touch", "x"}, "cwd": "/tmp", "parsedCmd": []any{},
	})
	ctx := b.next(t)
	_ = fix.SendNotification("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": "turn_9", "status": "interrupted", "items": []any{}}})
	requireCancelled(t, ctx, "after its thread's turn completed")
	requireNoFrame(t, frames, "for the withdrawn request")
}

// TestPing_ProcessDiesMidPing: the process exits while the probe is in
// flight. Ping reports the exit, not a bare transport error.
func TestPing_ProcessDiesMidPing(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	go func() {
		serveThreadStart(t, fix, "thr_1")
		if _, err := fix.ReadFrame(); err != nil {
			return
		}
		fix.FailFromServer(errors.New("signal: killed"))
	}()
	conn, _ := connectFake(t, fix)
	err := conn.Ping(context.Background(), 2*time.Second)
	var exit *ProcessExitError
	if !errors.As(err, &exit) {
		t.Fatalf("Ping = %v, want a *ProcessExitError", err)
	}
}
