//go:build integration

package codexcli

// Live checks of a custom model provider in its own CODEX_HOME, the way a
// consumer runs a "local model" provider: config.toml names a
// [model_providers.<id>] with a base_url, and CODEX_HOME reaches codex
// only through WithEnv. The model server is an in-test stub of the
// OpenAI Responses API, so these need no account and spend nothing:
//
//	go test -tags integration -run TestLive_Provider -count=1 -v .
//
// Each test guards the real ~/.codex/auth.json and never copies it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

const (
	stubModel    = "stub-model"
	stubReply    = "stub-ok"
	stubKeyEnv   = "LOCAL_KEY"
	stubProvider = "local"
)

// TestLive_ProviderWireResponses: a provider with wire_api = "responses"
// completes a turn against a stub that implements only POST /responses as
// SSE. It logs every endpoint codex called and checks the key named by
// env_key, and only that one, reached the server.
func TestLive_ProviderWireResponses(t *testing.T) {
	guardRealAuth(t)
	stub := startResponsesStub(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "config.toml"), providerConfig(stub.baseURL(), "responses"))
	key := randomToken(t)
	decoy := randomToken(t)
	conn := liveProviderConn(t, home, map[string]string{stubKeyEnv: key, "OPENAI_API_KEY": decoy}, WithEphemeralThread())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	th, err := conn.NewThread(ctx)
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	r := th.Response()
	t.Logf("thread/start: model=%s provider=%s", r.Model, r.ModelProvider)
	reply := stubTurn(t, th, "say hi")
	if !strings.Contains(reply, stubReply) {
		t.Errorf("agent message %q, want %q", reply, stubReply)
	}
	stub.logRequests(t)
	stub.requireAuth(t, "Bearer "+key)
	if n := stub.count("POST", "/v1/responses"); n == 0 {
		t.Errorf("no POST /v1/responses")
	}
}

// TestLive_ProviderMissingKey: with env_key set and the variable absent,
// codex fails, and this logs where.
func TestLive_ProviderMissingKey(t *testing.T) {
	guardRealAuth(t)
	stub := startResponsesStub(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "config.toml"), providerConfig(stub.baseURL(), "responses"))
	stage, err := providerFailure(t, home, nil)
	t.Logf("RESULT missing %s: stage=%s err=%v", stubKeyEnv, stage, err)
	stub.logRequests(t)
	if stage == "none" {
		t.Errorf("turn completed without %s", stubKeyEnv)
	}
}

// TestLive_ProviderWireChat: wire_api = "chat" is rejected; this logs at
// which stage (connect, thread/start or the first turn) and with what
// error, and whether the stub was ever called.
func TestLive_ProviderWireChat(t *testing.T) {
	guardRealAuth(t)
	stub := startResponsesStub(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "config.toml"), providerConfig(stub.baseURL(), "chat"))
	stage, err := providerFailure(t, home, map[string]string{stubKeyEnv: randomToken(t)})
	t.Logf("RESULT wire_api=chat: stage=%s err=%v", stage, err)
	stub.logRequests(t)
	if stage == "none" {
		t.Errorf("wire_api = \"chat\" completed a turn")
	}
}

// providerFailure connects, starts a thread and runs a turn, and returns
// the first stage that failed with its error, or "none".
func providerFailure(t *testing.T, home string, env map[string]string) (string, error) {
	t.Helper()
	var stderr stderrLog
	client := New(WithEphemeralThread(),
		WithEnv(mergeEnv(map[string]string{"CODEX_HOME": home}, env)),
		WithStderrCallback(stderr.add))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := client.Connect(context.Background())
	if err != nil {
		stderr.log(t)
		return "connect", err
	}
	defer conn.Close()
	t.Logf("connect ok; config/read model_provider=%s", configValue(t, conn, "model_provider"))
	th, err := conn.NewThread(ctx)
	if err != nil {
		stderr.log(t)
		return "thread/start", err
	}
	stream, err := th.StartTurn(ctx, "say hi")
	if err != nil {
		stderr.log(t)
		return "turn/start", err
	}
	defer stream.Close()
	var errs []string
	turn, err := drainTurnObserving(stream, 2*time.Minute, func(ev Event) {
		if e, ok := ev.(*ErrorEvent); ok {
			errs = append(errs, fmt.Sprintf("%+v", e))
		}
	})
	stderr.log(t)
	if err != nil {
		return "turn", err
	}
	for _, e := range errs {
		t.Logf("error event: %s", e)
	}
	if turn.Status != schema.TurnCompleted {
		b, _ := json.Marshal(turn.Error)
		return "turn", fmt.Errorf("status %s: %s", turn.Status, b)
	}
	return "none", nil
}

