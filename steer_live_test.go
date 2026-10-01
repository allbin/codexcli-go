//go:build integration

package codexcli

// Live checks of Thread.SendMessage over turn/steer. They run against the
// codex on PATH with a signed-in account and spend two small model turns
// and one compaction:
//
//	go test -tags integration -run TestLive_SendMessage -count=1 -v .

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

func liveSteerThread(t *testing.T) (*Conn, *Thread) {
	t.Helper()
	// Connect ties the subprocess to ctx, so it must outlive the test body.
	// No cwd: codex writes a trust entry to config.toml for an explicit one.
	conn, err := New(WithEphemeralThread()).Connect(context.Background())
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

// TestLive_SendMessageSteersRunningTurn: the message joins the running
// turn, arrives on its stream and redirects the model; a turn/start sent
// mid-turn folds into the same turn; once the turn ends, both the local
// check and codex's own answer surface as ErrNoActiveTurn.
func TestLive_SendMessageSteersRunningTurn(t *testing.T) {
	_, th := liveSteerThread(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stream, err := th.StartTurn(ctx, "Count slowly from 1 to 60, one number per line. Do not run any tools.")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()
	for ev := range stream.Events() {
		if _, ok := ev.(*AgentMessageDeltaEvent); ok {
			break
		}
	}
	active := th.ActiveTurnID()

	th.setActiveTurn("00000000-0000-0000-0000-000000000000")
	_, err = th.SendMessage(ctx, "wrong turn")
	th.setActiveTurn(active)
	if !errors.Is(err, ErrNoActiveTurn) {
		t.Errorf("SendMessage naming another turn = %v, want ErrNoActiveTurn", err)
	}

	const steer = "Stop counting. Reply with only the word STEERED."
	got, err := th.SendMessage(ctx, steer)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got != active {
		t.Errorf("SendMessage turn = %s, want the active turn %s", got, active)
	}
	folded, err := th.startTurnInput(ctx, []schema.UserInput{schema.TextInput("folded follow-up")})
	if err != nil {
		t.Fatalf("turn/start mid-turn: %v", err)
	}
	if folded.Turn.ID != active {
		t.Errorf("turn/start mid-turn returned turn %s, want it folded into %s", folded.Turn.ID, active)
	}

	var steered, last string
	turn, err := drainTurnObserving(stream, 2*time.Minute, func(ev Event) {
		e, ok := ev.(*ItemCompletedEvent)
		if !ok {
			return
		}
		for _, in := range e.Item.UserMessageContent() {
			if in.Text == steer {
				steered = e.TurnID
			}
		}
		if e.Item.Type == schema.ItemTypeAgentMessage {
			last = e.Item.Text
		}
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if turn.ID != active || steered != active {
		t.Errorf("turn %s, steered userMessage on turn %q, want both %s", turn.ID, steered, active)
	}
	if !strings.Contains(last, "STEERED") {
		t.Errorf("final agent message %q, want STEERED", last)
	}

	if _, err := th.SendMessage(ctx, "late"); !errors.Is(err, ErrNoActiveTurn) {
		t.Errorf("SendMessage on idle thread = %v, want ErrNoActiveTurn", err)
	}
	th.setActiveTurn(active)
	_, err = th.SendMessage(ctx, "stale")
	th.setActiveTurn("")
	if !errors.Is(err, ErrNoActiveTurn) {
		t.Errorf("SendMessage naming the finished turn = %v, want ErrNoActiveTurn from codex", err)
	}
}

// TestLive_SendMessageCompactionTurn: codex refuses both turn/steer and
// turn/start while a compaction turn runs.
func TestLive_SendMessageCompactionTurn(t *testing.T) {
	conn, th := liveSteerThread(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	liveSteerTurn(t, th, "Reply with exactly the text ready and nothing else. Do not run any tools.")

	var resp struct{}
	if err := conn.rpc.Request(ctx, "thread/compact/start", map[string]any{"threadId": th.ID}, &resp); err != nil {
		t.Fatalf("thread/compact/start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for th.ActiveTurnID() == "" {
		if time.Now().After(deadline) {
			t.Fatal("compaction turn never started")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := th.SendMessage(ctx, "during compaction"); !errors.Is(err, ErrTurnNotSteerable) {
		t.Errorf("SendMessage during compaction = %v, want ErrTurnNotSteerable", err)
	}
	if _, err := th.startTurnInput(ctx, []schema.UserInput{schema.TextInput("during compaction")}); !errors.Is(err, ErrTurnNotSteerable) {
		t.Errorf("turn/start during compaction = %v, want ErrTurnNotSteerable", err)
	}
}

func liveSteerTurn(t *testing.T, th *Thread, prompt string) {
	t.Helper()
	stream, err := th.StartTurn(context.Background(), prompt)
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()
	turn, err := drainTurnObserving(stream, 2*time.Minute, nil)
	if err != nil || turn.Status != schema.TurnCompleted {
		t.Fatalf("turn: %v, %+v", err, turn)
	}
}
