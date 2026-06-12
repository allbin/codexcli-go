package codexcli

import (
	"context"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// TestApplyAccumulatedOutput unit-tests the core reconstruction logic:
// fill from the buffer when the server value is empty, defer to the
// server value when present, and drain the buffer on completion either
// way so it can't leak.
func TestApplyAccumulatedOutput(t *testing.T) {
	t.Run("fills from buffer when server left it empty", func(t *testing.T) {
		c := &Conn{}
		c.cmdOutput.append("item_1", "foo")
		c.cmdOutput.append("item_1", "bar")

		item := schema.ThreadItem{ID: "item_1", Type: schema.ItemTypeCommandExecution}
		c.applyAccumulatedOutput(&item)

		if item.AggregatedOutput == nil || *item.AggregatedOutput != "foobar" {
			t.Fatalf("AggregatedOutput = %v, want \"foobar\"", item.AggregatedOutput)
		}
		if _, ok := c.cmdOutput.take("item_1"); ok {
			t.Errorf("buffer not drained after completion")
		}
	})

	t.Run("prefers non-empty server value and still drains", func(t *testing.T) {
		c := &Conn{}
		c.cmdOutput.append("item_2", "buffered")

		server := "server-output"
		item := schema.ThreadItem{ID: "item_2", Type: schema.ItemTypeCommandExecution, AggregatedOutput: &server}
		c.applyAccumulatedOutput(&item)

		if item.AggregatedOutput == nil || *item.AggregatedOutput != "server-output" {
			t.Fatalf("AggregatedOutput = %v, want server value to win", item.AggregatedOutput)
		}
		if _, ok := c.cmdOutput.take("item_2"); ok {
			t.Errorf("buffer not drained when server value wins")
		}
	})

	t.Run("empty server-set value is treated as empty", func(t *testing.T) {
		c := &Conn{}
		c.cmdOutput.append("item_3", "from-deltas")

		empty := ""
		item := schema.ThreadItem{ID: "item_3", Type: schema.ItemTypeCommandExecution, AggregatedOutput: &empty}
		c.applyAccumulatedOutput(&item)

		if item.AggregatedOutput == nil || *item.AggregatedOutput != "from-deltas" {
			t.Fatalf("AggregatedOutput = %v, want \"from-deltas\"", item.AggregatedOutput)
		}
	})

	t.Run("no buffered output leaves the item untouched", func(t *testing.T) {
		c := &Conn{}
		item := schema.ThreadItem{ID: "item_4", Type: schema.ItemTypeCommandExecution}
		c.applyAccumulatedOutput(&item)
		if item.AggregatedOutput != nil {
			t.Errorf("AggregatedOutput = %v, want nil", item.AggregatedOutput)
		}
	})
}

// TestAccumulatedOutput_StreamPopulates drives a full turn through the
// fixture with WithAccumulatedOutput set: the server streams command
// output deltas and completes the item with a null aggregatedOutput. The
// ItemCompletedEvent the consumer sees should carry the reconstructed
// output.
func TestAccumulatedOutput_StreamPopulates(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread(), WithAccumulatedOutput())

	go runCommandOutputServer(t, fix, &cmdOutputScript{
		deltas: []string{"line1\n", "line2\n", "line3\n"},
	})

	stream, err := client.Run(context.Background(), "run a command")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	item := drainForCommandItem(t, stream, 3*time.Second)
	if item == nil {
		t.Fatal("no commandExecution ItemCompletedEvent observed")
	}
	if item.AggregatedOutput == nil {
		t.Fatalf("AggregatedOutput is nil, want reconstructed output")
	}
	if got, want := *item.AggregatedOutput, "line1\nline2\nline3\n"; got != want {
		t.Errorf("AggregatedOutput = %q, want %q", got, want)
	}
}

// TestAccumulatedOutput_PrefersServerValue confirms that when the server
// does populate aggregatedOutput, the SDK leaves it alone even with the
// option enabled and buffered deltas present.
func TestAccumulatedOutput_PrefersServerValue(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread(), WithAccumulatedOutput())

	server := "authoritative server output"
	go runCommandOutputServer(t, fix, &cmdOutputScript{
		deltas:           []string{"ignored1", "ignored2"},
		serverAggregated: &server,
	})

	stream, err := client.Run(context.Background(), "run a command")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	item := drainForCommandItem(t, stream, 3*time.Second)
	if item == nil || item.AggregatedOutput == nil {
		t.Fatal("no populated commandExecution ItemCompletedEvent observed")
	}
	if got := *item.AggregatedOutput; got != server {
		t.Errorf("AggregatedOutput = %q, want server value %q", got, server)
	}
}

// TestAccumulatedOutput_DisabledByDefault pins the existing behavior:
// without the option, completed items pass through unchanged and a
// server-null aggregatedOutput stays nil.
func TestAccumulatedOutput_DisabledByDefault(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())

	go runCommandOutputServer(t, fix, &cmdOutputScript{
		deltas: []string{"line1\n", "line2\n"},
	})

	stream, err := client.Run(context.Background(), "run a command")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	item := drainForCommandItem(t, stream, 3*time.Second)
	if item == nil {
		t.Fatal("no commandExecution ItemCompletedEvent observed")
	}
	if item.AggregatedOutput != nil {
		t.Errorf("AggregatedOutput = %q, want nil (option disabled)", *item.AggregatedOutput)
	}
}

// cmdOutputScript parameterizes runCommandOutputServer.
type cmdOutputScript struct {
	deltas           []string
	serverAggregated *string // when non-nil, set on the item/completed payload
}

// runCommandOutputServer scripts a one-turn exchange whose single item is
// a commandExecution that streams output deltas before completing.
func runCommandOutputServer(t *testing.T, fix *BidiFixtureExecutor, s *cmdOutputScript) {
	t.Helper()
	threadID := "thr_cmd"
	turnID := "turn_cmd"
	itemID := "item_cmd"

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
		"item": map[string]any{"id": itemID, "type": "commandExecution", "command": "echo hi"},
	})
	for _, d := range s.deltas {
		_ = fix.SendNotification("item/commandExecution/outputDelta", map[string]any{
			"threadId": threadID, "turnId": turnID, "itemId": itemID, "delta": d,
		})
	}

	completedItem := map[string]any{
		"id": itemID, "type": "commandExecution", "status": "completed", "exitCode": 0,
	}
	if s.serverAggregated != nil {
		completedItem["aggregatedOutput"] = *s.serverAggregated
	}
	_ = fix.SendNotification("item/completed", map[string]any{
		"threadId": threadID, "turnId": turnID, "item": completedItem,
	})
	_ = fix.SendNotification("turn/completed", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "completed", "items": []any{}},
	})
	drainStrayFrames(fix)
}

// drainForCommandItem reads stream events until the turn completes,
// returning the commandExecution item from the ItemCompletedEvent (or nil
// if none was seen).
func drainForCommandItem(t *testing.T, s *Stream, timeout time.Duration) *schema.ThreadItem {
	t.Helper()
	deadline := time.After(timeout)
	var cmd *schema.ThreadItem
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return cmd
			}
			if e, ok := ev.(*ItemCompletedEvent); ok && e.Item.Type == schema.ItemTypeCommandExecution {
				it := e.Item
				cmd = &it
			}
			if _, done := ev.(*TurnCompletedEvent); done {
				return cmd
			}
		case <-deadline:
			t.Fatal("timeout draining stream for command item")
			return nil
		}
	}
}
