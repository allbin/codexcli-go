package codexcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// Client wraps a codex app-server executor with default options.
type Client struct {
	executor Executor
	defaults []Option
	logger   *slog.Logger
}

// New creates a client with the given default options. Pass
// WithBinaryPath to override the codex binary location.
func New(defaults ...Option) *Client {
	return NewClient(nil, defaults...)
}

// NewClient creates a client with explicit client-level options and
// per-call defaults.
func NewClient(clientOpts []ClientOption, defaults ...Option) *Client {
	resolved := resolveOptions(defaults, nil)
	exec := NewLocalExecutor()
	if resolved.binaryPath != "" {
		exec.BinaryPath = resolved.binaryPath
	}
	c := &Client{executor: exec, defaults: defaults}
	for _, o := range clientOpts {
		o(c)
	}
	return c
}

// NewWithExecutor creates a client backed by an arbitrary Executor.
// Use this for testing or to run codex against a remote host.
func NewWithExecutor(executor Executor, defaults ...Option) *Client {
	return &Client{executor: executor, defaults: defaults}
}

// WithLogger sets a structured logger for diagnostic output (rare on
// the happy path; populated on protocol errors and unknown methods).
func WithLogger(l *slog.Logger) ClientOption {
	return func(c *Client) { c.logger = l }
}

func (c *Client) log() *slog.Logger {
	if c.logger != nil {
		return c.logger
	}
	return slog.New(discardHandler{})
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }

// Connect starts the codex subprocess, performs the
// initialize/initialized handshake, and returns a Client-bound Conn
// holding the live JSON-RPC session.
//
// Conn is the lower-level surface: callers can dispatch any request,
// register notification handlers, and run multiple turns on multiple
// threads. Most callers should use Client.Run instead.
func (c *Client) Connect(ctx context.Context, opts ...Option) (*Conn, error) {
	resolved := resolveOptions(c.defaults, opts)

	procCtx, cancel := context.WithCancel(ctx)
	proc, err := c.executor.Start(procCtx, &StartConfig{
		Args:    []string{"--listen", "stdio://"},
		Env:     resolved.env,
		WorkDir: resolved.workDir,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start codex: %w", err)
	}

	conn := &Conn{
		proc:    proc,
		ctx:     procCtx,
		cancel:  cancel,
		options: resolved,
		logger:  c.log(),
		// notifications dispatched directly through the rpc; populated below
	}
	conn.notifyDispatch = func(method string, params json.RawMessage) {
		conn.dispatchNotification(method, params)
	}
	conn.requestDispatch = func(method string, id json.RawMessage, params json.RawMessage) {
		conn.dispatchServerRequest(method, id, params)
	}
	conn.rpc = newRPCConn(procCtx, proc.Stdout, proc.Stdin, conn.notifyDispatch, conn.requestDispatch)

	// stderr drain (best effort — surface lines to callback if set)
	conn.stderrDone = make(chan struct{})
	go conn.drainStderr()

	// wait reaper
	conn.waitDone = make(chan struct{})
	go func() {
		conn.waitErr = proc.Wait()
		close(conn.waitDone)
		conn.rpc.Close()
	}()

	if err := conn.handshake(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Run is the high-level convenience: connect, start a thread, dispatch
// one turn with the given prompt, and return a Stream of events. The
// underlying connection is closed once the stream ends.
func (c *Client) Run(ctx context.Context, prompt string, opts ...Option) (*Stream, error) {
	conn, err := c.Connect(ctx, opts...)
	if err != nil {
		return nil, err
	}
	thread, err := conn.NewThread(ctx)
	if err != nil {
		conn.Close()
		return nil, err
	}

	events := make(chan Event, 64)
	done := make(chan struct{})
	streamCtx, cancel := context.WithCancel(ctx)
	stream := newStream(events, done, func() {
		cancel()
		conn.Close()
	})

	resolved := resolveOptions(c.defaults, opts)
	events <- &StartEvent{ThreadID: thread.ID, Model: resolved.model, Cwd: resolved.cwd}

	// Subscribe to thread/turn notifications via the conn's dispatcher.
	sub := conn.subscribe(thread.ID)
	go func() {
		defer close(done)
		defer close(events)
		defer conn.unsubscribe(thread.ID)

		if _, err := thread.startTurn(streamCtx, prompt, opts...); err != nil {
			events <- &ErrorEvent{Err: fmt.Errorf("turn/start: %w", err), Fatal: true}
			return
		}

		for {
			select {
			case ev, ok := <-sub:
				if !ok {
					return
				}
				events <- ev
				if _, isCompleted := ev.(*TurnCompletedEvent); isCompleted {
					return
				}
				if e, ok := ev.(*ErrorEvent); ok && e.Fatal {
					return
				}
			case <-streamCtx.Done():
				return
			}
		}
	}()
	return stream, nil
}

// Conn is a live, post-handshake JSON-RPC session against `codex
// app-server`. Methods are concurrency-safe.
type Conn struct {
	proc    *Process
	rpc     *rpcConn
	options *options
	logger  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	notifyDispatch  func(string, json.RawMessage)
	requestDispatch func(string, json.RawMessage, json.RawMessage)

	subsMu sync.Mutex
	subs   map[string]chan Event // keyed by thread id

	stderrDone chan struct{}
	waitDone   chan struct{}
	waitErr    error

	initOnce  sync.Once
	closeOnce sync.Once
}

// Close terminates the underlying process and releases resources.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		if c.proc.Stdin != nil {
			_ = c.proc.Stdin.Close()
		}
		// Give the process a beat to exit cleanly before the context
		// kill takes effect.
		select {
		case <-c.waitDone:
		case <-time.After(2 * time.Second):
		}
		c.rpc.Close()
		c.subsMu.Lock()
		for tid, ch := range c.subs {
			close(ch)
			delete(c.subs, tid)
		}
		c.subsMu.Unlock()
	})
	return nil
}

