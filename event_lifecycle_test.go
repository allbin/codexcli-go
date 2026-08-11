package codexcli

import (
	"encoding/json"
	"log/slog"
	"testing"
)

// newDispatchConn builds a Conn wired only far enough to route
// notifications to one subscriber. It skips the subprocess entirely —
// dispatchNotification is pure decode-and-deliver.
func newDispatchConn(t *testing.T, threadID string) (*Conn, <-chan Event) {
	t.Helper()
	c := &Conn{options: resolveOptions(nil, nil), logger: slog.New(discardHandler{})}
	return c, c.subscribe(threadID)
}

// recvEvent pulls one event from the subscriber, failing if none is
// buffered. Dispatch is synchronous, so a non-blocking read is enough.
func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	default:
		t.Fatal("no event delivered")
		return nil
	}
}

// TestDispatch_ThreadStatusChanged pins the notification codex emits
// around every turn, including the active variant's flag payload.
func TestDispatch_ThreadStatusChanged(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("thread/status/changed", json.RawMessage(
		`{"threadId":"t1","status":{"type":"active","activeFlags":[]}}`))

	ev, ok := recvEvent(t, sub).(*ThreadStatusChangedEvent)
	if !ok {
		t.Fatal("want *ThreadStatusChangedEvent")
	}
	if ev.ThreadID != "t1" || ev.Status != "active" {
		t.Errorf("got %+v", ev)
	}
	if len(ev.StatusRaw) == 0 {
		t.Error("StatusRaw not preserved")
	}
}

// TestDispatch_TurnPlanUpdated covers the plan payload plus the optional
// explanation, which arrives as a JSON null on most updates.
func TestDispatch_TurnPlanUpdated(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("turn/plan/updated", json.RawMessage(
		`{"threadId":"t1","turnId":"u1","explanation":null,
		  "plan":[{"step":"read files","status":"completed"},
		          {"step":"write patch","status":"in_progress"}]}`))

	ev, ok := recvEvent(t, sub).(*TurnPlanUpdatedEvent)
	if !ok {
		t.Fatal("want *TurnPlanUpdatedEvent")
	}
	if len(ev.Plan) != 2 || ev.Plan[1].Status != "in_progress" {
		t.Errorf("plan = %+v", ev.Plan)
	}
	if ev.Explanation != "" {
		t.Errorf("Explanation = %q, want empty for null", ev.Explanation)
	}
}

// TestDispatch_FileChangePatchUpdated confirms the change entries decode
// far enough for Kind() to work, since that is the accessor consumers use.
func TestDispatch_FileChangePatchUpdated(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("item/fileChange/patchUpdated", json.RawMessage(
		`{"threadId":"t1","turnId":"u1","itemId":"i1",
		  "changes":[{"path":"a.go","diff":"+x","kind":{"type":"add"}}]}`))

	ev, ok := recvEvent(t, sub).(*FileChangePatchUpdatedEvent)
	if !ok {
		t.Fatal("want *FileChangePatchUpdatedEvent")
	}
	if len(ev.Changes) != 1 || ev.Changes[0].Kind() != "add" {
		t.Errorf("changes = %+v", ev.Changes)
	}
}

// TestDispatch_ReasoningSummaryPartAdded pins the index that pairs this
// event with the summary deltas that follow it.
func TestDispatch_ReasoningSummaryPartAdded(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("item/reasoning/summaryPartAdded", json.RawMessage(
		`{"threadId":"t1","turnId":"u1","itemId":"i1","summaryIndex":2}`))

	ev, ok := recvEvent(t, sub).(*ReasoningSummaryPartAddedEvent)
	if !ok {
		t.Fatal("want *ReasoningSummaryPartAddedEvent")
	}
	if ev.SummaryIndex != 2 || ev.ItemID != "i1" {
		t.Errorf("got %+v", ev)
	}
}

