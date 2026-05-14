package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// TestApproval_CommandExecution_Accept exercises the typical accept path:
// the model proposes a shell command, the SDK consults the configured
// ApprovalFunc, sends back "accept", and observes the command stream
// proceed to item/completed.
func TestApproval_CommandExecution_Accept(t *testing.T) {
	fix := NewBidiFixtureExecutor()

	var observed atomic.Pointer[CommandExecutionApprovalRequest]
	var callCount atomic.Int32
	approver := func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
		callCount.Add(1)
		if r, ok := req.(*CommandExecutionApprovalRequest); ok {
			observed.Store(r)
		}
		return Accept{}, nil
	}

	client := NewWithExecutor(fix, WithEphemeralThread(), WithApprovalHandler(approver))
	go runApprovalServer(t, fix, &approvalScript{
		approvalMethod:   schema.MethodCommandExecutionRequestApproval,
		approvalParams:   commandApprovalParams("thr_appr", "turn_appr", "item_1", "ls /tmp"),
		expectedDecision: "accept",
	})

	stream, err := client.Run(context.Background(), "run ls /tmp")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	if _, err := drainTurn(stream, 3*time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if callCount.Load() != 1 {
		t.Errorf("approval handler invocations = %d, want 1", callCount.Load())
	}
	if got := observed.Load(); got == nil {
		t.Errorf("did not observe typed CommandExecutionApprovalRequest")
	} else if got.Params.Command == nil || *got.Params.Command != "ls /tmp" {
		t.Errorf("approval params not propagated: %+v", got.Params)
	}
}

// TestApproval_CommandExecution_Decline confirms the deny path: the
// SDK returns "decline" and the turn surfaces a clean denied item then
// completes without hanging.
func TestApproval_CommandExecution_Decline(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	approver := func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
		return Decline{}, nil
	}
	client := NewWithExecutor(fix, WithEphemeralThread(), WithApprovalHandler(approver))
	go runApprovalServer(t, fix, &approvalScript{
		approvalMethod:   schema.MethodCommandExecutionRequestApproval,
		approvalParams:   commandApprovalParams("thr_d", "turn_d", "item_1", "rm -rf /"),
		expectedDecision: "decline",
		declined:         true,
	})

	stream, err := client.Run(context.Background(), "remove the world")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	turn, err := drainTurn(stream, 3*time.Second)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if turn == nil || turn.Status != schema.TurnCompleted {
		t.Errorf("turn status = %v, want completed", turn)
	}
}