func (c *Conn) handshake(ctx context.Context) error {
	var err error
	c.initOnce.Do(func() {
		params := schema.InitializeParams{
			ClientInfo:   c.options.clientInfo,
			Capabilities: c.options.caps,
		}
		var resp schema.InitializeResponse
		hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if e := c.rpc.Request(hctx, "initialize", params, &resp); e != nil {
			err = fmt.Errorf("initialize: %w", e)
			return
		}
		if e := c.rpc.Notify("initialized", struct{}{}); e != nil {
			err = fmt.Errorf("initialized: %w", e)
			return
		}
		c.logger.Debug("codexcli initialized",
			"userAgent", resp.UserAgent,
			"codexHome", resp.CodexHome,
			"platform", resp.PlatformOs)
	})
	return err
}

// NewThread issues a thread/start request using the client defaults.
// Per-call overrides are not currently exposed at this layer — set
// thread defaults via Options on New / Connect.
func (c *Conn) NewThread(ctx context.Context) (*Thread, error) {
	params := c.options.buildThreadStartParams()
	var resp schema.ThreadStartResponse
	if err := c.rpc.Request(ctx, "thread/start", params, &resp); err != nil {
		return nil, fmt.Errorf("thread/start: %w", err)
	}
	return &Thread{ID: resp.Thread.ID, conn: c, response: resp}, nil
}

// --- subscription bookkeeping ---

func (c *Conn) subscribe(threadID string) <-chan Event {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if c.subs == nil {
		c.subs = map[string]chan Event{}
	}
	ch := make(chan Event, 64)
	c.subs[threadID] = ch
	return ch
}

func (c *Conn) unsubscribe(threadID string) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if ch, ok := c.subs[threadID]; ok {
		close(ch)
		delete(c.subs, threadID)
	}
}

func (c *Conn) deliver(threadID string, ev Event) {
	c.subsMu.Lock()
	ch := c.subs[threadID]
	c.subsMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	default:
		// Drop on slow consumer; better to lose a notification than
		// stall the read loop and block every other thread.
		c.logger.Warn("codexcli: subscriber slow, dropping event", "threadID", threadID)
	}
}

// dispatchNotification routes inbound server notifications. Untyped
// shapes fall through to UnknownEvent so consumers can detect drift.
func (c *Conn) dispatchNotification(method string, params json.RawMessage) {
	switch method {
	case "thread/started":
		// Subscription happens after thread/start returns, so we don't
		// surface this event to consumers — they already know the
		// thread id from NewThread. Logged for diagnostics.
		var p schema.ThreadStartedNotification
		_ = json.Unmarshal(params, &p)
		c.logger.Debug("thread/started", "threadID", p.Thread.ID)
	case "turn/started":
		var p schema.TurnStartedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &TurnStartedEvent{ThreadID: p.ThreadId, Turn: p.Turn})
		}
	case "turn/completed":
		var p schema.TurnCompletedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &TurnCompletedEvent{ThreadID: p.ThreadId, Turn: p.Turn})
		}
	case "item/started":
		var p schema.ItemStartedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ItemStartedEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId, Item: p.Item, StartedAtMs: p.StartedAtMs,
			})
		}
	case "item/completed":
		var p schema.ItemCompletedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ItemCompletedEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId, Item: p.Item,
			})
		}
	case "item/agentMessage/delta":
		var p schema.AgentMessageDeltaNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &AgentMessageDeltaEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId, ItemID: p.ItemId, Delta: p.Delta,
			})
		}
	case "error":
		var p schema.ErrorNotification
		if err := json.Unmarshal(params, &p); err == nil {
			threadID := ""
			if p.ThreadId != nil {
				threadID = *p.ThreadId
			}
			c.deliver(threadID, &ErrorEvent{Err: wrapTurnError(&p.Error), Fatal: false})
		}
	default:
		// Broadcast unknown events to every subscriber so consumers
		// have a chance to log them. Cheap — there are usually 1-2.
		c.subsMu.Lock()
		for _, ch := range c.subs {
			select {
			case ch <- &UnknownEvent{Method: method, Params: params}:
			default:
			}
		}
		c.subsMu.Unlock()
	}
}

// dispatchServerRequest handles inbound JSON-RPC requests from the
// server (approvals, permission prompts, dynamic tool calls, etc.).
// This first pass auto-declines everything — approval/permission
// callbacks land in a follow-up.
func (c *Conn) dispatchServerRequest(method string, id json.RawMessage, params json.RawMessage) {
	c.logger.Warn("codexcli: server request not handled in v0 — auto-declining",
		"method", method, "params", string(params))
	switch method {
	case "execCommandApproval", "applyPatchApproval",
		"item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		_ = c.rpc.Respond(id, map[string]any{"decision": "decline"})
	default:
		_ = c.rpc.RespondError(id, -32601, "method not implemented by codexcli-go v0")
	}
}

func (c *Conn) drainStderr() {
	defer close(c.stderrDone)
	r := bufio.NewReader(c.proc.Stderr)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			s := strings.TrimRight(line, "\r\n")
			if cb := c.options.stderrCallback; cb != nil {
				cb(s)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.logger.Debug("stderr read error", "err", err)
			}
			return
		}
	}
}

// suppress unused import warning until exit-error wiring lands
var _ = errors.New
