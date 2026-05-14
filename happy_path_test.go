package codexcli

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestHappyPath drives a full one-turn exchange against the fixture
// executor. The point is to lock down the wire shape codexcli sends —
// initialize, initialized, thread/start, turn/start — and to confirm
// the streamed events surface to consumers in the expected order.
func TestHappyPath_OneTurn(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread(), WithCwd("/tmp"))

	go runHappyPathServer(t, fix)

	stream, err := client.Run(context.Background(), "say hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	var sawTurnStart, sawCompleted bool
	var deltaText string
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				break loop
			}
			switch e := ev.(type) {
			case *TurnStartedEvent:
				sawTurnStart = true
				if e.Turn.ID == "" {
					t.Errorf("TurnStartedEvent missing turn id")
				}
			case *AgentMessageDeltaEvent:
				deltaText += e.Delta
			case *TurnCompletedEvent:
				sawCompleted = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for events")
		}
	}

	if !sawTurnStart {
		t.Errorf("missing TurnStartedEvent")
	}
	if !sawCompleted {
		t.Errorf("missing TurnCompletedEvent")
	}
	if deltaText != "hello" {
		t.Errorf("deltaText = %q, want %q", deltaText, "hello")
	}

	turn, werr := stream.Wait()
	if werr != nil {
		t.Errorf("Wait err = %v", werr)
	}
	if turn == nil || turn.Status != "completed" {
		t.Errorf("turn = %+v", turn)
	}
}

// runHappyPathServer is the scripted server side of TestHappyPath. Each
// step is intentionally sequential: read the next request, respond,
// emit notifications, advance.
func runHappyPathServer(t *testing.T, fix *BidiFixtureExecutor) {
	t.Helper()

	// 1. initialize
	id, _ := expectRequest(t, fix, "initialize")
	_ = fix.SendResponse(id, map[string]any{
		"codexHome":      "/tmp/codex-home",
		"platformFamily": "unix",
		"platformOs":     "linux",
		"userAgent":      "codex-cli/0.130.0",
	})
	// 2. initialized notification
	expectNotification(t, fix, "initialized")

	// 3. thread/start
	threadID := "thr_test_1"
	id, _ = expectRequest(t, fix, "thread/start")
	_ = fix.SendResponse(id, map[string]any{
		"thread":         map[string]any{"id": threadID, "sessionId": "sess_1", "cwd": "/tmp", "cliVersion": "0.130.0", "modelProvider": "openai", "ephemeral": true, "createdAt": 1, "updatedAt": 1, "preview": ""},
		"cwd":            "/tmp",
		"model":          "gpt-5",
		"modelProvider":  "openai",
		"approvalPolicy": "never",
		"sandbox":        map[string]any{"mode": "read-only"},
	})

	// 4. turn/start
	turnID := "turn_test_1"
	id, _ = expectRequest(t, fix, "turn/start")
	_ = fix.SendResponse(id, map[string]any{
		"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
	})

	// 5. notifications: turn/started, item lifecycle, turn/completed
	_ = fix.SendNotification("turn/started", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
	})
	_ = fix.SendNotification("item/started", map[string]any{
		"threadId":    threadID,
		"turnId":      turnID,
		"startedAtMs": 1,
		"item":        map[string]any{"id": "item_1", "type": "agentMessage"},
	})
	_ = fix.SendNotification("item/agentMessage/delta", map[string]any{
		"threadId": threadID, "turnId": turnID, "itemId": "item_1", "delta": "he",
	})
	_ = fix.SendNotification("item/agentMessage/delta", map[string]any{
		"threadId": threadID, "turnId": turnID, "itemId": "item_1", "delta": "llo",
	})
	_ = fix.SendNotification("item/completed", map[string]any{
		"threadId": threadID, "turnId": turnID,
		"item": map[string]any{"id": "item_1", "type": "agentMessage", "text": "hello"},
	})
	_ = fix.SendNotification("turn/completed", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "completed", "items": []any{}},
	})

	// Drain any stray writes from the client (e.g. ctx-cancel cleanup
	// notifications) until the test closes the connection.
	go func() {
		for {
			if _, err := fix.ReadFrame(); err != nil {
				return
			}
		}
	}()
}

// stable marker so json import is exercised even if test list reshuffles.
var _ = json.RawMessage("{}")
