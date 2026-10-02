package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// Wire shapes below are from live captures on codex 0.159.3: a child's
// first frame (thread/status/changed idle) can precede the parent's
// subAgentActivity that names it, and children never send thread/started.

func subAgentItem(kind, childID, path string) map[string]any {
	return map[string]any{"type": "subAgentActivity", "id": "call_spawn", "kind": kind, "agentThreadId": childID, "agentPath": path}
}

func turnStarted(fix *BidiFixtureExecutor, threadID, turnID string) {
	_ = fix.SendNotification("turn/started", map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}})
}

func turnCompleted(fix *BidiFixtureExecutor, threadID, turnID, status string) {
	_ = fix.SendNotification("turn/completed", map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": status, "items": []any{}}})
}

// startParentTurn runs serveThreadStart, answers turn/start for thr_1 with
// turn_p, and returns once the client has the parent turn active.
func startParentTurn(t *testing.T, fix *BidiFixtureExecutor) (*Conn, *Thread, *Stream) {
	t.Helper()
	ready := make(chan struct{})
	go func() {
		serveThreadStart(t, fix, "thr_1")
		id, _ := expectRequest(t, fix, "turn/start")
		_ = fix.SendResponse(id, map[string]any{"turn": map[string]any{"id": "turn_p", "status": "inProgress", "items": []any{}}})
		turnStarted(fix, "thr_1", "turn_p")
		close(ready)
	}()
	conn, th := connectFake(t, fix)
	stream, err := th.StartTurn(context.Background(), "spawn a helper")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	<-ready
	nextEvent(t, stream, func(ev Event) bool { _, ok := ev.(*TurnStartedEvent); return ok })
	return conn, th, stream
}

func nextEvent(t *testing.T, s *Stream, match func(Event) bool) Event {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("stream closed")
			}
			if match(ev) {
				return ev
			}
		case <-timeout:
			t.Fatal("timed out waiting for event")
		}
	}
}

func childEvent(ev Event) (*ChildThreadEvent, bool) {
	ce, ok := ev.(*ChildThreadEvent)
	return ce, ok
}

// TestSubagent_ChildEventsReachParentStream: child frames, including the one
// that arrives before the child is named, reach the parent's Stream wrapped
// in ChildThreadEvent with the child's identity, in order.
func TestSubagent_ChildEventsReachParentStream(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	conn, th, stream := startParentTurn(t, fix)

	_ = fix.SendNotification("thread/status/changed", map[string]any{"threadId": "thr_c", "status": map[string]any{"type": "idle"}})
	_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_p", "item": subAgentItem("started", "thr_c", "/root/helper")})
	turnStarted(fix, "thr_c", "turn_c")
	_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_c", "turnId": "turn_c", "item": map[string]any{"type": "agentMessage", "id": "m1", "text": "child says hi"}})

	var got []string
	for len(got) < 3 {
		ce, _ := childEvent(nextEvent(t, stream, func(ev Event) bool { _, ok := childEvent(ev); return ok }))
		if ce.Child.ThreadID != "thr_c" || ce.Child.ParentThreadID != "thr_1" || ce.Child.AgentPath != "/root/helper" || ce.Child.ToolCallID != "call_spawn" || ce.Child.SpawnTurnID != "turn_p" {
			t.Fatalf("child identity = %+v", ce.Child)
		}
		got = append(got, eventName(ce.Event))
	}
	want := []string{"*codexcli.ThreadStatusChangedEvent", "*codexcli.TurnStartedEvent", "*codexcli.ItemCompletedEvent"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("child events = %v, want %v", got, want)
		}
	}
	kids := th.Children()
	if len(kids) != 1 || kids[0].ThreadID != "thr_c" || kids[0].ActiveTurnID != "turn_c" {
		t.Errorf("Children() = %+v", kids)
	}
	if c, ok := conn.ChildThread("thr_c"); !ok || c.ParentThreadID != "thr_1" {
		t.Errorf("ChildThread = %+v %v", c, ok)
	}
}