// TestApproval_FileChange checks the file-change approval shape uses the
// correct method name and decision dialect.
func TestApproval_FileChange(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	approver := func(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
		if _, ok := req.(*FileChangeApprovalRequest); !ok {
			t.Errorf("expected *FileChangeApprovalRequest, got %T", req)
		}
		return AcceptForSession{}, nil
	}
	client := NewWithExecutor(fix, WithEphemeralThread(), WithApprovalHandler(approver))
	go runApprovalServer(t, fix, &approvalScript{
		approvalMethod:   schema.MethodFileChangeRequestApproval,
		approvalParams:   fileChangeApprovalParams("thr_fc", "turn_fc", "item_1"),
		expectedDecision: "acceptForSession",
	})

	stream, err := client.Run(context.Background(), "make a change")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	if _, err := drainTurn(stream, 3*time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// TestApproval_DefaultDeniesAll verifies that without WithApprovalHandler
// the dispatcher auto-declines instead of stalling.
func TestApproval_DefaultDeniesAll(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())
	go runApprovalServer(t, fix, &approvalScript{
		approvalMethod:   schema.MethodCommandExecutionRequestApproval,
		approvalParams:   commandApprovalParams("thr_x", "turn_x", "item_1", "ls"),
		expectedDecision: "decline",
		declined:         true,
	})

	stream, err := client.Run(context.Background(), "run ls")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()
	if _, err := drainTurn(stream, 3*time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// TestApproval_HandlerError makes the callback return an error and
// confirms the dispatcher sends back a JSON-RPC error rather than a
// success body.
func TestApproval_HandlerError(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	approver := func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
		return nil, errors.New("policy not configured")
	}
	client := NewWithExecutor(fix, WithEphemeralThread(), WithApprovalHandler(approver))

	threadID := "thr_err"
	turnID := "turn_err"

	go func() {
		// boilerplate
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
		// approval request
		approvalID := json.RawMessage(`"appr_1"`)
		_ = fix.SendRequest(approvalID, schema.MethodCommandExecutionRequestApproval,
			commandApprovalParams(threadID, turnID, "item_1", "ls"))
		// expect an error response from the client
		got, err := fix.ReadFrame()
		if err != nil {
			t.Errorf("read approval response: %v", err)
			return
		}
		if string(got.ID) != string(approvalID) {
			t.Errorf("response id = %s, want %s", string(got.ID), string(approvalID))
		}
		if got.Error == nil {
			t.Errorf("expected error response, got result=%s", string(got.Result))
		} else if !strings.Contains(got.Error.Message, "policy not configured") {
			t.Errorf("error message = %q, want contains 'policy not configured'", got.Error.Message)
		}
		_ = fix.SendNotification("turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed", "items": []any{}},
		})
		drainStrayFrames(fix)
	}()

	stream, err := client.Run(context.Background(), "run ls")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()
	if _, err := drainTurn(stream, 3*time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// TestApprovalRequestEvent_Surfaces verifies that approval requests are
// also broadcast as events so UI consumers can render pending state in
// parallel with the callback.
func TestApprovalRequestEvent_Surfaces(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	var wg sync.WaitGroup
	wg.Add(1)
	approver := func(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
		// Block briefly so the event has time to arrive on the stream
		// before the response goes back; ensures we test the broadcast
		// path, not just an accidental ordering.
		time.Sleep(50 * time.Millisecond)
		return Accept{}, nil
	}
	client := NewWithExecutor(fix, WithEphemeralThread(), WithApprovalHandler(approver))
	go func() {
		defer wg.Done()
		// thread id must match what runApprovalServer responds with
		// ("thr_appr"); otherwise the approval event delivers to a
		// non-existent subscription.
		runApprovalServer(t, fix, &approvalScript{
			approvalMethod:   schema.MethodCommandExecutionRequestApproval,
			approvalParams:   commandApprovalParams("thr_appr", "turn_appr", "item_1", "ls"),
			expectedDecision: "accept",
		})
	}()

	stream, err := client.Run(context.Background(), "run ls")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()
	saw := false
	seen := []string{}
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				break loop
			}
			seen = append(seen, fmtEventType(ev))
			if e, ok := ev.(*ApprovalRequestEvent); ok {
				saw = true
				if e.Request.Method() != schema.MethodCommandExecutionRequestApproval {
					t.Errorf("approval event method = %q", e.Request.Method())
				}
			}
		case <-deadline:
			t.Fatal("timeout waiting for events")
		}
	}
	if !saw {
		t.Errorf("no ApprovalRequestEvent observed; events seen: %v", seen)
	}
	wg.Wait()
}

// approvalScript captures the parameters runApprovalServer needs to
// reproduce a single-approval turn.
type approvalScript struct {
	approvalMethod   string
	approvalParams   any
	expectedDecision string
	declined         bool // when true, the served command emits a "declined" item/completed
}

