package codexcli

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// TestInterrupt_MidTurn drives a turn that emits a few deltas, asks the
// thread to interrupt, and verifies:
//   - the turn/interrupt request is sent and acknowledged
//   - the stream surfaces a turn/completed event with status=interrupted
//   - the event channel closes cleanly (no goroutine leak)
//   - no subsequent ProcessExitError fires (the connection is still live)
func TestInterrupt_MidTurn(t *testing.T) {
	startGoroutines := runtime.NumGoroutine()
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())

	threadID := "thr_int"
	turnID := "turn_int"

	go runInterruptServer(t, fix, threadID, turnID)

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	thread, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}

	stream, err := thread.StartTurn(context.Background(), "count slowly to 20")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Wait for the first delta to confirm the turn is mid-stream, then
	// fire the interrupt.
	deadline := time.After(3 * time.Second)
	gotDelta := false
	for !gotDelta {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatal("stream closed before any delta")
			}
			if _, ok := ev.(*AgentMessageDeltaEvent); ok {
				gotDelta = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for first delta")
		}
	}

	if err := thread.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	// Drain remaining events.
	var sawInterrupted bool
	for {
		ev, ok := <-stream.Events()
		if !ok {
			break
		}
		if e, ok := ev.(*TurnCompletedEvent); ok {
			if e.Turn.Status == schema.TurnInterrupted {
				sawInterrupted = true
			}
		}
	}
	if !sawInterrupted {
		t.Errorf("did not observe TurnCompletedEvent with status=interrupted")
	}

	// Subsequent turn must still work on the same conn — interrupt
	// should not poison the JSON-RPC channel.
	go runInterruptFollowupTurn(t, fix, threadID, "turn_followup")
	stream2, err := thread.StartTurn(context.Background(), "ok now stop")
	if err != nil {
		t.Fatalf("StartTurn followup: %v", err)
	}
	if _, err := drainTurn(stream2, 3*time.Second); err != nil {
		t.Errorf("followup drain: %v", err)
	}
	stream2.Close()
	conn.Close()

	// Allow background goroutines to wind down; modest tolerance — the
	// rpc reader and reaper take a moment to exit after Close.
	checkGoroutineLeak(t, startGoroutines)
}

// TestInterrupt_NoActiveTurn confirms that calling Interrupt when no
// turn is active still succeeds (the server handles it as a no-op).
func TestInterrupt_NoActiveTurn(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())

	threadID := "thr_noturn"
	go func() {
		id, _ := expectRequest(t, fix, "initialize")
		_ = fix.SendResponse(id, basicInitResponse())
		expectNotification(t, fix, "initialized")
		id, _ = expectRequest(t, fix, "thread/start")
		_ = fix.SendResponse(id, basicThreadStartResponse(threadID))
		id, _ = expectRequest(t, fix, "turn/interrupt")
		_ = fix.SendResponse(id, map[string]any{})
		drainStrayFrames(fix)
	}()

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	thread, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	if err := thread.Interrupt(context.Background()); err != nil {
		t.Errorf("Interrupt: %v", err)
	}
}

// runInterruptServer scripts a turn that emits two deltas then waits for
// a turn/interrupt request to complete the turn with status=interrupted.
func runInterruptServer(t *testing.T, fix *BidiFixtureExecutor, threadID, turnID string) {
	t.Helper()
	id, _ := expectRequest(t, fix, "initialize")
	_ = fix.SendResponse(id, basicInitResponse())
	expectNotification(t, fix, "initialized")
	id, _ = expectRequest(t, fix, "thread/start")
	_ = fix.SendResponse(id, basicThreadStartResponse(threadID))
	id, _ = expectRequest(t, fix, "turn/start")
	_ = fix.SendResponse(id, map[string]any{
		"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
	})
	_ = fix.SendNotification("turn/started", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
	})
	_ = fix.SendNotification("item/started", map[string]any{
		"threadId": threadID, "turnId": turnID, "startedAtMs": 1,
		"item": map[string]any{"id": "item_1", "type": "agentMessage"},
	})
	_ = fix.SendNotification("item/agentMessage/delta", map[string]any{
		"threadId": threadID, "turnId": turnID, "itemId": "item_1", "delta": "1\n",
	})
	_ = fix.SendNotification("item/agentMessage/delta", map[string]any{
		"threadId": threadID, "turnId": turnID, "itemId": "item_1", "delta": "2\n",
	})

	// Read the turn/interrupt request — the client will send it once
	// the test signals interrupt; meanwhile any concurrent writes from
	// the server are non-blocking because pipe buffers absorb them.
	id, _ = expectRequest(t, fix, "turn/interrupt")
	_ = fix.SendResponse(id, map[string]any{})

	_ = fix.SendNotification("turn/completed", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "interrupted", "items": []any{}},
	})
}

// runInterruptFollowupTurn lets a second turn run to completion on the
// same thread after an interrupt — proving the connection is reusable.
func runInterruptFollowupTurn(t *testing.T, fix *BidiFixtureExecutor, threadID, turnID string) {
	t.Helper()
	id, _ := expectRequest(t, fix, "turn/start")
	_ = fix.SendResponse(id, map[string]any{
		"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
	})
	_ = fix.SendNotification("turn/started", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
	})
	_ = fix.SendNotification("turn/completed", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "completed", "items": []any{}},
	})
}

// checkGoroutineLeak verifies the goroutine count returns roughly to
// baseline. Some slack is allowed because Go's runtime keeps cached
// worker goroutines alive between tests.
func checkGoroutineLeak(t *testing.T, start int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= start+5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine leak: started with %d, now %d", start, runtime.NumGoroutine())
}

// TestInterrupt_ExitErrorWhenProcessDies confirms that calling Interrupt
// after the subprocess has died returns the typed ProcessExitError
// instead of hanging or panicking.
func TestInterrupt_ExitErrorWhenProcessDies(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())

	threadID := "thr_dead"
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		id, _ := expectRequest(t, fix, "initialize")
		_ = fix.SendResponse(id, basicInitResponse())
		expectNotification(t, fix, "initialized")
		id, _ = expectRequest(t, fix, "thread/start")
		_ = fix.SendResponse(id, basicThreadStartResponse(threadID))
		// Process dies.
		fix.FailFromServer(errors.New("simulated SIGKILL"))
	}()

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	thread, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	wg.Wait()
	// give reaper a moment to record the exit
	time.Sleep(50 * time.Millisecond)

	err = thread.Interrupt(context.Background())
	if err == nil {
		t.Fatal("expected interrupt to error after process death")
	}
	if !errors.Is(err, ErrProcessExited) {
		t.Errorf("err = %v, want errors.Is(ErrProcessExited)", err)
	}
	if !strings.Contains(err.Error(), "process") {
		t.Errorf("err message should mention process: %v", err)
	}
}
