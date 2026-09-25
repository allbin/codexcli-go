//go:build integration

package codexcli

// Live checks that WithThreadConfig delivers a per-thread MCP server to
// codex and that ListMcpServerStatus reads it back. They run against the
// codex on PATH in a sandboxed CODEX_HOME:
//
//	go test -tags integration -run TestLive_ThreadConfig -count=1 -v .
//
// The MCP server is an in-test streamable-HTTP stub that records the
// Authorization header of every request. TestLive_ThreadConfigMcpServer
// also spends one model turn, when the real codex home is signed in, to
// materialize a rollout, call the stub's tool and resume the thread.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

const mcpStubTool = "SuggestSessionPrompt"

func TestLive_ThreadConfigMcpServer(t *testing.T) {
	home, signedIn := sandboxCodexHome(t)
	user := startMcpStub(t)
	writeFile(t, filepath.Join(home, "config.toml"),
		fmt.Sprintf("[mcp_servers.userlevel]\nurl = %q\n", user.url))

	stub := startMcpStub(t)
	token := randomToken(t)
	var requests serverRequestLog
	conn := liveMcpConn(t, home,
		WithThreadConfig(mcpServerConfig(stub.url, token)),
		WithApprovalHandler(requests.approve),
		WithServerRequestHandler(requests.serve))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	th, err := conn.NewThread(ctx)
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	servers := waitMcpTool(t, conn, th.ID, "agentique", mcpStubTool)

	stub.requireOnly(t, token)
	requireNotInCmdlines(t, token)

	// A nested mcp_servers overlay adds to the servers config.toml
	// defines rather than replacing the table.
	if s := findServer(servers, "userlevel"); s == nil {
		t.Errorf("config.toml server missing next to the overlay: %v", serverNames(servers))
	}
	// Without a thread the process-level config shows, not the overlay.
	global, err := conn.ListMcpServerStatus(ctx, "")
	if err != nil {
		t.Fatalf("ListMcpServerStatus(no thread): %v", err)
	}
	t.Logf("servers without threadId: %v", serverNames(global))
	if findServer(global, "agentique") != nil {
		t.Errorf("overlay server listed without a threadId")
	}

	if !signedIn {
		t.Log("codex home not signed in: skipping the tool call, rollout and resume checks")
		reportPersistence(t, home, token)
		return
	}

	callStubTool(t, th, &requests)
	if n := stub.toolCalls(); n != 1 {
		t.Errorf("stub saw %d tools/call, want 1", n)
	}
	stub.requireOnly(t, token)
	conn.Close()
	before := stub.hits()

	// Resume in a fresh process with a new token set at connect time.
	token2 := randomToken(t)
	stub2 := startMcpStub(t)
	conn2 := liveMcpConn(t, home, WithThreadConfig(mcpServerConfig(stub2.url, token2)))
	if _, err := conn2.ResumeThread(ctx, th.ID); err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	waitMcpTool(t, conn2, th.ID, "agentique", mcpStubTool)
	stub2.requireOnly(t, token2)
	// The resumed thread takes only the config sent with thread/resume:
	// nothing replays the first overlay from the rollout.
	if n := stub.hits(); n != before {
		t.Errorf("first stub hit %d more time(s) after resume", n-before)
	}
	conn2.Close()

	reportPersistence(t, home, token, token2)
}

// TestLive_ThreadConfigIsolation: two threads on one connection, and a
// thread on a second connection, each reach their own server with their
// own token and nothing else.
func TestLive_ThreadConfigIsolation(t *testing.T) {
	home, _ := sandboxCodexHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	type session struct {
		stub  *mcpStub
		token string
	}
	var sessions []session
	start := func(conn *Conn) {
		s := session{stub: startMcpStub(t), token: randomToken(t)}
		// NewThread takes no per-call options, so a second thread on one
		// connection gets its own config through newThreadWithConfig.
		th, err := newThreadWithConfig(ctx, conn, mcpServerConfig(s.stub.url, s.token))
		if err != nil {
			t.Fatalf("thread/start: %v", err)
		}
		waitMcpTool(t, conn, th, "agentique", mcpStubTool)
		sessions = append(sessions, s)
	}

	connA := liveMcpConn(t, home)
	start(connA)
	start(connA)
	start(liveMcpConn(t, home))

	for _, s := range sessions {
		s.stub.requireOnly(t, s.token)
	}
}

