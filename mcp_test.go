package codexcli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// capturingExecutor records the StartConfig the client spawns with, so a
// test can assert what reaches the subprocess's argv and env.
type capturingExecutor struct {
	*BidiFixtureExecutor
	mu  sync.Mutex
	cfg StartConfig
}

func (e *capturingExecutor) Start(ctx context.Context, cfg *StartConfig) (*Process, error) {
	e.mu.Lock()
	e.cfg = *cfg
	e.mu.Unlock()
	return e.BidiFixtureExecutor.Start(ctx, cfg)
}

// wireConfig returns the `config` object of a thread/start or
// thread/resume request as it went over the wire.
func wireConfig(t *testing.T, params json.RawMessage) map[string]any {
	t.Helper()
	var p struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		t.Fatalf("decode params %s: %v", params, err)
	}
	return p.Config
}

func threadResponse(id string) map[string]any {
	return map[string]any{
		"thread": map[string]any{"id": id, "cwd": "/tmp", "createdAt": 0},
		"model":  "gpt-5", "modelProvider": "openai", "cwd": "/tmp",
	}
}

func mcpServer(token string) map[string]any {
	return map[string]any{
		"mcp_servers": map[string]any{
			"agentique": map[string]any{
				"url":          "http://127.0.0.1:1/mcp",
				"http_headers": map[string]any{"Authorization": "Bearer " + token},
			},
		},
	}
}

func TestWithThreadConfig_ReachesThreadStart(t *testing.T) {
	const token = "tok-secret-start"
	fix := &capturingExecutor{BidiFixtureExecutor: NewBidiFixtureExecutor()}
	client := NewWithExecutor(fix, WithThreadConfig(mcpServer(token)))

	got := make(chan json.RawMessage, 1)
	go skillsServer(t, fix.BidiFixtureExecutor, map[string]func(id, params json.RawMessage){
		"thread/start": func(id, params json.RawMessage) {
			got <- params
			_ = fix.SendResponse(id, threadResponse("thr_1"))
		},
	})

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	if _, err := conn.NewThread(context.Background()); err != nil {
		t.Fatalf("NewThread: %v", err)
	}

	params := <-got
	if cfg := wireConfig(t, params); !reflect.DeepEqual(cfg, mcpServer(token)) {
		t.Errorf("thread/start config = %v, want %v", cfg, mcpServer(token))
	}

	fix.mu.Lock()
	defer fix.mu.Unlock()
	for _, a := range fix.cfg.Args {
		if strings.Contains(a, token) {
			t.Errorf("token leaked into argv: %q", fix.cfg.Args)
		}
	}
	for k, v := range fix.cfg.Env {
		if strings.Contains(v, token) {
			t.Errorf("token leaked into env %s", k)
		}
	}
}

func TestWithThreadConfig_ResumeCarriesConnectTimeConfig(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithThreadConfig(mcpServer("tok-resume")))

	got := make(chan json.RawMessage, 2)
	go skillsServer(t, fix, map[string]func(id, params json.RawMessage){
		"thread/resume": func(id, params json.RawMessage) {
			got <- params
			_ = fix.SendResponse(id, threadResponse("thr_1"))
		},
	})

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	if _, err := conn.ResumeThread(context.Background(), "thr_1"); err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	if cfg := wireConfig(t, <-got); !reflect.DeepEqual(cfg, mcpServer("tok-resume")) {
		t.Errorf("thread/resume config = %v, want %v", cfg, mcpServer("tok-resume"))
	}

	// A resume-time config merges over the connect-time one.
	_, err = conn.ResumeThread(context.Background(), "thr_1",
		WithThreadConfig(map[string]any{"model_reasoning_summary": "none"}))
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	want := mcpServer("tok-resume")
	want["model_reasoning_summary"] = "none"
	if cfg := wireConfig(t, <-got); !reflect.DeepEqual(cfg, want) {
		t.Errorf("thread/resume config = %v, want %v", cfg, want)
	}
}

