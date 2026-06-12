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
	"sync/atomic"
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

	// wait reaper: classifies exit, stores typed error, broadcasts a
	// terminal ProcessExitEvent to every subscriber, then closes their
	// channels so consumers reading from sub channels see the exit
	// before EOF. The rpc connection is closed last to keep stderr tail
	// capture and subscriber notification ordered.
	conn.waitDone = make(chan struct{})
	go conn.reapProcess()

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

	threadsMu sync.Mutex
	threads   map[string]*Thread

	// cmdOutput reconstructs commandExecution output from streamed
	// deltas when WithAccumulatedOutput is set. Self-synchronized.
	cmdOutput cmdOutputAccumulator

	stderrDone chan struct{}
	stderrBuf  stderrRing

	waitDone chan struct{}
	waitErr  error
	exitErr  atomic.Pointer[ProcessExitError]

	initOnce      sync.Once
	closeOnce     sync.Once
	closeSubsOnce sync.Once
}

// Close terminates the underlying process and releases resources.
//
// Close is idempotent; multiple callers and goroutines can invoke it
// safely. The call blocks until either the reaper has finished (and
// therefore subscribers have all been closed cleanly) or the 5-second
// safety timeout expires.
func (c *Conn) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		var errs []error
		c.cancel()
		if c.proc.Stdin != nil {
			if err := c.proc.Stdin.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close stdin: %w", err))
			}
		}
		c.rpc.Close()
		select {
		case <-c.waitDone:
		case <-time.After(5 * time.Second):
			c.logger.Warn("codexcli: Close timed out waiting for reaper; leaving subs open")
			errs = append(errs, fmt.Errorf("codexcli: close timed out waiting for reaper"))
		}
		closeErr = errors.Join(errs...)
	})
	return closeErr
}

// ExitError returns the typed exit error once the subprocess has died,
// or nil while still running. Callers can poll this from any goroutine.
func (c *Conn) ExitError() *ProcessExitError {
	return c.exitErr.Load()
}

// ProcessInfo returns a lightweight liveness snapshot for watchdogs.
func (c *Conn) ProcessInfo() ProcessInfo {
	ex := c.exitErr.Load()
	info := ProcessInfo{
		Running: ex == nil,
		Exit:    ex,
	}
	if c.rpc != nil {
		info.LastStdoutAt = c.rpc.LastReadAt()
	}
	return info
}

// Ping checks whether the connection still appears alive. Codex app-server
// does not currently expose a dedicated control round-trip, so this is a
// local liveness probe rather than a protocol ping.
func (c *Conn) Ping(ctx context.Context, timeout time.Duration) error {
	if err := c.checkExited(); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = 1 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		if err := c.checkExited(); err != nil {
			return err
		}
		return ErrClosed
	case <-timer.C:
		return c.checkExited()
	default:
		return nil
	}
}

// reapProcess runs in its own goroutine for the life of the Conn. It
// blocks on proc.Wait, classifies the exit, stores it on the Conn,
// emits a ProcessExitEvent to every active subscriber, then closes
// every subscription channel so consumers iterating the events channel
// see a clean shutdown sequence even when the process dies mid-turn.
func (c *Conn) reapProcess() {
	c.waitErr = c.proc.Wait()

	// Wait for stderr drain to finish so the captured tail reflects
	// everything the subprocess printed before terminating. drainStderr
	// closes c.stderrDone when it sees EOF.
	select {
	case <-c.stderrDone:
	case <-time.After(stderrDrainGracePeriod):
	}

	exit := classifyExit(c.waitErr, c.ctx.Err(), c.stderrBuf.String())
	c.exitErr.Store(exit)

	// Wake up anyone blocked on rpc.Request — closing rpc fails their
	// pending response channels. Done before subscriber teardown so a
	// turn goroutine that races with reap sees the typed error rather
	// than hanging on a future read.
	c.rpc.Close()

	// Wait for the rpc read loop to exit before touching subscriber
	// channels. The read loop owns sends into sub via dispatchNotification;
	// closing subs while it's still running races on the channel state.
	<-c.rpc.Done()

	close(c.waitDone)

	// Deliver the final event then close subs. This is the contract
	// callers rely on: the last event before sub close is the typed
	// exit, so a `range sub` loop can promote it to a stream-level error.
	c.deliverExitAndCloseSubs(exit)
}