func TestDispatch_ThreadCompacted(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("thread/compacted", json.RawMessage(
		`{"threadId":"t1","turnId":"u1"}`))

	if _, ok := recvEvent(t, sub).(*ContextCompactedEvent); !ok {
		t.Fatal("want *ContextCompactedEvent")
	}
}

func TestDispatch_ModelRerouted(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("model/rerouted", json.RawMessage(
		`{"threadId":"t1","turnId":"u1","fromModel":"a","toModel":"b","reason":"capacity"}`))

	ev, ok := recvEvent(t, sub).(*ModelReroutedEvent)
	if !ok {
		t.Fatal("want *ModelReroutedEvent")
	}
	if ev.FromModel != "a" || ev.ToModel != "b" {
		t.Errorf("got %+v", ev)
	}
}

// TestDispatch_McpServerStatus checks the broadcast path: the payload's
// threadId is advisory, and the event must reach subscribers regardless.
func TestDispatch_McpServerStatus(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("mcpServer/startupStatus/updated", json.RawMessage(
		`{"threadId":null,"name":"codex_apps","status":"ready","error":null,"failureReason":null}`))

	ev, ok := recvEvent(t, sub).(*McpServerStatusEvent)
	if !ok {
		t.Fatal("want *McpServerStatusEvent")
	}
	if ev.Name != "codex_apps" || ev.Status != "ready" {
		t.Errorf("got %+v", ev)
	}
}

// TestDispatch_Warnings covers both warning methods and the
// thread-scoped vs broadcast delivery split between them.
func TestDispatch_Warnings(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")

	c.dispatchNotification("warning", json.RawMessage(`{"message":"disk low","threadId":null}`))
	ev, ok := recvEvent(t, sub).(*WarningEvent)
	if !ok {
		t.Fatal("want *WarningEvent")
	}
	if ev.Message != "disk low" || ev.Guardian {
		t.Errorf("got %+v", ev)
	}

	c.dispatchNotification("guardianWarning", json.RawMessage(`{"message":"risky","threadId":"t1"}`))
	ev, ok = recvEvent(t, sub).(*WarningEvent)
	if !ok {
		t.Fatal("want *WarningEvent")
	}
	if !ev.Guardian || ev.ThreadID != "t1" {
		t.Errorf("got %+v", ev)
	}
}

func TestDispatch_DeprecationNotice(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("deprecationNotice", json.RawMessage(
		`{"summary":"model/list models field","details":"use data"}`))

	ev, ok := recvEvent(t, sub).(*DeprecationNoticeEvent)
	if !ok {
		t.Fatal("want *DeprecationNoticeEvent")
	}
	if ev.Summary == "" || ev.Details != "use data" {
		t.Errorf("got %+v", ev)
	}
}

// TestDispatch_ItemCompletedTimestamp locks the completedAtMs field added
// in 0.14x — the only wall-clock signal for items with no durationMs.
func TestDispatch_ItemCompletedTimestamp(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("item/completed", json.RawMessage(
		`{"threadId":"t1","turnId":"u1","completedAtMs":1786433752224,
		  "item":{"id":"i1","type":"agentMessage","text":"hi"}}`))

	ev, ok := recvEvent(t, sub).(*ItemCompletedEvent)
	if !ok {
		t.Fatal("want *ItemCompletedEvent")
	}
	if ev.CompletedAtMs != 1786433752224 {
		t.Errorf("CompletedAtMs = %d", ev.CompletedAtMs)
	}
}

// TestDispatch_UnknownStillSurfaces guards the drift-detection escape
// hatch: adding typed cases above must not swallow unrecognized methods.
func TestDispatch_UnknownStillSurfaces(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification("thread/realtime/started", json.RawMessage(`{"threadId":"t1"}`))

	ev, ok := recvEvent(t, sub).(*UnknownEvent)
	if !ok {
		t.Fatal("want *UnknownEvent")
	}
	if ev.Method != "thread/realtime/started" {
		t.Errorf("Method = %q", ev.Method)
	}
}