func TestWithThreadConfig_DeepMerge(t *testing.T) {
	first := map[string]any{
		"mcp_servers": map[string]any{
			"a": map[string]any{"url": "http://a", "http_headers": map[string]any{"X-One": "1"}},
		},
		"sandbox_mode": "read-only",
	}
	second := map[string]any{
		"mcp_servers": map[string]any{
			"a": map[string]any{"http_headers": map[string]any{"X-Two": "2"}},
			"b": map[string]any{"command": "srv", "args": []any{"--stdio"}},
		},
		"sandbox_mode": "workspace-write",
	}
	o := resolveOptions([]Option{WithThreadConfig(first)}, []Option{WithThreadConfig(second)})

	want := map[string]any{
		"mcp_servers": map[string]any{
			"a": map[string]any{"url": "http://a", "http_headers": map[string]any{"X-One": "1", "X-Two": "2"}},
			"b": map[string]any{"command": "srv", "args": []any{"--stdio"}},
		},
		"sandbox_mode": "workspace-write",
	}
	if got := o.buildThreadStartParams().Config; !reflect.DeepEqual(got, want) {
		t.Errorf("start config = %v, want %v", got, want)
	}
	if got := o.buildThreadResumeParams("t").Config; !reflect.DeepEqual(got, want) {
		t.Errorf("resume config = %v, want %v", got, want)
	}

	// A later non-map value replaces a map, and vice versa.
	o = resolveOptions(nil, []Option{
		WithThreadConfig(map[string]any{"x": map[string]any{"k": 1}}),
		WithThreadConfig(map[string]any{"x": "flat"}),
	})
	if got := o.threadConfig["x"]; got != "flat" {
		t.Errorf("x = %v, want flat", got)
	}
}

func TestWithThreadConfig_DoesNotAliasCallerMaps(t *testing.T) {
	headers := map[string]any{"Authorization": "Bearer one"}
	server := map[string]any{"url": "http://a", "http_headers": headers}
	cfg := map[string]any{"mcp_servers": map[string]any{"a": server}}
	opt := WithThreadConfig(cfg)

	// Merging a second config must not write into the caller's maps.
	o := resolveOptions([]Option{opt}, []Option{WithThreadConfig(map[string]any{
		"mcp_servers": map[string]any{"a": map[string]any{"http_headers": map[string]any{"X-Extra": "1"}}},
	})})
	if len(headers) != 1 || len(server) != 2 {
		t.Errorf("caller maps mutated: headers=%v server=%v", headers, server)
	}

	// Mutating the caller's map after the option was built changes nothing.
	headers["Authorization"] = "Bearer two"
	o = resolveOptions([]Option{opt}, nil)
	got := o.threadConfig["mcp_servers"].(map[string]any)["a"].(map[string]any)["http_headers"].(map[string]any)
	if got["Authorization"] != "Bearer one" {
		t.Errorf("Authorization = %v, want the value at option time", got["Authorization"])
	}

	// Mutating the resolved config does not reach the next resolve.
	got["Authorization"] = "Bearer three"
	o = resolveOptions([]Option{opt}, nil)
	got = o.threadConfig["mcp_servers"].(map[string]any)["a"].(map[string]any)["http_headers"].(map[string]any)
	if got["Authorization"] != "Bearer one" {
		t.Errorf("Authorization = %v, resolved maps are shared across resolves", got["Authorization"])
	}
}

func TestWithThreadConfig_ExtraConfigWins(t *testing.T) {
	o := resolveOptions(nil, []Option{
		WithThreadConfig(map[string]any{"typed": true}),
		WithThreadExtra(map[string]any{"config": map[string]any{"extra": true}}),
	})
	for name, params := range map[string]any{
		"thread/start":  o.buildThreadStartParams(),
		"thread/resume": o.buildThreadResumeParams("t"),
	} {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		if cfg := wireConfig(t, raw); !reflect.DeepEqual(cfg, map[string]any{"extra": true}) {
			t.Errorf("%s config = %v, want the WithThreadExtra one", name, cfg)
		}
	}
}