// TestSubagent_GrandchildRoutesToRoot: a child's own subagent is attributed
// to the child and delivered on the root thread's Stream.
func TestSubagent_GrandchildRoutesToRoot(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	_, th, stream := startParentTurn(t, fix)
	_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_p", "item": subAgentItem("started", "thr_c", "/root/a")})
	turnStarted(fix, "thr_c", "turn_c")
	_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_c", "turnId": "turn_c", "item": subAgentItem("started", "thr_g", "/root/a/b")})
	turnStarted(fix, "thr_g", "turn_g")
	ev := nextEvent(t, stream, func(ev Event) bool {
		ce, ok := childEvent(ev)
		return ok && ce.Child.ThreadID == "thr_g"
	})
	ce, _ := childEvent(ev)
	if ce.Child.ParentThreadID != "thr_c" || ce.Child.AgentPath != "/root/a/b" {
		t.Errorf("grandchild = %+v", ce.Child)
	}
	if n := len(th.Children()); n != 2 {
		t.Errorf("Children() has %d threads, want 2 (all descendants)", n)
	}
}

// TestSubagent_InterruptCascades: codex 0.159.3 leaves a subagent running
// when its parent's turn is interrupted (observed live twice), so
// Thread.Interrupt interrupts every descendant's active turn, then its own.
func TestSubagent_InterruptCascades(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	_, th, stream := startParentTurn(t, fix)
	_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_p", "item": subAgentItem("started", "thr_c", "/root/a")})
	turnStarted(fix, "thr_c", "turn_c")
	nextEvent(t, stream, func(ev Event) bool {
		ce, ok := childEvent(ev)
		return ok && eventName(ce.Event) == "*codexcli.TurnStartedEvent"
	})

	got := make(chan schema.TurnInterruptParams, 4)
	go func() {
		for range 2 {
			id, raw := expectRequest(t, fix, "turn/interrupt")
			var p schema.TurnInterruptParams
			_ = json.Unmarshal(raw, &p)
			got <- p
			_ = fix.SendResponse(id, map[string]any{})
		}
		drainStrayFrames(fix)
	}()
	if err := th.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	first, second := <-got, <-got
	if first.ThreadID != "thr_c" || first.TurnID != "turn_c" || second.ThreadID != "thr_1" || second.TurnID != "turn_p" {
		t.Errorf("interrupts = %+v then %+v, want the child's turn then the parent's", first, second)
	}
}

// TestSubagent_InterruptCascadeBounded: a child that never answers its
// interrupt does not block the parent's.
func TestSubagent_InterruptCascadeBounded(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	_, th, stream := startParentTurn(t, fix)
	_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_p", "item": subAgentItem("started", "thr_c", "/root/a")})
	turnStarted(fix, "thr_c", "turn_c")
	nextEvent(t, stream, func(ev Event) bool {
		ce, ok := childEvent(ev)
		return ok && eventName(ce.Event) == "*codexcli.TurnStartedEvent"
	})
	parent := make(chan struct{})
	go func() {
		for {
			f, err := fix.ReadFrame()
			if err != nil {
				return
			}
			if f.Method == "turn/interrupt" && strings.Contains(string(f.Params), "thr_1") {
				_ = fix.SendResponse(f.ID, map[string]any{})
				close(parent)
			}
		}
	}()
	start := time.Now()
	if err := th.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	<-parent
	if d := time.Since(start); d > childInterruptTimeout+2*time.Second {
		t.Errorf("Interrupt took %s with a silent child", d)
	}
}

// TestInterrupt_NoActiveTurnSendsNothing: codex 0.159.3 rejects
// turn/interrupt without a turnId ("missing field `turnId`"), so with no
// active turn Interrupt is a local no-op.
func TestInterrupt_NoActiveTurnSendsNothing(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	go serveThreadStart(t, fix, "thr_1")
	_, th := connectFake(t, fix)
	frames := readFrames(fix)
	if err := th.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	requireNoFrame(t, frames, "for Interrupt with no active turn")
}

