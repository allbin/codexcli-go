package codexcli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// TestTranscript_HappyTurnReplay drives the SDK against a JSONL
// transcript recorded by cmd/capture from a real codex 0.147.0 install,
// exercising the codepath consumers actually hit — including the noisy
// startup notifications (mcpServer/startupStatus/updated,
// thread/status/changed, account/rateLimits/updated, ...) that used to
// fall through to UnknownEvent.
//
// The transcript is loose-matched: in-frames are matched by method
// only, so per-run id differences don't fail the test.
func TestTranscript_HappyTurnReplay(t *testing.T) {
	entries, err := LoadTranscript("testdata/happy_turn.jsonl")
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("empty transcript")
	}

	clientInfo := clientInfoFromTranscript(entries)
	exec := NewTranscriptFixtureExecutor(t, entries)
	client := NewWithExecutor(exec, WithEphemeralThread(), WithClientInfo(clientInfo))

	stream, err := client.Run(context.Background(), "Reply with exactly: hello world")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	var unknown []string
	var statuses []string
	var cmdLiteral, cmdOutput string
	var usage int64
	turn, err := drainTurnObserving(stream, 5*time.Second, func(ev Event) {
		switch e := ev.(type) {
		case *UnknownEvent:
			unknown = append(unknown, e.Method)
		case *ThreadStatusChangedEvent:
			statuses = append(statuses, e.Status)
		case *TokenUsageUpdatedEvent:
			usage = e.TokenUsage.Total.TotalTokens
		case *ItemCompletedEvent:
			if e.Item.Type == schema.ItemTypeCommandExecution {
				cmdLiteral = e.Item.CommandLiteral()
				if e.Item.AggregatedOutput != nil {
					cmdOutput = *e.Item.AggregatedOutput
				}
			}
		}
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if turn == nil {
		t.Fatal("no turn observed")
	}

	if !strings.EqualFold(string(turn.Status), "completed") {
		t.Errorf("turn status = %q, want completed", turn.Status)
	}

	// A real turn must be fully typed. A new method showing up here is the
	// signal that codex moved and this SDK needs a protocol pass.
	if len(unknown) > 0 {
		t.Errorf("unhandled notifications in a normal turn: %v", unknown)
	}
	// Codex brackets the turn with active/idle.
	if len(statuses) != 2 || statuses[0] != "active" || statuses[1] != "idle" {
		t.Errorf("thread statuses = %v, want [active idle]", statuses)
	}
	// Guards the cmd -> command rename: this decodes to "" if CommandAction
	// or ThreadItem.Command regress.
	if cmdLiteral != "echo hello-from-codex" {
		t.Errorf("CommandLiteral() = %q", cmdLiteral)
	}
	if cmdOutput != "hello-from-codex\n" {
		t.Errorf("AggregatedOutput = %q", cmdOutput)
	}
	// Usage only arrives via thread/tokenUsage/updated now.
	if usage == 0 {
		t.Error("no token usage observed")
	}
	if turn.Usage != nil {
		t.Errorf("Turn.Usage = %+v, want nil (removed from the wire)", turn.Usage)
	}
}

// clientInfoFromTranscript pulls the ClientInfo out of the first
// initialize frame so the replay matches what the recorder actually
// wrote — exercises the marshaler against bytes we know codex accepts.
func clientInfoFromTranscript(entries []transcriptEntry) schema.ClientInfo {
	for _, e := range entries {
		if e.Direction == "in" && e.Frame.Method == "initialize" {
			var p struct {
				ClientInfo schema.ClientInfo `json:"clientInfo"`
			}
			_ = json.Unmarshal(e.Frame.Params, &p)
			return p.ClientInfo
		}
	}
	return schema.ClientInfo{Name: "codexcli_capture", Version: "0.0.1"}
}

// TestTranscript_EffortSettingsReadBack replays a codex 0.153.4 capture of
// a turn/start carrying effort "high" on a connection that negotiated
// experimentalApi. Codex answered with thread/settings/updated just before
// the turn/start response, so the typed event must reach the turn's stream
// ahead of TurnStartedEvent. The capture's hook/* frames came from the
// recording machine's hooks.json and were dropped when scrubbing.
func TestTranscript_EffortSettingsReadBack(t *testing.T) {
	entries, err := LoadTranscript("testdata/effort_settings_turn.jsonl")
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	exec := NewTranscriptFixtureExecutor(t, entries)
	client := NewWithExecutor(exec, WithExperimentalAPI(), WithEphemeralThread(),
		WithClientInfo(clientInfoFromTranscript(entries)))

	ctx := context.Background()
	conn, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	thread, err := conn.NewThread(ctx)
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	if got := thread.Response().ReasoningEffort; got == nil || *got != "medium" {
		t.Errorf("thread/start ReasoningEffort = %v, want medium", got)
	}

	stream, err := thread.StartTurn(ctx, "Reply with exactly the text T1-high", WithEffort("high"))
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()

	var settings *ThreadSettingsUpdatedEvent
	var settingsBeforeStart bool
	var unknown []string
	turn, err := drainTurnObserving(stream, 5*time.Second, func(ev Event) {
		switch e := ev.(type) {
		case *ThreadSettingsUpdatedEvent:
			settings = e
		case *TurnStartedEvent:
			settingsBeforeStart = settings != nil
		case *UnknownEvent:
			unknown = append(unknown, e.Method)
		}
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if turn == nil || turn.Status != "completed" {
		t.Fatalf("turn = %+v", turn)
	}
	if settings == nil {
		t.Fatalf("no ThreadSettingsUpdatedEvent (unknown methods: %v)", unknown)
	}
	if settings.ThreadID != thread.ID || settings.Effort != "high" || settings.Model != "gpt-5.6-sol" {
		t.Errorf("settings = %+v", settings)
	}
	if !settingsBeforeStart {
		t.Error("ThreadSettingsUpdatedEvent arrived after TurnStartedEvent")
	}
	for _, m := range unknown {
		if m == schema.MethodThreadSettingsUpdated {
			t.Errorf("%s still surfaced as UnknownEvent", m)
		}
	}
}
