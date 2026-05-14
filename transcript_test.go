package codexcli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// TestTranscript_HappyTurnReplay drives the SDK against the JSONL
// transcript recorded by cmd/capture, exercising the codepath consumers
// will hit against a real codex install — including the noisy startup
// notifications (configWarning, mcpServer/startupStatus/updated,
// thread/status/changed, account/rateLimits/updated, ...) that surface
// as UnknownEvent today.
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

	turn, err := drainTurn(stream, 5*time.Second)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if turn == nil {
		t.Fatal("no turn observed")
	}

	if !strings.EqualFold(string(turn.Status), "completed") {
		t.Errorf("turn status = %q, want completed", turn.Status)
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