func (c *Conn) deliverExitAndCloseSubs(exit *ProcessExitError) {
	c.closeSubsOnce.Do(func() {
		c.subsMu.Lock()
		defer c.subsMu.Unlock()
		for tid, ch := range c.subs {
			if exit != nil {
				select {
				case ch <- &ProcessExitEvent{Err: exit}:
				default:
				}
			}
			close(ch)
			delete(c.subs, tid)
		}
	})
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
	if err := c.checkExited(); err != nil {
		return nil, err
	}
	params := c.options.buildThreadStartParams()
	var resp schema.ThreadStartResponse
	if err := c.rpc.Request(ctx, "thread/start", params, &resp); err != nil {
		return nil, c.promoteRPCError("thread/start", err)
	}
	t := &Thread{ID: resp.Thread.ID, conn: c, response: resp}
	c.registerThread(t)
	return t, nil
}

// ResumeThread issues a thread/resume request to rehydrate a previously
// persisted thread. On success the returned Thread is ready for
// StartTurn; the server reloads the conversation history from disk.
//
// Returns ErrThreadNotFound when the server reports the thread doesn't
// exist (deleted, wrong id, etc.), so callers can fall back to NewThread.
func (c *Conn) ResumeThread(ctx context.Context, threadID string, opts ...Option) (*Thread, error) {
	if err := c.checkExited(); err != nil {
		return nil, err
	}
	resolved := resolveOptions(c.options.callOpts(), opts)
	params := resolved.buildThreadResumeParams(threadID)
	var resp schema.ThreadResumeResponse
	if err := c.rpc.Request(ctx, "thread/resume", params, &resp); err != nil {
		if isThreadNotFoundError(err) {
			return nil, fmt.Errorf("thread/resume %s: %w", threadID, ErrThreadNotFound)
		}
		return nil, c.promoteRPCError("thread/resume", err)
	}
	t := &Thread{ID: resp.Thread.ID, conn: c, response: resp}
	c.registerThread(t)
	return t, nil
}

// isThreadNotFoundError checks if an RPC error indicates the thread
// couldn't be found, matching the known server error message patterns.
func isThreadNotFoundError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"not found", "missing thread", "no such thread",
		"unknown thread", "does not exist",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// ResumeCursor carries the minimum state needed to resume a thread in a
// future session. Consumers should persist this value and pass it to
// ResumeThread.
type ResumeCursor struct {
	ThreadID string `json:"threadId"`
}

// checkExited returns the typed ProcessExitError if the subprocess has
// already terminated, or nil otherwise.
func (c *Conn) checkExited() error {
	if ex := c.exitErr.Load(); ex != nil {
		return ex
	}
	return nil
}