// TestSubagent_ChildApprovalOnParentStream: an approval a child asks for
// reaches the parent's Stream as a ChildThreadEvent, so a host can draw it
// under the subagent that asked.
func TestSubagent_ChildApprovalOnParentStream(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	_, _, stream := startParentTurn(t, fix)
	_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_p", "item": subAgentItem("started", "thr_c", "/root/a")})
	turnStarted(fix, "thr_c", "turn_c")
	go func() {
		_ = fix.SendRequest(json.RawMessage(`9`), schema.MethodCommandExecutionRequestApproval, commandApprovalParams("thr_c", "turn_c", "item_x", "rm x"))
		drainStrayFrames(fix)
	}()
	ev := nextEvent(t, stream, func(ev Event) bool {
		ce, ok := childEvent(ev)
		return ok && eventName(ce.Event) == "*codexcli.ApprovalRequestEvent"
	})
	if ce, _ := childEvent(ev); ce.Child.ThreadID != "thr_c" {
		t.Errorf("approval attributed to %+v", ce.Child)
	}
}

// TestSetName: thread/name/set carries the name; codex's answers for an
// unknown thread and an empty name are typed.
func TestSetName(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	sent := make(chan json.RawMessage, 3)
	go func() {
		serveThreadStart(t, fix, "thr_1")
		id, raw := expectRequest(t, fix, schema.MethodThreadNameSet)
		sent <- raw
		_ = fix.SendResponse(id, map[string]any{})
		id, _ = expectRequest(t, fix, schema.MethodThreadNameSet)
		_ = fix.SendErrorResponse(id, rpcCodeInvalidRequest, "no rollout found for thread id thr_1")
		id, _ = expectRequest(t, fix, schema.MethodThreadNameSet)
		_ = fix.SendErrorResponse(id, rpcCodeInvalidRequest, "ephemeral thread does not support metadata updates: thr_1")
		drainStrayFrames(fix)
	}()
	_, th := connectFake(t, fix)
	if err := th.SetName(context.Background(), "Refactor parser"); err != nil {
		t.Fatalf("SetName: %v", err)
	}
	var p schema.ThreadSetNameParams
	_ = json.Unmarshal(<-sent, &p)
	if p.ThreadID != "thr_1" || p.Name != "Refactor parser" {
		t.Errorf("params = %+v", p)
	}
	if err := th.SetName(context.Background(), "x"); !errors.Is(err, ErrThreadNotFound) {
		t.Errorf("SetName on a missing thread = %v, want ErrThreadNotFound", err)
	}
	if err := th.SetName(context.Background(), "x"); !errors.Is(err, ErrThreadEphemeral) {
		t.Errorf("SetName on an ephemeral thread = %v, want ErrThreadEphemeral", err)
	}
	if err := th.SetName(context.Background(), "  "); err == nil {
		t.Error("SetName accepted a blank name")
	}
}

// TestDispatch_ThreadNameUpdated: codex announces every rename.
func TestDispatch_ThreadNameUpdated(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification(schema.MethodThreadNameUpdated, json.RawMessage(`{"threadId":"t1","threadName":"Probe name two"}`))
	ev, ok := recvEvent(t, sub).(*ThreadNameUpdatedEvent)
	if !ok || ev.ThreadID != "t1" || ev.Name != "Probe name two" {
		t.Fatalf("got %#v", ev)
	}
}

