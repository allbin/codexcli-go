package schema

import (
	"encoding/json"
	"testing"
)

// TestUserMessageContent_ParsesSkillAndText decodes a real userMessage
// item payload (captured from a live codex turn that attached a skill
// input) and asserts the content blocks surface as the UserInput union.
func TestUserMessageContent_ParsesSkillAndText(t *testing.T) {
	// Verbatim shape from a live item/completed notification.
	raw := []byte(`{
		"id": "39aace7f",
		"type": "userMessage",
		"content": [
			{"type": "skill", "name": "probe-skill", "path": "/repo/.codex/skills/probe-skill/SKILL.md"},
			{"type": "text", "text": "Invoke the probe-skill.", "text_elements": []}
		]
	}`)
	var item ThreadItem
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	content := item.UserMessageContent()
	if len(content) != 2 {
		t.Fatalf("content len = %d, want 2", len(content))
	}
	if content[0].Type != "skill" || content[0].Name != "probe-skill" ||
		content[0].Path != "/repo/.codex/skills/probe-skill/SKILL.md" {
		t.Errorf("content[0] = %+v", content[0])
	}
	if content[1].Type != "text" || content[1].Text != "Invoke the probe-skill." {
		t.Errorf("content[1] = %+v", content[1])
	}
}

// TestUserMessageContent_GuardsOnType ensures the accessor does not try to
// parse a reasoning item's "content" (a different shape) as UserInput.
func TestUserMessageContent_GuardsOnType(t *testing.T) {
	raw := []byte(`{"id": "rs_1", "type": "reasoning", "summary": [], "content": []}`)
	var item ThreadItem
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := item.UserMessageContent(); got != nil {
		t.Errorf("UserMessageContent() = %v, want nil for reasoning item", got)
	}
}

// TestGranularApproval_Marshal asserts the wire shape: required fields
// always present, optional toggles omitted when false, and present when set.
func TestGranularApproval_Marshal(t *testing.T) {
	pol := NewGranularApproval(GranularApproval{SkillApproval: true})
	got, err := json.Marshal(pol)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	g, ok := m["granular"]
	if !ok {
		t.Fatalf("missing granular wrapper: %s", got)
	}
	// Required fields are always present (even when false).
	for _, k := range []string{"mcp_elicitations", "rules", "sandbox_approval"} {
		if _, ok := g[k]; !ok {
			t.Errorf("required field %q missing: %s", k, got)
		}
	}
	// skill_approval set -> present and true.
	if g["skill_approval"] != true {
		t.Errorf("skill_approval = %v, want true", g["skill_approval"])
	}
	// request_permissions left false -> omitted.
	if _, ok := g["request_permissions"]; ok {
		t.Errorf("request_permissions should be omitted when false: %s", got)
	}
}

// TestGranularApproval_RoundTrip confirms the Granular accessor reads back
// what NewGranularApproval wrote, and that the bare-string form reports
// not-granular.
func TestGranularApproval_RoundTrip(t *testing.T) {
	in := GranularApproval{McpElicitations: true, Rules: true, SandboxApproval: true, SkillApproval: true}
	pol := NewGranularApproval(in)

	got, ok := pol.Granular()
	if !ok {
		t.Fatal("Granular() reported not-granular for a granular policy")
	}
	if got != in {
		t.Errorf("round-trip = %+v, want %+v", got, in)
	}
	if s := pol.AskForApprovalString(); s != "" {
		t.Errorf("AskForApprovalString() = %q, want empty for granular form", s)
	}

	str := NewAskForApprovalString("on-request")
	if _, ok := str.Granular(); ok {
		t.Error("Granular() reported granular for a bare-string policy")
	}
}
