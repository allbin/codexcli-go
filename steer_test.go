package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// steerWire is testdata/steer_compact_wire.json: a live capture of a turn
// steered mid-stream, followed by a compaction turn.
type steerWire struct {
	ThreadID      string `json:"threadId"`
	SteeredTurnID string `json:"steeredTurnId"`
	Notifications []struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	} `json:"notifications"`
}

func loadSteerWire(t *testing.T) steerWire {
	t.Helper()
	raw, err := os.ReadFile("testdata/steer_compact_wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var w steerWire
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

// index returns the position of the n-th (0-based) notification matching
// method whose params contain substr.
func (w steerWire) index(t *testing.T, method, substr string, n int) int {
	t.Helper()
	for i, nf := range w.Notifications {
		if nf.Method == method && strings.Contains(string(nf.Params), substr) {
			if n == 0 {
				return i
			}
			n--
		}
	}
	t.Fatalf("no notification %s containing %q", method, substr)
	return -1
}

func (w steerWire) replay(fix *BidiFixtureExecutor, from, to int) {
	for _, nf := range w.Notifications[from:to] {
		_ = fix.SendNotification(nf.Method, nf.Params)
	}
}

func steerConn(t *testing.T, fix *BidiFixtureExecutor) *Thread {
	t.Helper()
	conn, err := NewWithExecutor(fix, WithEphemeralThread()).Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	th, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	return th
}

func serveThreadStart(t *testing.T, fix *BidiFixtureExecutor, threadID string) {
	id, _ := expectRequest(t, fix, "initialize")
	_ = fix.SendResponse(id, basicInitResponse())
	expectNotification(t, fix, "initialized")
	id, _ = expectRequest(t, fix, "thread/start")
	_ = fix.SendResponse(id, basicThreadStartResponse(threadID))
}

// TestSendMessage_SteersRunningTurn replays the captured wire: the steer
// carries the active turn as expectedTurnId, the steered userMessage
// arrives on the original stream, and once the turn completes SendMessage
// fails locally with ErrNoActiveTurn. The compaction turn that follows
// refuses both turn/steer and turn/start with ErrTurnNotSteerable.
func TestSendMessage_SteersRunningTurn(t *testing.T) {
	w := loadSteerWire(t)
	turnStarted := w.index(t, "turn/started", w.SteeredTurnID, 0)
	firstAnswer := w.index(t, "item/completed", `"agentMessage"`, 0)
	firstDone := w.index(t, "turn/completed", w.SteeredTurnID, 0)
	compactStarted := w.index(t, "turn/started", "", 1)
	var compact struct {
		Turn schema.Turn `json:"turn"`
	}
	_ = json.Unmarshal(w.Notifications[compactStarted].Params, &compact)

	fix := NewBidiFixtureExecutor()
	steerParams := make(chan schema.TurnSteerParams, 2)
	served := make(chan struct{})
	go func() {
		defer close(served)
		serveThreadStart(t, fix, w.ThreadID)
		id, _ := expectRequest(t, fix, "turn/start")
		_ = fix.SendResponse(id, map[string]any{
			"turn": map[string]any{"id": w.SteeredTurnID, "status": "inProgress", "items": []any{}},
		})
		w.replay(fix, turnStarted, firstAnswer+1)

		id, raw := expectRequest(t, fix, schema.MethodTurnSteer)
		var p schema.TurnSteerParams
		_ = json.Unmarshal(raw, &p)
		steerParams <- p
		_ = fix.SendResponse(id, schema.TurnSteerResponse{TurnID: w.SteeredTurnID})
		w.replay(fix, firstAnswer+1, firstDone+1)

		// SendMessage on the idle thread must not reach the server: the
		// next request is the interrupt.
		id, _ = expectRequest(t, fix, "turn/interrupt")
		_ = fix.SendResponse(id, map[string]any{})

		w.replay(fix, firstDone+1, compactStarted+1)
		id, raw = expectRequest(t, fix, schema.MethodTurnSteer)
		_ = json.Unmarshal(raw, &p)
		steerParams <- p
		_ = fix.SendErrorResponseData(id, rpcCodeInvalidRequest, "cannot steer a compact turn", map[string]any{
			"message":        "cannot steer a compact turn",
			"codexErrorInfo": map[string]any{"activeTurnNotSteerable": map[string]any{"turnKind": "compact"}},
		})
		id, _ = expectRequest(t, fix, "turn/start")
		_ = fix.SendErrorResponse(id, -32603, "failed to submit turn input: ActiveTurnNotSteerable { turn_kind: Compact }")
		drainStrayFrames(fix)
	}()

	th := steerConn(t, fix)
	stream, err := th.StartTurn(context.Background(), "Count slowly from 1 to 40")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	for ev := range stream.Events() {
		if e, ok := ev.(*ItemCompletedEvent); ok && e.Item.Type == schema.ItemTypeAgentMessage {
			break
		}
	}

	const steer = "Forget counting. Just reply with the single word STEERED."
	got, err := th.SendMessage(context.Background(), steer)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got != w.SteeredTurnID {
		t.Errorf("SendMessage turn = %q, want %q", got, w.SteeredTurnID)
	}
	p := <-steerParams
	if p.ThreadID != w.ThreadID || p.ExpectedTurnID != w.SteeredTurnID {
		t.Errorf("turn/steer params = %+v, want thread %s expected turn %s", p, w.ThreadID, w.SteeredTurnID)
	}
	if len(p.Input) != 1 || p.Input[0].Text != steer {
		t.Errorf("turn/steer input = %+v", p.Input)
	}

	var steered bool
	turn, err := drainTurnObserving(stream, 3*time.Second, func(ev Event) {
		if e, ok := ev.(*ItemCompletedEvent); ok {
			for _, in := range e.Item.UserMessageContent() {
				steered = steered || in.Text == steer
			}
		}
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if turn.ID != w.SteeredTurnID || !steered {
		t.Errorf("turn %s steered=%v, want %s with the steered userMessage on its stream", turn.ID, steered, w.SteeredTurnID)
	}

	if _, err := th.SendMessage(context.Background(), "late"); !errors.Is(err, ErrNoActiveTurn) {
		t.Errorf("SendMessage after turn/completed = %v, want ErrNoActiveTurn", err)
	}
	if err := th.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for th.ActiveTurnID() != compact.Turn.ID {
		if time.Now().After(deadline) {
			t.Fatalf("compaction turn %s never became active", compact.Turn.ID)
		}
		time.Sleep(time.Millisecond)
	}
	_, err = th.SendMessage(context.Background(), "during compaction")
	if !errors.Is(err, ErrTurnNotSteerable) || !strings.Contains(err.Error(), "compact") {
		t.Errorf("SendMessage during compaction = %v, want ErrTurnNotSteerable naming compact", err)
	}
	if p := <-steerParams; p.ExpectedTurnID != compact.Turn.ID {
		t.Errorf("compaction steer expectedTurnId = %s, want %s", p.ExpectedTurnID, compact.Turn.ID)
	}

	stream2, err := th.StartTurn(context.Background(), "during compaction")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	_, err = stream2.Wait()
	if !errors.Is(err, ErrTurnNotSteerable) {
		t.Errorf("StartTurn during compaction = %v, want ErrTurnNotSteerable", err)
	}
	<-served
}

// TestSendMessage_TurnEndedInFlight: the turn ends between ActiveTurnID
// and codex receiving the steer. Both of codex's answers for that race
// surface as ErrNoActiveTurn; any other rejection is passed through.
func TestSendMessage_TurnEndedInFlight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		message string
		want    error
	}{
		{"turn completed", rpcCodeInvalidRequest, "no active turn to steer", ErrNoActiveTurn},
		{"turn replaced", rpcCodeInvalidRequest, "expected active turn id `turn_1` but found `turn_2`", ErrNoActiveTurn},
		{"review turn, no data", rpcCodeInvalidRequest, "cannot steer a review turn", ErrTurnNotSteerable},
		{"other", -32603, "boom", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix := NewBidiFixtureExecutor()
			registered := make(chan struct{})
			go func() {
				serveThreadStart(t, fix, "thr_1")
				// A turn/started for a thread NewThread has not returned
				// yet would be dropped.
				<-registered
				_ = fix.SendNotification("turn/started", map[string]any{
					"threadId": "thr_1",
					"turn":     map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}},
				})
				id, _ := expectRequest(t, fix, schema.MethodTurnSteer)
				_ = fix.SendErrorResponse(id, tc.code, tc.message)
				drainStrayFrames(fix)
			}()
			th := steerConn(t, fix)
			close(registered)
			deadline := time.Now().Add(3 * time.Second)
			for th.ActiveTurnID() == "" {
				if time.Now().After(deadline) {
					t.Fatal("turn never became active")
				}
				time.Sleep(time.Millisecond)
			}
			_, err := th.SendMessage(context.Background(), "hi")
			if err == nil {
				t.Fatal("SendMessage succeeded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && (errors.Is(err, ErrNoActiveTurn) || errors.Is(err, ErrTurnNotSteerable)) {
				t.Errorf("err = %v, want neither sentinel", err)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("err = %v, want codex's message kept", err)
			}
		})
	}
}
