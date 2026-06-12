package codexcli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// skillsServer runs the scripted server side of a skills RPC test: it
// completes the initialize handshake, then dispatches client requests to
// the provided handlers keyed by method until the connection closes.
func skillsServer(t *testing.T, fix *BidiFixtureExecutor, handlers map[string]func(id, params json.RawMessage)) {
	t.Helper()
	id, _ := expectRequest(t, fix, "initialize")
	_ = fix.SendResponse(id, map[string]any{
		"codexHome": "/tmp", "platformFamily": "unix", "platformOs": "linux", "userAgent": "test",
	})
	expectNotification(t, fix, "initialized")

	for {
		f, err := fix.ReadFrame()
		if err != nil {
			return
		}
		if !f.IsRequest() {
			continue
		}
		if h, ok := handlers[f.Method]; ok {
			h(f.ID, f.Params)
		}
	}
}

func TestConnListSkills_HappyPath(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())

	var gotParams schema.SkillsListParams
	go skillsServer(t, fix, map[string]func(id, params json.RawMessage){
		schema.MethodSkillsList: func(id, params json.RawMessage) {
			_ = json.Unmarshal(params, &gotParams)
			_ = fix.SendResponse(id, map[string]any{
				"data": []any{
					map[string]any{
						"cwd": "/repo",
						"skills": []any{
							map[string]any{
								"name": "review", "description": "Review a PR", "enabled": true,
								"path": "/repo/.codex/skills/review", "scope": "repo",
								"interface": map[string]any{"displayName": "Code Review", "defaultPrompt": "Review this"},
								"dependencies": map[string]any{
									"tools": []any{map[string]any{"type": "command", "value": "git"}},
								},
							},
							map[string]any{
								"name": "deploy", "description": "Deploy", "enabled": false,
								"path": "/repo/.codex/skills/deploy", "scope": "user",
							},
						},
						"errors": []any{
							map[string]any{"path": "/repo/.codex/skills/broken", "message": "missing SKILL.md"},
						},
					},
				},
			})
		},
	})

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entries, err := conn.ListSkills(ctx, []string{"/repo"}, true)
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}

	// Request params reached the server intact.
	if len(gotParams.Cwds) != 1 || gotParams.Cwds[0] != "/repo" {
		t.Errorf("Cwds = %v, want [/repo]", gotParams.Cwds)
	}
	if !gotParams.ForceReload {
		t.Errorf("ForceReload = false, want true")
	}

	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Cwd != "/repo" {
		t.Errorf("Cwd = %q", e.Cwd)
	}
	if len(e.Skills) != 2 {
		t.Fatalf("skills len = %d, want 2", len(e.Skills))
	}
	review := e.Skills[0]
	if review.Name != "review" || review.Scope != schema.SkillScopeRepo || !review.Enabled {
		t.Errorf("review = %+v", review)
	}
	if review.Interface == nil || review.Interface.DisplayName == nil || *review.Interface.DisplayName != "Code Review" {
		t.Errorf("review.Interface = %+v", review.Interface)
	}
	if review.Dependencies == nil || len(review.Dependencies.Tools) != 1 || review.Dependencies.Tools[0].Value != "git" {
		t.Errorf("review.Dependencies = %+v", review.Dependencies)
	}
	if e.Skills[1].Enabled {
		t.Errorf("deploy should be disabled")
	}
	if len(e.Errors) != 1 || e.Errors[0].Message != "missing SKILL.md" {
		t.Errorf("Errors = %+v", e.Errors)
	}

	// SkillInput convenience pulls name+path off the metadata.
	in := SkillInput(review)
	if in.Type != "skill" || in.Name != "review" || in.Path != "/repo/.codex/skills/review" {
		t.Errorf("SkillInput = %+v", in)
	}
}

