package schema

import (
	"encoding/json"
	"testing"
)

// TestUserInputConstructors verifies each constructor emits exactly the
// wire shape codex app-server expects: the right `type` discriminator and
// only the fields that variant requires (omitempty drops the rest).
func TestUserInputConstructors(t *testing.T) {
	cases := []struct {
		name  string
		input UserInput
		want  string
	}{
		{"text", TextInput("hello"), `{"type":"text","text":"hello"}`},
		{"image", ImageInput("https://example.com/a.png"), `{"type":"image","url":"https://example.com/a.png"}`},
		{"localImage", LocalImageInput("/tmp/a.png"), `{"type":"localImage","path":"/tmp/a.png"}`},
		{"skill", SkillInput("review", "/repo/.codex/skills/review"), `{"type":"skill","path":"/repo/.codex/skills/review","name":"review"}`},
		{"mention", MentionInput("main.go", "/repo/main.go"), `{"type":"mention","path":"/repo/main.go","name":"main.go"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Compare structurally so field ordering is not asserted.
			if !jsonEqual(t, string(got), tc.want) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var ma, mb map[string]any
	if err := json.Unmarshal([]byte(a), &ma); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal([]byte(b), &mb); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	if len(ma) != len(mb) {
		return false
	}
	for k, va := range ma {
		if vb, ok := mb[k]; !ok || va != vb {
			return false
		}
	}
	return true
}