// promoteRPCError replaces a generic ErrClosed with the typed exit
// error when one is available, so callers see why the process died
// instead of a bare "connection closed".
func (c *Conn) promoteRPCError(op string, err error) error {
	if errors.Is(err, ErrClosed) {
		if ex := c.exitErr.Load(); ex != nil {
			return fmt.Errorf("%s: %w", op, ex)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// Interrupt cancels an in-flight turn. Pass the empty string for turnID
// to interrupt whatever turn the server treats as active for this thread.
//
// The call returns once the server acknowledges with `{}`; a separate
// `turn/completed` notification with `status: "interrupted"` arrives on
// the event channel.
func (c *Conn) Interrupt(ctx context.Context, threadID, turnID string) error {
	return c.interrupt(ctx, threadID, turnID)
}

func (c *Conn) interrupt(ctx context.Context, threadID, turnID string) error {
	if err := c.checkExited(); err != nil {
		return err
	}
	params := schema.TurnInterruptParams{ThreadID: threadID, TurnID: turnID}
	var resp schema.TurnInterruptResponse
	if err := c.rpc.Request(ctx, "turn/interrupt", params, &resp); err != nil {
		return c.promoteRPCError("turn/interrupt", err)
	}
	return nil
}

func (c *Conn) registerThread(t *Thread) {
	c.threadsMu.Lock()
	if c.threads == nil {
		c.threads = map[string]*Thread{}
	}
	c.threads[t.ID] = t
	c.threadsMu.Unlock()
}

func (c *Conn) lookupThread(id string) *Thread {
	c.threadsMu.Lock()
	defer c.threadsMu.Unlock()
	return c.threads[id]
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

// broadcastEvent sends an event to every active subscriber. Used for
// connection-scoped notifications (rate limits, etc.) that aren't tied
// to a specific thread.
func (c *Conn) broadcastEvent(ev Event) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- ev:
		default:
		}
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
			if t := c.lookupThread(p.ThreadId); t != nil {
				t.setActiveTurn(p.Turn.ID)
			}
			c.deliver(p.ThreadId, &TurnStartedEvent{ThreadID: p.ThreadId, Turn: p.Turn})
		}
	case "turn/completed":
		var p schema.TurnCompletedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			if t := c.lookupThread(p.ThreadId); t != nil {
				t.setActiveTurn("")
			}
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
			if c.options.accumulateOutput {
				c.applyAccumulatedOutput(&p.Item)
			}
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
	case "item/commandExecution/outputDelta":
		var p schema.CommandExecutionOutputDeltaNotification
		if err := json.Unmarshal(params, &p); err == nil {
			if c.options.accumulateOutput {
				c.cmdOutput.append(p.ItemId, p.Delta)
			}
			c.deliver(p.ThreadId, &ContentDeltaEvent{
				Kind: ContentDeltaCommandOutput, ThreadID: p.ThreadId,
				TurnID: p.TurnId, ItemID: p.ItemId, Delta: p.Delta,
			})
		}
	case "item/fileChange/outputDelta":
		var p schema.FileChangeOutputDeltaNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ContentDeltaEvent{
				Kind: ContentDeltaFileChangeOutput, ThreadID: p.ThreadId,
				TurnID: p.TurnId, ItemID: p.ItemId, Delta: p.Delta,
			})
		}
	case "item/reasoning/textDelta":
		var p schema.ReasoningTextDeltaNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ContentDeltaEvent{
				Kind: ContentDeltaReasoningText, ThreadID: p.ThreadId,
				TurnID: p.TurnId, ItemID: p.ItemId, Delta: p.Delta,
				ContentIndex: p.ContentIndex,
			})
		}
	case "item/reasoning/summaryTextDelta":
		var p schema.ReasoningSummaryTextDeltaNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ContentDeltaEvent{
				Kind: ContentDeltaReasoningSummary, ThreadID: p.ThreadId,
				TurnID: p.TurnId, ItemID: p.ItemId, Delta: p.Delta,
				SummaryIndex: p.SummaryIndex,
			})
		}
	case "item/plan/delta":
		var p schema.PlanDeltaNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ContentDeltaEvent{
				Kind: ContentDeltaPlan, ThreadID: p.ThreadId,
				TurnID: p.TurnId, ItemID: p.ItemId, Delta: p.Delta,
			})
		}
	case "turn/diff/updated":
		var p schema.TurnDiffUpdatedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &TurnDiffUpdatedEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId, Diff: p.Diff,
			})
		}
	case "account/rateLimits/updated":
		var p schema.AccountRateLimitsUpdatedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.broadcastEvent(&RateLimitsUpdatedEvent{RateLimits: p.RateLimits})
		}
	case "thread/tokenUsage/updated":
		var p schema.ThreadTokenUsageUpdatedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &TokenUsageUpdatedEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId, TokenUsage: p.TokenUsage,
			})
		}
	case schema.MethodSkillsChanged:
		// Empty payload; no decode needed. Broadcast as an invalidation
		// signal so consumers caching skills/list output can refresh.
		c.broadcastEvent(&SkillsChangedEvent{})
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
		c.broadcastEvent(&UnknownEvent{Method: method, Params: params})
	}
}

// dispatchServerRequest handles inbound JSON-RPC requests from the
// server (approvals, permission prompts, dynamic tool calls, etc.).
//
// Approval routing:
//   - Approval methods (see schema.Method*Approval consts) decode into a
//     typed ApprovalRequest and dispatch to options.approvalFunc. If no
//     handler is registered the request auto-declines with the kind's
//     "decline" decision so the agent's turn can proceed without the
//     blocked action.
//   - Unknown methods get a JSON-RPC method-not-found response. A copy
//     of the raw payload is broadcast to every subscriber as an
//     UnknownEvent so test fixtures and consumers can spot drift.
//
// Runs in its own goroutine per request so concurrent approvals don't
// serialize the rpc read loop.
func (c *Conn) dispatchServerRequest(method string, id json.RawMessage, params json.RawMessage) {
	go c.handleServerRequest(method, id, params)
}