// TestExperimentalApiAndGranularApproval_ReachWire verifies that
// WithExperimentalAPI surfaces as capabilities.experimentalApi on the
// initialize frame, and that a granular approval policy with skill_approval
// set reaches thread/start intact — the two things a live codex requires to
// drive skill approval.
func TestExperimentalApiAndGranularApproval_ReachWire(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix,
		WithEphemeralThread(),
		WithCwd("/tmp"),
		WithExperimentalAPI(),
		WithApprovalPolicy(schema.NewGranularApproval(schema.GranularApproval{
			McpElicitations: false, Rules: false, SandboxApproval: false, SkillApproval: true,
		})),
	)

	type caps struct {
		ExperimentalApi bool `json:"experimentalApi"`
	}
	gotCaps := make(chan caps, 1)
	gotApproval := make(chan schema.AskForApproval, 1)

	go func() {
		id, params := expectRequest(t, fix, "initialize")
		var ip struct {
			Capabilities caps `json:"capabilities"`
		}
		_ = json.Unmarshal(params, &ip)
		gotCaps <- ip.Capabilities
		_ = fix.SendResponse(id, map[string]any{
			"codexHome": "/tmp", "platformFamily": "unix", "platformOs": "linux", "userAgent": "test",
		})
		expectNotification(t, fix, "initialized")

		id, params = expectRequest(t, fix, "thread/start")
		var tp struct {
			ApprovalPolicy schema.AskForApproval `json:"approvalPolicy"`
		}
		_ = json.Unmarshal(params, &tp)
		gotApproval <- tp.ApprovalPolicy
		_ = fix.SendResponse(id, map[string]any{
			"thread":         map[string]any{"id": "t", "sessionId": "s", "cwd": "/tmp", "cliVersion": "x", "modelProvider": "openai", "ephemeral": true, "createdAt": 1, "updatedAt": 1, "preview": ""},
			"cwd":            "/tmp",
			"model":          "gpt-5",
			"modelProvider":  "openai",
			"approvalPolicy": "never",
			"sandbox":        map[string]any{"mode": "read-only"},
		})
		go func() {
			for {
				if _, err := fix.ReadFrame(); err != nil {
					return
				}
			}
		}()
	}()

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	if _, err := conn.NewThread(context.Background()); err != nil {
		t.Fatalf("NewThread: %v", err)
	}

	select {
	case c := <-gotCaps:
		if !c.ExperimentalApi {
			t.Error("initialize did not carry capabilities.experimentalApi=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no initialize frame")
	}

	select {
	case ap := <-gotApproval:
		g, ok := ap.Granular()
		if !ok {
			t.Fatalf("thread/start approvalPolicy not granular: %s", string(ap.Raw))
		}
		if !g.SkillApproval {
			t.Errorf("skill_approval = false, want true (raw: %s)", string(ap.Raw))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no thread/start frame")
	}
}

// TestSkillsChangedEvent_Dispatch drives a turn during which the server
// emits a skills/changed notification, and asserts the SDK surfaces it as
// a typed *SkillsChangedEvent on the stream.
func TestSkillsChangedEvent_Dispatch(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread(), WithCwd("/tmp"))

	go func() {
		id, _ := expectRequest(t, fix, "initialize")
		_ = fix.SendResponse(id, map[string]any{
			"codexHome": "/tmp", "platformFamily": "unix", "platformOs": "linux", "userAgent": "test",
		})
		expectNotification(t, fix, "initialized")

		threadID := "thr_skills"
		id, _ = expectRequest(t, fix, "thread/start")
		_ = fix.SendResponse(id, map[string]any{
			"thread":         map[string]any{"id": threadID, "sessionId": "s", "cwd": "/tmp", "cliVersion": "x", "modelProvider": "openai", "ephemeral": true, "createdAt": 1, "updatedAt": 1, "preview": ""},
			"cwd":            "/tmp",
			"model":          "gpt-5",
			"modelProvider":  "openai",
			"approvalPolicy": "never",
			"sandbox":        map[string]any{"mode": "read-only"},
		})

		turnID := "turn_skills"
		id, _ = expectRequest(t, fix, "turn/start")
		_ = fix.SendResponse(id, map[string]any{
			"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
		})
		_ = fix.SendNotification("turn/started", map[string]any{
			"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
		})
		// Empty-payload skills/changed mid-turn.
		_ = fix.SendNotification(schema.MethodSkillsChanged, map[string]any{})
		_ = fix.SendNotification("turn/completed", map[string]any{
			"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "completed", "items": []any{}},
		})
		go func() {
			for {
				if _, err := fix.ReadFrame(); err != nil {
					return
				}
			}
		}()
	}()

	stream, err := client.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	var sawChanged bool
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				break loop
			}
			switch ev.(type) {
			case *SkillsChangedEvent:
				sawChanged = true
			case *TurnCompletedEvent:
				break loop
			}
		case <-deadline:
			t.Fatal("timeout waiting for events")
		}
	}
	if !sawChanged {
		t.Error("missing SkillsChangedEvent")
	}
}

func TestConnSetSkillEnabled_ByNameAndPath(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())

	var writes []schema.SkillsConfigWriteParams
	go skillsServer(t, fix, map[string]func(id, params json.RawMessage){
		schema.MethodSkillsConfigWrite: func(id, params json.RawMessage) {
			var p schema.SkillsConfigWriteParams
			_ = json.Unmarshal(params, &p)
			writes = append(writes, p)
			// Echo the requested state back as effective.
			_ = fix.SendResponse(id, map[string]any{"effectiveEnabled": p.Enabled})
		},
	})

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	eff, err := conn.SetSkillEnabledByName(ctx, "review", false)
	if err != nil {
		t.Fatalf("SetSkillEnabledByName: %v", err)
	}
	if eff {
		t.Errorf("effective enabled = true, want false")
	}

	eff, err = conn.SetSkillEnabledByPath(ctx, "/repo/.codex/skills/deploy", true)
	if err != nil {
		t.Fatalf("SetSkillEnabledByPath: %v", err)
	}
	if !eff {
		t.Errorf("effective enabled = false, want true")
	}

	if len(writes) != 2 {
		t.Fatalf("writes len = %d, want 2", len(writes))
	}
	// First write selects by name, not path.
	if writes[0].Name == nil || *writes[0].Name != "review" || writes[0].Path != nil {
		t.Errorf("write[0] = %+v", writes[0])
	}
	if writes[0].Enabled {
		t.Errorf("write[0].Enabled = true, want false")
	}
	// Second write selects by path, not name.
	if writes[1].Path == nil || *writes[1].Path != "/repo/.codex/skills/deploy" || writes[1].Name != nil {
		t.Errorf("write[1] = %+v", writes[1])
	}
	if !writes[1].Enabled {
		t.Errorf("write[1].Enabled = false, want true")
	}
}
