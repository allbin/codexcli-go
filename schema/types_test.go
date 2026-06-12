package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestThreadItem_CommandActions exercises the full unmarshal → accessor path
// a consumer takes: decode a commandExecution item and read its parsed
// intents. Covers every action shape codex emits (read/search/update/unknown).
func TestThreadItem_CommandActions(t *testing.T) {
	const raw = `{
		"id": "c1",
		"type": "commandExecution",
		"commandActions": [
			{"type":"read","path":"foo.txt","cmd":"cat foo.txt"},
			{"type":"search","query":"TODO","path":"src","cmd":"rg TODO src"},
			{"type":"update","path":"a.go"},
			{"type":"unknown","cmd":"go test ./..."}
		]
	}`
	var item ThreadItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []CommandAction{
		{Type: "read", Path: "foo.txt", Cmd: "cat foo.txt"},
		{Type: "search", Query: "TODO", Path: "src", Cmd: "rg TODO src"},
		{Type: "update", Path: "a.go"},
		{Type: "unknown", Cmd: "go test ./..."},
	}
	if got := item.CommandActions(); !reflect.DeepEqual(got, want) {
		t.Errorf("CommandActions() = %#v, want %#v", got, want)
	}
}

// TestParseCommandActions covers the shared parse's tolerance for the empty,
// null, and malformed inputs the accessors funnel through it.
func TestParseCommandActions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []CommandAction
	}{
		{"empty", ``, nil},
		{"null", `null`, nil},
		{"not-an-array", `{"type":"read"}`, nil},
		{"malformed", `[`, nil},
		{"single", `[{"type":"read","path":"a"}]`, []CommandAction{{Type: "read", Path: "a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseCommandActions(json.RawMessage(tc.in)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseCommandActions(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func strptr(s string) *string { return &s }

// TestThreadItem_CommandLiteral checks the Command-then-action fallback and
// that both paths run through UnwrapShellCommand.
func TestThreadItem_CommandLiteral(t *testing.T) {
	cases := []struct {
		name string
		item ThreadItem
		want string
	}{
		{
			name: "command field unwrapped",
			item: ThreadItem{Command: strptr(`/usr/bin/bash -lc 'cat foo.txt'`)},
			want: "cat foo.txt",
		},
		{
			name: "falls back to first action cmd",
			item: ThreadItem{CommandActionsRaw: json.RawMessage(`[{"type":"read","cmd":"/bin/sh -c 'ls -la'"}]`)},
			want: "ls -la",
		},
		{
			name: "skips empty action cmd",
			item: ThreadItem{CommandActionsRaw: json.RawMessage(`[{"type":"update","path":"a.go"},{"type":"unknown","cmd":"go test"}]`)},
			want: "go test",
		},
		{
			name: "empty command prefers actions",
			item: ThreadItem{Command: strptr(""), CommandActionsRaw: json.RawMessage(`[{"type":"unknown","cmd":"echo hi"}]`)},
			want: "echo hi",
		},
		{
			name: "nothing present",
			item: ThreadItem{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.CommandLiteral(); got != tc.want {
				t.Errorf("CommandLiteral() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUnwrapShellCommand covers the -lc/-c envelope forms, the POSIX '\”
// escape, and the pass-through for unwrapped input.
func TestUnwrapShellCommand(t *testing.T) {
	cases := map[string]string{
		`/usr/bin/bash -lc 'cat foo.txt'`:     "cat foo.txt",
		`/bin/sh -c 'echo hi'`:                "echo hi",
		`/usr/bin/bash -lc 'gcc -c foo.c'`:    "gcc -c foo.c",
		`/usr/bin/bash -lc 'echo '\''hi'\'''`: "echo 'hi'",
		"/usr/bin/bash -lc\t'ls'":             "ls",
		`ls -la`:                              "ls -la",
		`  trimmed  `:                         "trimmed",
		``:                                    "",
	}
	for in, want := range cases {
		if got := UnwrapShellCommand(in); got != want {
			t.Errorf("UnwrapShellCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFileUpdateChange_Kind covers the bare-string, internally-tagged, and
// externally-tagged shapes the kind discriminator arrives in.
func TestFileUpdateChange_Kind(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"bare string add", `"add"`, "add"},
		{"bare string update", `"update"`, "update"},
		{"bare string delete", `"delete"`, "delete"},
		{"typed object", `{"type":"add"}`, "add"},
		{"externally tagged", `{"update":{}}`, "update"},
		{"externally tagged delete", `{"delete":{"path":"x"}}`, "delete"},
		{"empty", ``, ""},
		{"null", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := FileUpdateChange{KindRaw: json.RawMessage(tc.raw)}
			if got := c.Kind(); got != tc.want {
				t.Errorf("Kind() for %q = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestFileChanges_KindRoundTrip verifies the discriminator survives the full
// fileChange unmarshal path consumers use, not just direct field access.
func TestFileChanges_KindRoundTrip(t *testing.T) {
	const raw = `{
		"id": "f1",
		"type": "fileChange",
		"changes": [
			{"path":"new.go","diff":"+a","kind":{"add":{}}},
			{"path":"old.go","diff":"-b","kind":"update"}
		]
	}`
	var item ThreadItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	changes := item.FileChanges()
	if len(changes) != 2 {
		t.Fatalf("FileChanges() len = %d, want 2", len(changes))
	}
	if got := changes[0].Kind(); got != "add" {
		t.Errorf("changes[0].Kind() = %q, want add", got)
	}
	if got := changes[1].Kind(); got != "update" {
		t.Errorf("changes[1].Kind() = %q, want update", got)
	}
}

// TestApprovalParams_CommandActions confirms both the v2 and legacy approval
// params expose the same typed parse over their respective raw fields.
func TestApprovalParams_CommandActions(t *testing.T) {
	const v2 = `{
		"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,
		"commandActions":[{"type":"read","path":"foo.txt","cmd":"cat foo.txt"}]
	}`
	var p CommandExecutionRequestApprovalParams
	if err := json.Unmarshal([]byte(v2), &p); err != nil {
		t.Fatalf("unmarshal v2: %v", err)
	}
	want := []CommandAction{{Type: "read", Path: "foo.txt", Cmd: "cat foo.txt"}}
	if got := p.CommandActions(); !reflect.DeepEqual(got, want) {
		t.Errorf("v2 CommandActions() = %#v, want %#v", got, want)
	}

	const legacy = `{
		"conversationId":"c","callId":"k","command":["bash","-lc","ls"],"cwd":"/",
		"parsedCmd":[{"type":"unknown","cmd":"ls"}]
	}`
	var l ExecCommandApprovalParams
	if err := json.Unmarshal([]byte(legacy), &l); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	wantLegacy := []CommandAction{{Type: "unknown", Cmd: "ls"}}
	if got := l.ParsedCommandActions(); !reflect.DeepEqual(got, wantLegacy) {
		t.Errorf("legacy ParsedCommandActions() = %#v, want %#v", got, wantLegacy)
	}
}