// TestDeleteThread: thread/delete carries the thread id and drops the
// thread and its subagents, which codex deletes with it, from the
// connection; an unknown thread is ErrThreadNotFound.
func TestDeleteThread(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	sent := make(chan json.RawMessage, 1)
	go func() {
		serveThreadStart(t, fix, "thr_1")
		id, raw := expectRequest(t, fix, schema.MethodThreadDelete)
		sent <- raw
		_ = fix.SendResponse(id, map[string]any{})
		id, _ = expectRequest(t, fix, schema.MethodThreadDelete)
		_ = fix.SendErrorResponse(id, rpcCodeInvalidRequest, "no rollout found for thread id thr_1")
		drainStrayFrames(fix)
	}()
	conn, th := connectFake(t, fix)
	r := &conn.childReg
	r.mu.Lock()
	r.init()
	r.add(&ChildThread{ThreadID: "thr_c", ParentThreadID: "thr_1"})
	r.add(&ChildThread{ThreadID: "thr_gc", ParentThreadID: "thr_c"})
	r.add(&ChildThread{ThreadID: "thr_other", ParentThreadID: "thr_2"})
	r.mu.Unlock()

	if err := conn.DeleteThread(context.Background(), th.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	var p schema.ThreadDeleteParams
	_ = json.Unmarshal(<-sent, &p)
	if p.ThreadID != "thr_1" {
		t.Errorf("params = %+v", p)
	}
	if conn.lookupThread("thr_1") != nil {
		t.Error("deleted thread still registered")
	}
	for _, id := range []string{"thr_c", "thr_gc"} {
		if _, ok := conn.ChildThread(id); ok {
			t.Errorf("subagent %s of the deleted thread still reported", id)
		}
	}
	if _, ok := conn.ChildThread("thr_other"); !ok {
		t.Error("another thread's subagent was forgotten")
	}
	if err := conn.DeleteThread(context.Background(), th.ID); !errors.Is(err, ErrThreadNotFound) {
		t.Errorf("DeleteThread on a missing thread = %v, want ErrThreadNotFound", err)
	}
}

// TestDispatch_ThreadDeleted: codex announces a delete.
func TestDispatch_ThreadDeleted(t *testing.T) {
	c, sub := newDispatchConn(t, "t1")
	c.dispatchNotification(schema.MethodThreadDeleted, json.RawMessage(`{"threadId":"t1"}`))
	ev, ok := recvEvent(t, sub).(*ThreadDeletedEvent)
	if !ok || ev.ThreadID != "t1" {
		t.Fatalf("got %#v", ev)
	}
}

// TestUserInput_RoundTrip decodes the request codex 0.159.3 sent in Plan
// mode and encodes the answer shapes it accepts.
func TestUserInput_RoundTrip(t *testing.T) {
	req := ServerRequest{Method: schema.MethodToolRequestUserInput, Params: json.RawMessage(`{"threadId":"t","turnId":"u","itemId":"call_1","questions":[{"id":"color","header":"Color","question":"Which color do you prefer, red or blue?","isOther":true,"isSecret":false,"options":[{"label":"Red (Recommended)","description":"Choose red."},{"label":"Blue","description":"Choose blue."}]}],"isBlocking":true,"autoResolutionMs":null}`)}
	p, ok, err := req.UserInput()
	if err != nil || !ok {
		t.Fatalf("UserInput: %v %v", ok, err)
	}
	q := p.Questions[0]
	if q.ID != "color" || !q.IsOther || q.IsSecret || len(q.Options) != 2 || q.Options[1].Label != "Blue" || !p.IsBlocking || p.ItemID != "call_1" {
		t.Fatalf("decoded %+v", p)
	}
	if _, ok, _ := (ServerRequest{Method: "other"}).UserInput(); ok {
		t.Error("UserInput accepted another method")
	}
	body, _ := json.Marshal(schema.UserInputAnswers(map[string][]string{"color": {"Blue"}}))
	if string(body) != `{"answers":{"color":{"answers":["Blue"]}}}` {
		t.Errorf("answers = %s", body)
	}
	body, _ = json.Marshal(schema.UserInputDismissed())
	if string(body) != `{"answers":{}}` {
		t.Errorf("dismissed = %s", body)
	}
}

func eventName(ev Event) string {
	if ev == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", ev)
}

// TestSubagent_ReplayKeepsEventTimeTurn: frames held for an unnamed child
// replay with the turn that was active when each arrived, even if the
// child's turn ended before it was named.
func TestSubagent_ReplayKeepsEventTimeTurn(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	_, _, stream := startParentTurn(t, fix)
	turnStarted(fix, "thr_c", "turn_c")
	turnCompleted(fix, "thr_c", "turn_c", "completed")
	_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_p", "item": subAgentItem("started", "thr_c", "/root/a")})
	ev := nextEvent(t, stream, func(ev Event) bool { _, ok := childEvent(ev); return ok })
	ce, _ := childEvent(ev)
	if _, ok := ce.Event.(*TurnStartedEvent); !ok || ce.Child.ActiveTurnID != "turn_c" {
		t.Fatalf("first replayed = %T with active turn %q, want TurnStartedEvent with turn_c", ce.Event, ce.Child.ActiveTurnID)
	}
}

// TestSubagent_ResumedChildIsARoot: a former child resumed with
// ResumeThread owns its own subagents' events.
func TestSubagent_ResumedChildIsARoot(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	conn, _, _ := startParentTurn(t, fix)
	_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_p", "item": subAgentItem("started", "thr_c", "/root/a")})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := conn.ChildThread("thr_c"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never registered")
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		id, _ := expectRequest(t, fix, "thread/resume")
		_ = fix.SendResponse(id, basicThreadStartResponse("thr_c"))
		id, _ = expectRequest(t, fix, "turn/start")
		_ = fix.SendResponse(id, map[string]any{"turn": map[string]any{"id": "turn_c2", "status": "inProgress", "items": []any{}}})
		turnStarted(fix, "thr_c", "turn_c2")
		_ = fix.SendNotification("item/completed", map[string]any{"threadId": "thr_c", "turnId": "turn_c2", "item": subAgentItem("started", "thr_g", "/root/a/g")})
		turnStarted(fix, "thr_g", "turn_g")
		drainStrayFrames(fix)
	}()
	resumed, err := conn.ResumeThread(context.Background(), "thr_c")
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	if _, ok := conn.ChildThread("thr_c"); ok {
		t.Error("resumed thread still listed as a child")
	}
	stream, err := resumed.StartTurn(context.Background(), "go on")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ev := nextEvent(t, stream, func(ev Event) bool { _, ok := childEvent(ev); return ok })
	if ce, _ := childEvent(ev); ce.Child.ThreadID != "thr_g" || ce.Child.ParentThreadID != "thr_c" {
		t.Errorf("grandchild event = %+v", ce.Child)
	}
}