// TestLive_ThreadConfigMcpToolApproval: codex asks before running an MCP
// tool, as an mcpServer/elicitation/request rather than an approval
// request; unanswered, or under approval policy never, the call fails.
// The server's default_tools_approval_mode = "approve" skips the ask.
func TestLive_ThreadConfigMcpToolApproval(t *testing.T) {
	home, signedIn := sandboxCodexHome(t)
	if !signedIn {
		t.Skip("codex home not signed in")
	}
	cases := []struct {
		name    string
		policy  string
		mode    string // default_tools_approval_mode; "" leaves it unset
		handler bool
		asks    bool
		runs    bool
	}{
		{"on-request/accepting-handler", "on-request", "", true, true, true},
		{"on-request/no-handler", "on-request", "", false, false, false},
		{"never", "never", "", false, false, false},
		{"never/mode-approve", "never", "approve", false, false, true},
		{"on-request/mode-approve", "on-request", "approve", true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := startMcpStub(t)
			var requests serverRequestLog
			cfg := mcpServerConfig(stub.url, randomToken(t))
			if tc.mode != "" {
				cfg["mcp_servers"].(map[string]any)["agentique"].(map[string]any)["default_tools_approval_mode"] = tc.mode
			}
			opts := []Option{
				WithEphemeralThread(),
				WithApprovalPolicy(schema.NewAskForApprovalString(tc.policy)),
				WithThreadConfig(cfg),
			}
			if tc.handler {
				opts = append(opts, WithApprovalHandler(requests.approve), WithServerRequestHandler(requests.serve))
			}
			conn := liveMcpConn(t, home, opts...)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			th, err := conn.NewThread(ctx)
			if err != nil {
				t.Fatalf("NewThread: %v", err)
			}
			waitMcpTool(t, conn, th.ID, "agentique", mcpStubTool)
			callStubTool(t, th, &requests)
			asked := len(requests.methods()) > 0
			ran := stub.toolCalls() > 0
			t.Logf("RESULT policy=%s mode=%q handler=%v: asked=%v ran=%v", tc.policy, tc.mode, tc.handler, asked, ran)
			if tc.handler && asked != tc.asks {
				t.Errorf("asked = %v, want %v", asked, tc.asks)
			}
			if ran != tc.runs {
				t.Errorf("tool ran = %v, want %v", ran, tc.runs)
			}
		})
	}
}

// callStubTool runs one turn asking the model to call the stub's tool and
// logs the MCP tool call items and server requests it produced.
func callStubTool(t *testing.T, th *Thread, requests *serverRequestLog) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stream, err := th.StartTurn(ctx, fmt.Sprintf(
		"Call the %s tool from the agentique MCP server once with the prompt \"hi\" and title \"t\", then reply done.", mcpStubTool))
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()
	var calls []schema.ThreadItem
	turn, err := drainTurnObserving(stream, 2*time.Minute, func(ev Event) {
		if e, ok := ev.(*ItemCompletedEvent); ok && e.Item.Type == schema.ItemTypeMcpToolCall {
			calls = append(calls, e.Item)
		}
	})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	t.Logf("turn status %s; mcp tool calls %d; server requests %v", turn.Status, len(calls), requests.methods())
	for _, c := range calls {
		t.Logf("mcpToolCall server=%v tool=%v status=%s result=%s error=%s",
			deref(c.Server), deref(c.Tool), deref(c.Status), c.McpResult, c.McpError)
	}
}

// newThreadWithConfig sends thread/start with a config overlay built by
// WithThreadConfig on top of the connection's options. NewThread takes
// no per-call options, so this is how one connection hosts threads with
// different MCP configs.
func newThreadWithConfig(ctx context.Context, conn *Conn, cfg map[string]any) (string, error) {
	params := resolveOptions(conn.options.callOpts(), []Option{WithEphemeralThread(), WithThreadConfig(cfg)}).buildThreadStartParams()
	var resp schema.ThreadStartResponse
	if err := conn.rpc.Request(ctx, "thread/start", params, &resp); err != nil {
		return "", err
	}
	return resp.Thread.ID, nil
}

func mcpServerConfig(url, token string) map[string]any {
	return map[string]any{
		"mcp_servers": map[string]any{
			"agentique": map[string]any{
				"url":          url,
				"http_headers": map[string]any{"Authorization": "Bearer " + token},
			},
		},
	}
}