// runApprovalServer scripts a turn with exactly one approval round-trip.
func runApprovalServer(t *testing.T, fix *BidiFixtureExecutor, s *approvalScript) {
	t.Helper()
	id, _ := expectRequest(t, fix, "initialize")
	_ = fix.SendResponse(id, basicInitResponse())
	expectNotification(t, fix, "initialized")
	threadID := "thr_appr"
	turnID := "turn_appr"
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
		"item": map[string]any{"id": "item_1", "type": "commandExecution"},
	})

	approvalID := json.RawMessage(`"appr_1"`)
	_ = fix.SendRequest(approvalID, s.approvalMethod, s.approvalParams)

	// expect the typed decision back
	resp, err := fix.ReadFrame()
	if err != nil {
		t.Errorf("read approval response: %v", err)
		return
	}
	if string(resp.ID) != string(approvalID) {
		t.Errorf("approval response id = %s, want %s", string(resp.ID), string(approvalID))
	}
	var decisionBody struct {
		Decision json.RawMessage `json:"decision"`
	}
	if resp.Error != nil {
		t.Errorf("expected success response, got error %q", resp.Error.Message)
	}
	_ = json.Unmarshal(resp.Result, &decisionBody)
	decision := strings.Trim(string(decisionBody.Decision), `"`)
	if decision != s.expectedDecision {
		t.Errorf("decision = %q, want %q", decision, s.expectedDecision)
	}

	// emit the resulting item/completed and turn/completed
	itemStatus := "completed"
	if s.declined {
		itemStatus = "declined"
	}
	_ = fix.SendNotification("item/completed", map[string]any{
		"threadId": threadID, "turnId": turnID,
		"item": map[string]any{"id": "item_1", "type": "commandExecution", "status": itemStatus},
	})
	_ = fix.SendNotification("turn/completed", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": "completed", "items": []any{}},
	})
	drainStrayFrames(fix)
}

func commandApprovalParams(threadID, turnID, itemID, command string) map[string]any {
	return map[string]any{
		"threadId":    threadID,
		"turnId":      turnID,
		"itemId":      itemID,
		"command":     command,
		"startedAtMs": 1,
	}
}

func fileChangeApprovalParams(threadID, turnID, itemID string) map[string]any {
	return map[string]any{
		"threadId":    threadID,
		"turnId":      turnID,
		"itemId":      itemID,
		"startedAtMs": 1,
	}
}

func basicInitResponse() map[string]any {
	return map[string]any{
		"codexHome":      "/tmp/codex-home",
		"platformFamily": "unix",
		"platformOs":     "linux",
		"userAgent":      "codex-cli/0.130.0",
	}
}

func basicThreadStartResponse(threadID string) map[string]any {
	return map[string]any{
		"thread": map[string]any{
			"id": threadID, "sessionId": "sess_1", "cwd": "/tmp",
			"cliVersion": "0.130.0", "modelProvider": "openai",
			"ephemeral": true, "createdAt": 1, "updatedAt": 1, "preview": "",
		},
		"cwd": "/tmp", "model": "gpt-5", "modelProvider": "openai",
		"approvalPolicy": "untrusted",
		"sandbox":        map[string]any{"mode": "read-only"},
	}
}

// drainTurn reads stream events until it sees TurnCompletedEvent or
// hits the timeout. Returns the final turn or an error.
func drainTurn(s *Stream, timeout time.Duration) (*schema.Turn, error) {
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				turn, err := s.Wait()
				return turn, err
			}
			if e, ok := ev.(*TurnCompletedEvent); ok {
				return &e.Turn, nil
			}
		case <-deadline:
			return nil, errors.New("timeout draining stream")
		}
	}
}

// drainStrayFrames consumes any frames the client sends after the
// scripted exchange ends (e.g. context-cancel-induced notifications).
func drainStrayFrames(fix *BidiFixtureExecutor) {
	go func() {
		for {
			if _, err := fix.ReadFrame(); err != nil {
				return
			}
		}
	}()
}

func fmtEventType(ev Event) string {
	switch e := ev.(type) {
	case *StartEvent:
		return "Start"
	case *TurnStartedEvent:
		return "TurnStarted"
	case *ItemStartedEvent:
		return "ItemStarted"
	case *AgentMessageDeltaEvent:
		return "AgentMessageDelta"
	case *ItemCompletedEvent:
		return "ItemCompleted"
	case *TurnCompletedEvent:
		return "TurnCompleted"
	case *ErrorEvent:
		return "Error"
	case *ApprovalRequestEvent:
		return "ApprovalRequest:" + e.Request.Method()
	case *ProcessExitEvent:
		return "ProcessExit"
	case *UnknownEvent:
		return "Unknown:" + e.Method
	case *UnknownServerRequestEvent:
		return "UnknownServerRequest:" + e.Method
	default:
		return "?"
	}
}