func (c *Conn) handleServerRequest(method string, id json.RawMessage, params json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("codexcli: server request handler panicked: %v", r)
			c.logger.Error("codexcli: panic in server request handler", "method", method, "err", err)
			c.broadcastEvent(&ErrorEvent{Err: err, Fatal: false})
			_ = c.rpc.RespondError(id, -32000, err.Error())
		}
	}()

	req, err := decodeApprovalRequest(method, params)
	if err != nil {
		c.logger.Error("codexcli: failed to decode approval params",
			"method", method, "err", err)
		_ = c.rpc.RespondError(id, -32602, "invalid approval params: "+err.Error())
		return
	}
	if req != nil {
		c.routeApproval(method, id, req)
		return
	}

	// Non-approval server request — broadcast for observability and
	// answer via the generic handler when configured.
	c.broadcastEvent(&UnknownServerRequestEvent{Method: method, Params: params})

	if fn := c.options.serverRequestFunc; fn != nil {
		ctx, cancel := context.WithCancel(c.ctx)
		defer cancel()
		result, err := fn(ctx, ServerRequest{Method: method, Params: params})
		if err != nil {
			_ = c.rpc.RespondError(id, -32000, "server request handler error: "+err.Error())
			return
		}
		if len(result) == 0 {
			result = json.RawMessage(`{}`)
		}
		_ = c.rpc.RespondRaw(id, result)
		return
	}
	_ = c.rpc.RespondError(id, -32601, "codexcli: server request method not implemented: "+method)
}

func (c *Conn) routeApproval(method string, id json.RawMessage, req ApprovalRequest) {
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("codexcli: approval handler panicked: %v", r)
			c.logger.Error("codexcli: panic in approval handler", "method", method, "err", err)
			if tid := req.ThreadID(); tid != "" {
				c.deliver(tid, &ErrorEvent{Err: err, Fatal: false})
			}
			_ = c.rpc.RespondError(id, -32000, err.Error())
		}
	}()

	if tid := req.ThreadID(); tid != "" {
		c.deliver(tid, &ApprovalRequestEvent{Request: req})
	}

	fn := c.options.approvalFunc
	if fn == nil {
		fn = DenyAll
	}

	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()

	decision, err := fn(ctx, req)
	if err != nil {
		c.logger.Warn("codexcli: approval handler returned error",
			"method", method, "err", err)
		_ = c.rpc.RespondError(id, -32000, "approval handler error: "+err.Error())
		return
	}
	if decision == nil {
		decision = Decline{}
	}
	body, err := decision.marshalDecision(method)
	if err != nil {
		c.logger.Error("codexcli: approval decision marshal failed",
			"method", method, "err", err)
		_ = c.rpc.RespondError(id, -32000, err.Error())
		return
	}
	if err := c.rpc.RespondRaw(id, body); err != nil {
		c.logger.Warn("codexcli: failed to send approval response", "err", err)
	}
}

func (c *Conn) drainStderr() {
	defer close(c.stderrDone)
	r := bufio.NewReader(c.proc.Stderr)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			s := strings.TrimRight(line, "\r\n")
			c.stderrBuf.Write(line)
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

// stderrRing accumulates a bounded tail of subprocess stderr so the
// exit classifier can attach diagnostic context to ProcessExitError.
// 4 KiB is enough to catch a Rust panic message without unbounded
// growth in long-lived sessions.
type stderrRing struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (r *stderrRing) Write(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	const cap = 4 * 1024
	if r.buf.Len()+len(s) <= cap {
		r.buf.WriteString(s)
		return
	}
	// Reset and re-seed with the new line; trades older context for
	// fresher information on long runs.
	r.buf.Reset()
	if len(s) > cap {
		r.buf.WriteString(s[len(s)-cap:])
	} else {
		r.buf.WriteString(s)
	}
}

func (r *stderrRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}