// TestInterrupt_BoundedWhenStdinBlocked: codex stops reading stdin. The
// interrupt write blocks, and Interrupt still returns at ctx's deadline.
func TestInterrupt_BoundedWhenStdinBlocked(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	_, th, _ := startParentTurn(t, fix) // the server reads nothing after turn/start
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := th.Interrupt(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Interrupt took %s with a 200ms ctx", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Interrupt = %v, want DeadlineExceeded", err)
	}
}

// TestChildRegistry_Bounded: idle children past childLimit are forgotten,
// and an evicted unnamed thread takes its turn record with it.
func TestChildRegistry_Bounded(t *testing.T) {
	var r childRegistry
	r.init()
	for i := range childLimit + 50 {
		r.add(&ChildThread{ThreadID: fmt.Sprint("c", i), ParentThreadID: "p"})
	}
	if len(r.children) > childLimit || len(r.childOrder) != len(r.children) {
		t.Errorf("children = %d (order %d), limit %d", len(r.children), len(r.childOrder), childLimit)
	}
	c := &Conn{logger: nil}
	for i := range orphanThreadLimit + 10 {
		id := fmt.Sprint("u", i)
		c.childReg.init()
		c.childReg.turns[id] = "turn"
		c.subsMu.Lock()
		c.routeUnsubscribed(id, &TurnStartedEvent{ThreadID: id})
		c.subsMu.Unlock()
	}
	if n := len(c.childReg.turns); n > orphanThreadLimit {
		t.Errorf("turns holds %d threads after evictions, want at most %d", n, orphanThreadLimit)
	}
}