func TestWithThreadConfig_NotSentOnTurnStart(t *testing.T) {
	conn := resolveOptions([]Option{WithThreadConfig(map[string]any{"k": "v"})}, nil)
	raw, err := json.Marshal(resolveOptions(conn.callOpts(), nil).buildTurnStartParams("t", nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"config"`) {
		t.Errorf("turn/start carries config: %s", raw)
	}
}

func TestConnListMcpServerStatus_Paginates(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix)

	var mu sync.Mutex
	var seen []schema.ListMcpServerStatusParams
	go skillsServer(t, fix, map[string]func(id, params json.RawMessage){
		schema.MethodMcpServerStatusList: func(id, params json.RawMessage) {
			var p schema.ListMcpServerStatusParams
			_ = json.Unmarshal(params, &p)
			mu.Lock()
			seen = append(seen, p)
			mu.Unlock()
			if p.Cursor == nil {
				_ = fix.SendResponse(id, map[string]any{
					"data": []any{map[string]any{
						"name":       "agentique",
						"authStatus": "bearerToken", "runtimeStatus": "connected",
						"tools": map[string]any{
							"SuggestSessionPrompt": map[string]any{
								"name": "SuggestSessionPrompt", "description": "Suggest a session",
								"inputSchema": map[string]any{"type": "object"},
							},
						},
						"resources": []any{}, "resourceTemplates": []any{},
						"serverInfo":  map[string]any{"name": "agentique", "version": "1.0.0"},
						"futureField": 42,
					}},
					"nextCursor": "page2",
				})
				return
			}
			_ = fix.SendResponse(id, map[string]any{
				"data": []any{map[string]any{
					"name": "docs", "authStatus": "unsupported", "runtimeStatus": nil,
					"tools": map[string]any{}, "resources": []any{}, "resourceTemplates": []any{},
					"toolsError": "boom",
				}},
				"nextCursor": nil,
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
	servers, err := conn.ListMcpServerStatus(ctx, "thr_1")
	if err != nil {
		t.Fatalf("ListMcpServerStatus: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(seen))
	}
	for i, p := range seen {
		if p.ThreadId == nil || *p.ThreadId != "thr_1" {
			t.Errorf("request %d threadId = %v, want thr_1", i, p.ThreadId)
		}
	}
	if seen[1].Cursor == nil || *seen[1].Cursor != "page2" {
		t.Errorf("second request cursor = %v, want page2", seen[1].Cursor)
	}

	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(servers))
	}
	a := servers[0]
	if a.Name != "agentique" || a.AuthStatus != schema.McpAuthStatusBearerToken {
		t.Errorf("server = %+v", a)
	}
	if a.RuntimeStatus == nil || *a.RuntimeStatus != schema.McpServerConnectionConnected {
		t.Errorf("RuntimeStatus = %v, want connected", a.RuntimeStatus)
	}
	if names := a.ToolNames(); !reflect.DeepEqual(names, []string{"SuggestSessionPrompt"}) {
		t.Errorf("ToolNames = %v", names)
	}
	if tool := a.Tools["SuggestSessionPrompt"]; tool.Description == nil || string(tool.InputSchema) != `{"type":"object"}` {
		t.Errorf("tool = %+v", tool)
	}
	if a.ServerInfo == nil || a.ServerInfo.Version != "1.0.0" {
		t.Errorf("ServerInfo = %+v", a.ServerInfo)
	}
	if !strings.Contains(string(a.Raw), `"futureField":42`) {
		t.Errorf("Raw = %s, want the full entry", a.Raw)
	}
	d := servers[1]
	if d.RuntimeStatus != nil || d.ToolsError == nil || *d.ToolsError != "boom" {
		t.Errorf("second server = %+v", d)
	}
}

func TestConnListMcpServerStatus_NoThreadOmitsThreadID(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix)

	got := make(chan json.RawMessage, 1)
	go skillsServer(t, fix, map[string]func(id, params json.RawMessage){
		schema.MethodMcpServerStatusList: func(id, params json.RawMessage) {
			got <- params
			_ = fix.SendResponse(id, map[string]any{"data": []any{}})
		},
	})

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	servers, err := conn.ListMcpServerStatus(context.Background(), "")
	if err != nil {
		t.Fatalf("ListMcpServerStatus: %v", err)
	}
	if len(servers) != 0 {
		t.Errorf("servers = %v", servers)
	}
	if p := <-got; strings.Contains(string(p), "threadId") {
		t.Errorf("params = %s, want no threadId", p)
	}
}
