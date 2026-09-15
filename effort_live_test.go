//go:build integration

package codexcli

// Live checks of the reasoning-effort behaviour WithEffort documents. They
// run against the codex on PATH with a signed-in account and spend eight
// small model turns:
//
//	go test -tags integration -run TestLive_Effort -count=1 -v .
//
// The receipt is the model request itself. Each test points codex at a
// local proxy through the openai_base_url config key and reads
// reasoning.effort off every model request codex sends, tagging each one
// with the last user message so it can be matched to a turn.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// TestLive_EffortOverrideIsSticky: with no connect-time effort, a per-call
// WithEffort stays in force for the plain turns after it.
func TestLive_EffortOverrideIsSticky(t *testing.T) {
	p := startEffortProxy(t)
	th := liveThread(t, p)
	level := levelOtherThan(th)

	liveTurn(t, th, "sticky-override", WithEffort(level))
	liveTurn(t, th, "sticky-plain")

	p.requireEffort(t, "sticky-override", level)
	p.requireEffort(t, "sticky-plain", level)
}

// TestLive_EffortConnectTimeRevertsOverride: a connect-time WithEffort is
// re-sent on every plain turn, and WithEffort("") sends nothing, so it
// does not revert an override.
func TestLive_EffortConnectTimeRevertsOverride(t *testing.T) {
	p := startEffortProxy(t)
	th := liveThread(t, p, WithEffort("low"))

	liveTurn(t, th, "connect-override", WithEffort("high"))
	liveTurn(t, th, "connect-empty", WithEffort(""))
	liveTurn(t, th, "connect-plain")

	p.requireEffort(t, "connect-override", "high")
	p.requireEffort(t, "connect-empty", "high")
	p.requireEffort(t, "connect-plain", "low")
}

// TestLive_EffortSettingsEventNeedsExperimentalAPI: codex reports a
// changed effort through thread/settings/updated only on connections that
// negotiated experimentalApi.
func TestLive_EffortSettingsEventNeedsExperimentalAPI(t *testing.T) {
	for _, experimental := range []bool{false, true} {
		t.Run(fmt.Sprintf("experimental=%v", experimental), func(t *testing.T) {
			p := startEffortProxy(t)
			var opts []Option
			if experimental {
				opts = append(opts, WithExperimentalAPI())
			}
			th := liveThread(t, p, opts...)
			level := levelOtherThan(th)
			tag := fmt.Sprintf("settings-event-%v", experimental)

			got := liveTurn(t, th, tag, WithEffort(level))

			p.requireEffort(t, tag, level)
			switch {
			case experimental && (len(got) != 1 || got[0].Effort != level):
				t.Errorf("settings events = %+v, want one with Effort %q", got, level)
			case !experimental && len(got) != 0:
				t.Errorf("settings events = %+v, want none without experimentalApi", got)
			}
		})
	}
}

// levelOtherThan picks an effort that differs from the thread's starting
// one, so an override is observable.
func levelOtherThan(th *Thread) string {
	if e := th.Response().ReasoningEffort; e != nil && *e == "high" {
		return "low"
	}
	return "high"
}

// liveThread connects through the proxy and starts an ephemeral thread.
// It sets no cwd: codex writes a trust entry to config.toml for an
// explicit cwd under a permissive sandbox.
func liveThread(t *testing.T, p *effortProxy, opts ...Option) *Thread {
	t.Helper()
	// Connect ties the subprocess to ctx, so it must outlive the test body.
	ctx := context.Background()
	exec := argsExecutor{
		inner: NewLocalExecutor(),
		args:  []string{"-c", fmt.Sprintf("openai_base_url=%q", p.baseURL)},
	}
	client := NewWithExecutor(exec, append([]Option{WithEphemeralThread()}, opts...)...)
	conn, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	th, err := conn.NewThread(ctx)
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	return th
}

// liveTurn runs one turn whose prompt carries tag and returns the
// thread/settings/updated events seen on its stream.
func liveTurn(t *testing.T, th *Thread, tag string, opts ...Option) []*ThreadSettingsUpdatedEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	prompt := fmt.Sprintf("Reply with exactly the text %s and nothing else. Do not run any tools.", tag)
	stream, err := th.StartTurn(ctx, prompt, opts...)
	if err != nil {
		t.Fatalf("%s: StartTurn: %v", tag, err)
	}
	defer stream.Close()
	var settings []*ThreadSettingsUpdatedEvent
	turn, err := drainTurnObserving(stream, 2*time.Minute, func(ev Event) {
		if e, ok := ev.(*ThreadSettingsUpdatedEvent); ok {
			settings = append(settings, e)
		}
	})
	if err != nil {
		t.Fatalf("%s: %v", tag, err)
	}
	if turn.Status != "completed" {
		t.Fatalf("%s: turn status %s, error %+v", tag, turn.Status, turn.Error)
	}
	return settings
}

type argsExecutor struct {
	inner Executor
	args  []string
}

func (e argsExecutor) Start(ctx context.Context, cfg *StartConfig) (*Process, error) {
	c := *cfg
	c.Args = append(append([]string{}, e.args...), cfg.Args...)
	return e.inner.Start(ctx, &c)
}