func providerConfig(baseURL, wireAPI string) string {
	return fmt.Sprintf(`model = %q
model_provider = %q

[model_providers.%s]
name = "Local stub"
base_url = %q
wire_api = %q
env_key = %q
`, stubModel, stubProvider, stubProvider, baseURL, wireAPI, stubKeyEnv)
}

func mergeEnv(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func liveProviderConn(t *testing.T, home string, env map[string]string, opts ...Option) *Conn {
	t.Helper()
	var stderr stderrLog
	t.Cleanup(func() {
		if t.Failed() {
			stderr.log(t)
		}
	})
	client := New(append([]Option{
		WithEnv(mergeEnv(map[string]string{"CODEX_HOME": home}, env)),
		WithStderrCallback(stderr.add),
	}, opts...)...)
	conn, err := client.Connect(context.Background())
	if err != nil {
		stderr.log(t)
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// stubTurn runs one turn and returns the agent's message text.
func stubTurn(t *testing.T, th *Thread, prompt string, opts ...Option) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stream, err := th.StartTurn(ctx, prompt, opts...)
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()
	var reply strings.Builder
	turn, err := drainTurnObserving(stream, 2*time.Minute, func(ev Event) {
		switch e := ev.(type) {
		case *ItemCompletedEvent:
			if e.Item.Type == schema.ItemTypeAgentMessage {
				reply.WriteString(e.Item.Text)
			}
		case *ErrorEvent:
			t.Logf("error event: %+v", e)
		}
	})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if turn.Status != schema.TurnCompleted {
		b, _ := json.Marshal(turn.Error)
		t.Fatalf("turn status %s: %s", turn.Status, b)
	}
	return reply.String()
}

type stderrLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *stderrLog) add(s string) {
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

func (l *stderrLog) log(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		t.Logf("stderr: %s", s)
	}
}

type stubRequest struct {
	method, path, auth string
	body               []byte
}

// responsesStub is the least of the OpenAI Responses API codex needs for
// a text turn: POST /v1/responses answers with a streamed assistant
// message. Every other path is a 404, recorded like the rest.
type responsesStub struct {
	srv *httptest.Server

	mu   sync.Mutex
	reqs []stubRequest
}

func startResponsesStub(t *testing.T) *responsesStub {
	t.Helper()
	s := &responsesStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *responsesStub) baseURL() string { return s.srv.URL + "/v1" }

func (s *responsesStub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, stubRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
	s.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(ev map[string]any) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	msg := map[string]any{
		"type": "message", "id": "msg_stub", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": stubReply, "annotations": []any{}}},
	}
	send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_stub"}})
	send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": msg})
	send(map[string]any{"type": "response.output_text.delta", "item_id": "msg_stub", "output_index": 0, "content_index": 0, "delta": stubReply})
	send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": msg})
	send(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_stub",
		"usage": map[string]any{
			"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 0},
			"output_tokens": 2, "output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens": 12,
		},
	}})
}

func (s *responsesStub) requests() []stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubRequest(nil), s.reqs...)
}

func (s *responsesStub) count(method, path string) int {
	n := 0
	for _, r := range s.requests() {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

// logRequests logs each distinct endpoint codex called and how often.
func (s *responsesStub) logRequests(t *testing.T) {
	t.Helper()
	seen := map[string]int{}
	var order []string
	for _, r := range s.requests() {
		k := r.method + " " + r.path
		if seen[k] == 0 {
			order = append(order, k)
		}
		seen[k]++
	}
	if len(order) == 0 {
		t.Log("stub: no requests")
	}
	for _, k := range order {
		t.Logf("stub: %s x%d", k, seen[k])
	}
}

// requireAuth fails unless every request carried want.
func (s *responsesStub) requireAuth(t *testing.T, want string) {
	t.Helper()
	for _, r := range s.requests() {
		if r.auth != want {
			t.Errorf("%s %s: Authorization %q, want the %s key", r.method, r.path, redact(r.auth), stubKeyEnv)
		}
	}
}

func redact(s string) string {
	if len(s) > 12 {
		return s[:12] + "..."
	}
	return s
}

// lastRequestBody returns the body of the last POST /v1/responses.
func (s *responsesStub) lastRequestBody() []byte {
	reqs := s.requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].method == http.MethodPost && reqs[i].path == "/v1/responses" {
			return reqs[i].body
		}
	}
	return nil
}

// configValue reads one top-level key of the effective config the
// app-server process holds, through config/read.
func configValue(t *testing.T, conn *Conn, key string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var resp struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := conn.rpc.Request(ctx, "config/read", map[string]any{}, &resp); err != nil {
		return "config/read error: " + err.Error()
	}
	if v, ok := resp.Config[key]; ok {
		return string(v)
	}
	return "<unset>"
}