// sandboxCodexHome makes an empty CODEX_HOME and copies the real home's
// auth.json into it when there is one, reporting whether it did.
func sandboxCodexHome(t *testing.T) (string, bool) {
	t.Helper()
	home := t.TempDir()
	real, err := resolveCodexHome("")
	if err != nil {
		return home, false
	}
	auth, err := os.ReadFile(filepath.Join(real, "auth.json"))
	if err != nil {
		return home, false
	}
	writeFile(t, filepath.Join(home, "auth.json"), string(auth))
	return home, true
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func liveMcpConn(t *testing.T, home string, opts ...Option) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := New(append([]Option{WithEnv(map[string]string{"CODEX_HOME": home})}, opts...)...)
	// Connect ties the subprocess to ctx only for the handshake.
	conn, err := client.Connect(context.WithoutCancel(ctx))
	if err != nil {
		t.Skipf("codex not runnable: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// waitMcpTool polls mcpServerStatus/list until server lists tool: codex
// connects to MCP servers in the background after thread/start.
func waitMcpTool(t *testing.T, conn *Conn, threadID, server, tool string) []schema.McpServerStatus {
	t.Helper()
	began := time.Now()
	deadline := began.Add(30 * time.Second)
	polls := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		servers, err := conn.ListMcpServerStatus(ctx, threadID)
		cancel()
		polls++
		if err != nil {
			t.Fatalf("ListMcpServerStatus: %v", err)
		}
		if s := findServer(servers, server); s != nil {
			if _, ok := s.Tools[tool]; ok {
				t.Logf("%s listed %v after %d poll(s), %s; auth=%s runtime=%v keys=%v",
					server, s.ToolNames(), polls, time.Since(began).Round(time.Millisecond),
					s.AuthStatus, derefStatus(s.RuntimeStatus), serverNames(servers))
				return servers
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s/%s never listed; last reply %s", server, tool, rawServers(servers))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func findServer(servers []schema.McpServerStatus, name string) *schema.McpServerStatus {
	for i := range servers {
		if servers[i].Name == name {
			return &servers[i]
		}
	}
	return nil
}

func serverNames(servers []schema.McpServerStatus) []string {
	var out []string
	for _, s := range servers {
		out = append(out, s.Name)
	}
	return out
}

func rawServers(servers []schema.McpServerStatus) string {
	var parts []string
	for _, s := range servers {
		parts = append(parts, string(s.Raw))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func derefStatus(p *schema.McpServerConnectionStatus) string {
	if p == nil {
		return "<nil>"
	}
	return string(*p)
}

func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "tok" + hex.EncodeToString(b)
}

// requireNotInCmdlines fails when token appears in the argv of any
// process this user can read, which covers codex and whatever it spawned.
func requireNotInCmdlines(t *testing.T, token string) {
	t.Helper()
	paths, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	if len(paths) == 0 {
		t.Log("no /proc: cmdline check skipped")
		return
	}
	appServers := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte("app-server")) {
			appServers++
		}
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("token in %s: %q", p, bytes.ReplaceAll(b, []byte{0}, []byte{' '}))
		}
	}
	t.Logf("token absent from %d process cmdlines (%d app-server)", len(paths), appServers)
}

// reportPersistence logs every file under home that holds one of the
// tokens. Codex writing a thread's config overlay to disk decides whether
// a consumer can pass a secret inline or should use bearer_token_env_var.
func reportPersistence(t *testing.T, home string, tokens ...string) {
	t.Helper()
	var hits, files []string
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(home, path)
		files = append(files, rel)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, tok := range tokens {
			if bytes.Contains(b, []byte(tok)) {
				hits = append(hits, rel)
				break
			}
		}
		return nil
	})
	for _, f := range files {
		if strings.HasPrefix(f, "sessions/") || strings.HasSuffix(f, ".sqlite") || f == "config.toml" {
			t.Logf("CODEX_HOME scanned: %s", f)
		}
	}
	if len(hits) > 0 {
		t.Logf("PERSISTED: token found on disk in %v", hits)
	} else {
		t.Logf("NOT PERSISTED: token in none of %d files", len(files))
	}
}

// serverRequestLog accepts every approval and elicitation and records
// which ones codex sent.
type serverRequestLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *serverRequestLog) add(s string) {
	l.mu.Lock()
	l.seen = append(l.seen, s)
	l.mu.Unlock()
}

func (l *serverRequestLog) methods() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen...)
}

func (l *serverRequestLog) approve(_ context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	l.add(fmt.Sprintf("approval %T", req))
	return Accept{}, nil
}

func (l *serverRequestLog) serve(_ context.Context, req ServerRequest) (json.RawMessage, error) {
	l.add(req.Method + " " + string(req.Params))
	if req.Method == schema.MethodMcpServerElicitationRequest {
		return json.RawMessage(`{"action":"accept","content":{}}`), nil
	}
	return nil, errors.New("unhandled")
}

// mcpStub is a minimal streamable-HTTP MCP server with one tool.
type mcpStub struct {
	url string

	mu    sync.Mutex
	auths []string
	calls int
}

func startMcpStub(t *testing.T) *mcpStub {
	t.Helper()
	s := &mcpStub{}
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	s.url = srv.URL + "/mcp"
	return s
}

func (s *mcpStub) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.mu.Unlock()
	if r.Method != http.MethodPost {
		// No server-initiated stream.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if msg.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch msg.Method {
	case "initialize":
		v := msg.Params.ProtocolVersion
		if v == "" {
			v = "2025-06-18"
		}
		result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agentique-stub", "version": "0.0.1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{
			"name":        mcpStubTool,
			"description": "Suggest a new session to the user.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":  map[string]any{"type": "string"},
					"prompt": map[string]any{"type": "string"},
				},
				"required": []string{"prompt"},
			},
		}}}
	case "tools/call":
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "suggested"}}}
	case "ping":
		result = map[string]any{}
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": msg.ID,
			"error": map[string]any{"code": -32601, "message": "method not found"},
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
}

func (s *mcpStub) hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.auths)
}

func (s *mcpStub) toolCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// requireOnly fails unless the stub was hit and every request carried
// exactly Bearer token.
func (s *mcpStub) requireOnly(t *testing.T, token string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auths) == 0 {
		t.Errorf("stub %s never hit", s.url)
		return
	}
	for _, a := range s.auths {
		if a != "Bearer "+token {
			t.Errorf("stub %s saw Authorization %q, want Bearer %s", s.url, a, token)
			return
		}
	}
	t.Logf("stub %s: %d request(s), all Bearer <its token>", s.url, len(s.auths))
}
