//go:build integration

package codexcli

// Live checks of subagent routing and cascading interrupt, thread naming,
// and request_user_input, against the codex on PATH with a signed-in
// account. They spend about five small turns, one of which spawns a
// subagent:
//
//	go test -tags integration -run 'TestLive_Subagent|TestLive_SetName|TestLive_UserInput' -count=1 -v .

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// TestLive_SubagentInterruptCascades: a subagent's events reach the
// parent's Stream, and Thread.Interrupt stops the subagent's turn too.
// Codex alone leaves it running.
func TestLive_SubagentInterruptCascades(t *testing.T) {
	conn := liveConn(t,
		WithApprovalPolicy(schema.NewAskForApprovalString("never")),
		WithSandbox(schema.SandboxDangerFull))
	th, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	stream, err := th.StartTurn(context.Background(),
		"Spawn exactly one subagent. Its task: run the shell command `sleep 90 && echo child-done` and report the output. Wait for it. Do not run the command yourself.")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()

	var child ChildThread
	deadline := time.After(2 * time.Minute)
	for child.ThreadID == "" {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatal("stream ended before a subagent turn started")
			}
			if ce, ok := ev.(*ChildThreadEvent); ok {
				if _, started := ce.Event.(*TurnStartedEvent); started {
					child = ce.Child
				}
			}
		case <-deadline:
			t.Fatal("no subagent turn on the parent's stream")
		}
	}
	t.Logf("child %s %s turn %s", child.ThreadID, child.AgentPath, child.ActiveTurnID)
	if child.ParentThreadID != th.ID || child.AgentPath == "" || child.ActiveTurnID == "" {
		t.Errorf("child = %+v", child)
	}
	time.Sleep(5 * time.Second)

	if err := th.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	wait := time.Now().Add(15 * time.Second)
	for {
		c, ok := conn.ChildThread(child.ThreadID)
		if ok && c.ActiveTurnID == "" {
			break
		}
		if time.Now().After(wait) {
			t.Fatalf("subagent turn still active 15s after Interrupt: %+v", c)
		}
		time.Sleep(100 * time.Millisecond)
	}
	turn, err := drainTurnObserving(stream, 30*time.Second, nil)
	if err != nil || turn.Status != schema.TurnInterrupted {
		t.Errorf("parent turn = %+v, %v; want interrupted", turn, err)
	}
}

// TestLive_SetName: a thread can be named before and during a turn, the
// rename is announced on the thread's stream, an ephemeral thread is
// ErrThreadEphemeral, and a missing thread is ErrThreadNotFound. The named
// thread is persistent, so it lives in a throwaway CODEX_HOME.
func TestLive_SetName(t *testing.T) {
	home, signedIn := sandboxCodexHome(t)
	if !signedIn {
		t.Skip("codex home not signed in")
	}
	conn := liveMcpConn(t, home)
	th, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	if err := th.SetName(context.Background(), "Before any turn"); err != nil {
		t.Fatalf("SetName before a turn: %v", err)
	}
	stream, err := th.StartTurn(context.Background(), "Reply with the word ok and nothing else.")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()
	renamed := make(chan error, 1)
	go func() { renamed <- th.SetName(context.Background(), "Renamed mid-turn") }()
	var names []string
	if _, err := drainTurnObserving(stream, 2*time.Minute, func(ev Event) {
		if e, ok := ev.(*ThreadNameUpdatedEvent); ok {
			names = append(names, e.Name)
		}
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := <-renamed; err != nil {
		t.Fatalf("SetName mid-turn: %v", err)
	}
	// Codex announces a name set before the first turn only when that
	// turn starts, so the first rename may show up on this stream too.
	if len(names) == 0 || names[len(names)-1] != "Renamed mid-turn" || len(names) > 2 {
		t.Errorf("ThreadNameUpdatedEvent names = %q, want the mid-turn rename last", names)
	}
	missing := &Thread{ID: "01a0f6f0-2dae-7cc3-b640-08b7c810a0f5", conn: conn}
	if err := missing.SetName(context.Background(), "x"); !errors.Is(err, ErrThreadNotFound) {
		t.Errorf("SetName on a missing thread = %v, want ErrThreadNotFound", err)
	}
	eph, err := liveMcpConn(t, home, WithEphemeralThread()).NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread ephemeral: %v", err)
	}
	if err := eph.SetName(context.Background(), "x"); !errors.Is(err, ErrThreadEphemeral) {
		t.Errorf("SetName on an ephemeral thread = %v, want ErrThreadEphemeral", err)
	}
}

// TestLive_UserInput: in Plan mode the model asks through
// request_user_input; an answer keyed by question id reaches it, and the
// dismissed reply reaches it as no answer.
func TestLive_UserInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(*schema.ToolRequestUserInputParams) schema.ToolRequestUserInputResponse
		want   string
	}{
		{"answered", func(p *schema.ToolRequestUserInputParams) schema.ToolRequestUserInputResponse {
			return schema.UserInputAnswers(map[string][]string{p.Questions[0].ID: {"Blue"}})
		}, "blue"},
		{"dismissed", func(*schema.ToolRequestUserInputParams) schema.ToolRequestUserInputResponse {
			return schema.UserInputDismissed()
		}, "no answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked *schema.ToolRequestUserInputParams
			handler := func(_ context.Context, req ServerRequest) (json.RawMessage, error) {
				p, ok, err := req.UserInput()
				if !ok || err != nil {
					return nil, errors.New("unexpected server request " + req.Method)
				}
				mu.Lock()
				asked = p
				mu.Unlock()
				return json.Marshal(tc.answer(p))
			}
			conn := liveConn(t, WithExperimentalAPI(),
				WithApprovalPolicy(schema.NewAskForApprovalString("never")),
				WithServerRequestHandler(handler))
			th, err := conn.NewThread(context.Background())
			if err != nil {
				t.Fatalf("NewThread: %v", err)
			}
			plan := map[string]any{"mode": "plan", "settings": map[string]any{"model": th.Response().Model, "reasoning_effort": "low", "developer_instructions": nil}}
			stream, err := th.StartTurn(context.Background(),
				"Use the request_user_input tool right now to ask me one question: which color do I prefer, red or blue (give those two options). Then reply with exactly the answer you received, or NO ANSWER if you got none. Do nothing else.",
				WithTurnExtra(map[string]any{"collaborationMode": plan}))
			if err != nil {
				t.Fatalf("StartTurn: %v", err)
			}
			defer stream.Close()
			var last string
			if _, err := drainTurnObserving(stream, 2*time.Minute, func(ev Event) {
				if e, ok := ev.(*ItemCompletedEvent); ok && e.Item.Type == schema.ItemTypeAgentMessage {
					last = e.Item.Text
				}
			}); err != nil {
				t.Fatalf("drain: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if asked == nil || len(asked.Questions) == 0 || asked.Questions[0].ID == "" {
				t.Fatalf("no request_user_input question: %+v", asked)
			}
			t.Logf("asked %+v; model said %q", asked.Questions[0], last)
			if !strings.Contains(strings.ToLower(last), tc.want) {
				t.Errorf("model said %q, want it to contain %q", last, tc.want)
			}
		})
	}
}