type modelRequest struct {
	effort   string
	lastUser string
}

// effortProxy forwards codex's model traffic to the real backend and
// records the effort of each request. Codex sends model requests as
// websocket messages, so the proxy tunnels the upgrade itself, declines
// permessage-deflate to keep frames readable, and decodes client frames
// before forwarding them.
type effortProxy struct {
	baseURL  string
	upstream *url.URL

	mu   sync.Mutex
	reqs []modelRequest
}

func startEffortProxy(t *testing.T) *effortProxy {
	t.Helper()
	upstream, basePath := liveUpstream(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &effortProxy{baseURL: "http://" + ln.Addr().String() + basePath, upstream: upstream}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(upstream)
		r.Out.Host = upstream.Host
	}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			p.tunnelWebsocket(t, w, r)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("Content-Encoding") == "" {
			body, _ := io.ReadAll(r.Body)
			p.record(body)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		rp.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return p
}

// liveUpstream reads the signed-in account, over a connection that does
// not go through the proxy, to pick the backend codex would call.
func liveUpstream(t *testing.T) (*url.URL, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := New().Connect(ctx)
	if err != nil {
		t.Skipf("codex not runnable: %v", err)
	}
	defer conn.Close()
	acct, err := conn.Account(ctx)
	if errors.Is(err, ErrNotSignedIn) {
		t.Skip("codex is not signed in")
	}
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	switch acct.Type {
	case schema.AccountTypeChatGPT:
		return &url.URL{Scheme: "https", Host: "chatgpt.com"}, "/backend-api/codex"
	case schema.AccountTypeAPIKey:
		return &url.URL{Scheme: "https", Host: "api.openai.com"}, "/v1"
	default:
		t.Skipf("no proxy route for account type %q", acct.Type)
		return nil, ""
	}
}

func (p *effortProxy) record(body []byte) {
	var m struct {
		Model     string `json:"model"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &m) != nil || m.Model == "" {
		return
	}
	req := modelRequest{effort: m.Reasoning.Effort}
	for i := len(m.Input) - 1; i >= 0 && req.lastUser == ""; i-- {
		if m.Input[i].Role != "user" {
			continue
		}
		for _, c := range m.Input[i].Content {
			if !strings.HasPrefix(c.Text, "<") {
				req.lastUser = c.Text
			}
		}
	}
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
}

// requireEffort asserts every model request made for tag carried want.
func (p *effortProxy) requireEffort(t *testing.T, tag, want string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var got []string
	for _, r := range p.reqs {
		if strings.Contains(r.lastUser, tag) {
			got = append(got, r.effort)
		}
	}
	if len(got) == 0 {
		t.Fatalf("%s: no model request seen by the proxy (%d total)", tag, len(p.reqs))
	}
	for _, e := range got {
		if e != want {
			t.Errorf("%s: model request efforts = %v, want all %q", tag, got, want)
			return
		}
	}
}

func (p *effortProxy) tunnelWebsocket(t *testing.T, w http.ResponseWriter, r *http.Request) {
	up, err := tls.Dial("tcp", p.upstream.Host+":443", &tls.Config{
		ServerName: p.upstream.Host, NextProtos: []string{"http/1.1"},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer up.Close()
	var handshake bytes.Buffer
	fmt.Fprintf(&handshake, "GET %s HTTP/1.1\r\nHost: %s\r\n", r.URL.RequestURI(), p.upstream.Host)
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Sec-Websocket-Extensions") {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&handshake, "%s: %s\r\n", k, v)
		}
	}
	handshake.WriteString("\r\n")
	if _, err := up.Write(handshake.Bytes()); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	client, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	go io.Copy(client, up)

	var message []byte
	for {
		frame, payload, fin, op, err := readClientFrame(buf.Reader)
		if err != nil {
			return
		}
		switch op {
		case 0x1, 0x2:
			message = append(message[:0], payload...)
		case 0x0:
			message = append(message, payload...)
		}
		if fin && op <= 0x2 {
			p.record(message)
		}
		if _, err := up.Write(frame); err != nil {
			return
		}
	}
}

// readClientFrame reads one masked client frame, returning its raw bytes
// for forwarding and its unmasked payload for inspection.
func readClientFrame(r io.Reader) (raw, payload []byte, fin bool, op byte, err error) {
	hdr := make([]byte, 2)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return
	}
	raw = append(raw, hdr...)
	fin, op = hdr[0]&0x80 != 0, hdr[0]&0x0f
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(r, ext); err != nil {
			return
		}
		raw, n = append(raw, ext...), uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(r, ext); err != nil {
			return
		}
		raw, n = append(raw, ext...), binary.BigEndian.Uint64(ext)
	}
	var key []byte
	if hdr[1]&0x80 != 0 {
		key = make([]byte, 4)
		if _, err = io.ReadFull(r, key); err != nil {
			return
		}
		raw = append(raw, key...)
	}
	body := make([]byte, n)
	if _, err = io.ReadFull(r, body); err != nil {
		return
	}
	raw = append(raw, body...)
	payload = body
	if key != nil {
		payload = make([]byte, n)
		for i := range body {
			payload[i] = body[i] ^ key[i%4]
		}
	}
	return
}
