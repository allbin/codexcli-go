package codexcli

import (
	"bufio"
	"bytes"
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
		defer conn.unsubscribe(thread.ID, sub)

		if _, err := thread.startTurn(streamCtx, prompt, opts...); err != nil {
			events <- &ErrorEvent{Err: fmt.Errorf("turn/start: %w", err), Fatal: true}
			return
		}

		for {
			select {
			case ev, ok := <-sub.out:
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
	subs   map[string]*subscription // keyed by thread id

	threadsMu sync.Mutex
	threads   map[string]*Thread

	// srvReqs holds the server requests a handler is still deciding, keyed
	// by request id; see trackServerRequest.
	srvReqMu sync.Mutex
	srvReqs  map[string]*serverRequestState

	// childReg tracks subagent threads; see subagent.go.
	childReg childRegistry

	// logins pairs account/login/completed with Login handles; see login.go.
	logins loginRegistry

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

// Done returns a channel that closes when the codex process has exited
// and been reaped, whether or not a turn is running. ExitError is non-nil
// once it is closed. Subscribed Streams receive their ProcessExitEvent
// shortly after.
func (c *Conn) Done() <-chan struct{} { return c.waitDone }

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

// pingMethod is the request Ping sends. thread/loaded/list reads the
// in-memory set of loaded threads: no disk, no network, no side effects.
// On codex 0.159.3 it answered in under 2ms while idle, mid-generation,
// with an approval pending, and during a 20s command that printed nothing.
const pingMethod = "thread/loaded/list"

// Ping sends a real request and reports whether codex answered within
// timeout (a zero timeout means 5s). Codex app-server has no ping method,
// so the probe is thread/loaded/list with limit 1.
//
// Any answer proves the request loop read stdin and wrote stdout, so an
// error response counts as alive too, which keeps a codex without the
// method working. Failure modes:
//
//   - ErrPingTimeout: the process is running but did not answer in time;
//   - the process exited: its ProcessExitError;
//   - the connection closed: ErrClosed;
//   - ctx ended first: ctx.Err().
//
// If the write itself blocks because codex stopped reading stdin, Ping
// still returns at the timeout, leaving a goroutine blocked in the write
// until the process exits or the connection closes.
func (c *Conn) Ping(ctx context.Context, timeout time.Duration) error {
	if err := c.checkExited(); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.rpc.Request(pctx, pingMethod, map[string]any{"limit": 1}, nil)
	}()
	var err error
	select {
	case err = <-done:
	case <-pctx.Done():
		err = pctx.Err()
	}
	var rerr *rpcError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rerr) && rerr.cause == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.DeadlineExceeded):
		if ex := c.checkExited(); ex != nil {
			return ex
		}
		return fmt.Errorf("codexcli: no answer to %s within %s: %w", pingMethod, timeout, ErrPingTimeout)
	}
	// The transport failed: stdout hit EOF or the connection closed. If
	// the process is exiting, the reaper is about to classify it; report
	// that rather than a bare EOF.
	select {
	case <-c.waitDone:
		if ex := c.checkExited(); ex != nil {
			return ex
		}
	case <-pctx.Done():
	}
	return c.promoteRPCError("ping", err)
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

// deliverExitAndCloseSubs queues the exit as each subscriber's last event
// and closes it once everything before it has been read.
func (c *Conn) deliverExitAndCloseSubs(exit *ProcessExitError) {
	c.closeSubsOnce.Do(func() {
		c.subsMu.Lock()
		defer c.subsMu.Unlock()
		for tid, sub := range c.subs {
			if exit != nil {
				sub.push(&ProcessExitEvent{Err: exit})
			}
			sub.finish()
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

// DeleteThread deletes a persisted thread from CODEX_HOME over
// thread/delete. A thread codex has no record of, including one already
// deleted, returns ErrThreadNotFound. Codex's schema announces a delete
// with thread/deleted, which arrives as ThreadDeletedEvent on an open
// Stream.
//
// Observed live on codex 0.160.0, three runs (delete_live_test.go):
//   - Removed: the rollout file sessions/YYYY/MM/DD/rollout-*-<id>.jsonl,
//     the thread's thread_items, thread_turns and
//     thread_history_projection_state rows in thread_history_1.sqlite, and
//     its threads row in state_5.sqlite. No other table or file in
//     CODEX_HOME mentions the id afterwards.
//   - Kept: rows in logs_2.sqlite's logs table, codex's own tracing log,
//     which carry the id in span metadata. A shell command's output, kept
//     in thread_items before the delete, was in no table afterwards.
//   - Subagents: deleting a thread deletes the subagent threads it spawned,
//     rollouts and rows alike, whether or not this connection has the
//     threads loaded. Deleting a child afterwards returns
//     ErrThreadNotFound, so a caller that deletes each of Children() as
//     well can treat that as done. Take Children() before deleting: the
//     deleted thread's subagents leave the connection's tree with it.
//   - A thread loaded on this connection is shut down first. Deleting a
//     thread with a turn running was not tested.
func (c *Conn) DeleteThread(ctx context.Context, threadID string) error {
	if err := c.checkExited(); err != nil {
		return err
	}
	params := schema.ThreadDeleteParams{ThreadID: threadID}
	if err := c.rpc.Request(ctx, schema.MethodThreadDelete, params, nil); err != nil {
		if isThreadNotFoundError(err) {
			return fmt.Errorf("%s %s: %w", schema.MethodThreadDelete, threadID, ErrThreadNotFound)
		}
		return c.promoteRPCError(schema.MethodThreadDelete, err)
	}
	c.forgetDeleted(threadID)
	return nil
}

// isThreadNotFoundError checks if an RPC error indicates the thread
// couldn't be found, matching the known server error message patterns.
func isThreadNotFoundError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"not found", "missing thread", "no such thread",
		"unknown thread", "does not exist",
		// codex 0.159.3: "no rollout found for thread id <uuid>"
		"no rollout found",
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
	c.detachThread(t.ID)
}

func (c *Conn) lookupThread(id string) *Thread {
	c.threadsMu.Lock()
	defer c.threadsMu.Unlock()
	return c.threads[id]
}

// --- subscription bookkeeping ---

// subscribe makes a new subscription the thread's only subscriber. A
// subscription it replaces is finished: its Stream gets what was already
// queued and then ends, rather than waiting for events that now go
// elsewhere. The caller reads sub.out and must unsubscribe when it stops.
func (c *Conn) subscribe(threadID string) *subscription {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if c.subs == nil {
		c.subs = map[string]*subscription{}
	}
	if old, ok := c.subs[threadID]; ok {
		old.finish()
	}
	sub := newSubscription()
	c.subs[threadID] = sub
	return sub
}

// unsubscribe is the reader leaving: sub drops anything still queued and
// its pump exits, whether or not sub is still the thread's subscriber. It
// is removed from the thread only while it is the current one, so a
// replaced Stream ending does not cut off its successor.
func (c *Conn) unsubscribe(threadID string, sub *subscription) {
	sub.cancel()
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if c.subs[threadID] == sub {
		delete(c.subs, threadID)
	}
}

// deliver queues ev for the thread's subscriber. It never blocks the read
// loop and never drops an event for a subscribed thread. An event for a
// subagent thread goes to its root thread's subscriber as a
// ChildThreadEvent (routeUnsubscribed).
func (c *Conn) deliver(threadID string, ev Event) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if sub := c.subs[threadID]; sub != nil {
		sub.push(ev)
		return
	}
	c.routeUnsubscribed(threadID, ev)
}

// broadcastEvent queues an event for every active subscriber. Used for
// connection-scoped notifications (rate limits, etc.) that aren't tied
// to a specific thread.
func (c *Conn) broadcastEvent(ev Event) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for _, sub := range c.subs {
		sub.push(ev)
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
			} else {
				c.noteForeignTurn(p.ThreadId, p.Turn.ID, true)
			}
			c.deliver(p.ThreadId, &TurnStartedEvent{ThreadID: p.ThreadId, Turn: p.Turn})
		}
	case "turn/completed":
		var p schema.TurnCompletedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			if t := c.lookupThread(p.ThreadId); t != nil {
				t.completedTurn(p.Turn.ID)
			} else {
				c.noteForeignTurn(p.ThreadId, p.Turn.ID, false)
			}
			// A request the turn was waiting on cannot be answered once
			// the turn is over; codex follows with serverRequest/resolved.
			// A request whose turn is unknown ("") can only be the
			// thread's one running turn's.
			c.withdrawServerRequests(func(_ string, st *serverRequestState) bool {
				return st.threadID == p.ThreadId && (st.turnID == "" || st.turnID == p.Turn.ID)
			})
			c.deliver(p.ThreadId, &TurnCompletedEvent{ThreadID: p.ThreadId, Turn: p.Turn})
		}
	case "item/started":
		var p schema.ItemStartedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ItemStartedEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId, Item: p.Item, StartedAtMs: p.StartedAtMs,
			})
			if p.Item.Type == schema.ItemTypeSubAgentActivity {
				c.noteSubAgent(p.ThreadId, p.TurnId, &p.Item)
			}
		}
	case "item/completed":
		var p schema.ItemCompletedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			if c.options.accumulateOutput {
				c.applyAccumulatedOutput(&p.Item)
			}
			c.deliver(p.ThreadId, &ItemCompletedEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId, Item: p.Item,
				CompletedAtMs: p.CompletedAtMs,
			})
			if p.Item.Type == schema.ItemTypeSubAgentActivity {
				c.noteSubAgent(p.ThreadId, p.TurnId, &p.Item)
			}
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
	case schema.MethodAccountLoginCompleted:
		var p schema.AccountLoginCompletedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.logins.complete(p)
			ev := &AccountLoginCompletedEvent{Success: p.Success}
			if p.LoginID != nil {
				ev.LoginID = *p.LoginID
			}
			if p.Error != nil {
				ev.Error = *p.Error
			}
			c.broadcastEvent(ev)
		}
	case schema.MethodAccountUpdated:
		var p schema.AccountUpdatedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			ev := &AccountUpdatedEvent{}
			if p.AuthMode != nil {
				ev.AuthMode = *p.AuthMode
			}
			if p.PlanType != nil {
				ev.PlanType = *p.PlanType
			}
			c.broadcastEvent(ev)
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
	case schema.MethodThreadStatusChanged:
		var p schema.ThreadStatusChangedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ThreadStatusChangedEvent{
				ThreadID: p.ThreadId, Status: p.StatusType(), StatusRaw: p.Status,
			})
		}
	case schema.MethodThreadSettingsUpdated:
		var p schema.ThreadSettingsUpdatedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			// Decode only the promoted fields, so upstream reshaping a field
			// this event does not type (sandbox, collaboration mode) cannot
			// drop the notification.
			var s struct {
				Model         string  `json:"model"`
				ModelProvider string  `json:"modelProvider"`
				Effort        *string `json:"effort"`
				Summary       *string `json:"summary"`
				ServiceTier   *string `json:"serviceTier"`
			}
			_ = json.Unmarshal(p.ThreadSettings, &s)
			ev := &ThreadSettingsUpdatedEvent{
				ThreadID: p.ThreadId, Model: s.Model, ModelProvider: s.ModelProvider,
				SettingsRaw: p.ThreadSettings,
			}
			if s.Effort != nil {
				ev.Effort = *s.Effort
			}
			if s.Summary != nil {
				ev.Summary = *s.Summary
			}
			if s.ServiceTier != nil {
				ev.ServiceTier = *s.ServiceTier
			}
			c.deliver(p.ThreadId, ev)
		}
	case schema.MethodTurnPlanUpdated:
		var p schema.TurnPlanUpdatedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			ev := &TurnPlanUpdatedEvent{ThreadID: p.ThreadId, TurnID: p.TurnId, Plan: p.Plan}
			if p.Explanation != nil {
				ev.Explanation = *p.Explanation
			}
			c.deliver(p.ThreadId, ev)
		}
	case schema.MethodFileChangePatchUpdated:
		var p schema.FileChangePatchUpdatedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &FileChangePatchUpdatedEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId, ItemID: p.ItemId, Changes: p.Changes,
			})
		}
	case schema.MethodReasoningSummaryPartAdded:
		var p schema.ReasoningSummaryPartAddedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ReasoningSummaryPartAddedEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId, ItemID: p.ItemId,
				SummaryIndex: p.SummaryIndex,
			})
		}
	case schema.MethodThreadCompacted:
		var p schema.ContextCompactedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ContextCompactedEvent{ThreadID: p.ThreadId, TurnID: p.TurnId})
		}
	case schema.MethodModelRerouted:
		var p schema.ModelReroutedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadId, &ModelReroutedEvent{
				ThreadID: p.ThreadId, TurnID: p.TurnId,
				FromModel: p.FromModel, ToModel: p.ToModel, ReasonRaw: p.Reason,
			})
		}
	case schema.MethodMcpServerStatusUpdated:
		var p schema.McpServerStatusUpdatedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			ev := &McpServerStatusEvent{Name: p.Name, Status: p.Status}
			if p.ThreadId != nil {
				ev.ThreadID = *p.ThreadId
			}
			if p.Error != nil {
				ev.Err = *p.Error
			}
			// Server startup is not thread-scoped in practice — codex
			// reports the same server per thread — so broadcast rather
			// than drop it when ThreadId is absent.
			c.broadcastEvent(ev)
		}
	case schema.MethodWarning, schema.MethodGuardianWarning:
		var p schema.WarningNotification
		if err := json.Unmarshal(params, &p); err == nil {
			ev := &WarningEvent{Message: p.Message, Guardian: method == schema.MethodGuardianWarning}
			if p.ThreadId != nil {
				ev.ThreadID = *p.ThreadId
			}
			if ev.ThreadID != "" {
				c.deliver(ev.ThreadID, ev)
			} else {
				c.broadcastEvent(ev)
			}
		}
	case schema.MethodConfigWarning:
		var p schema.ConfigWarningNotification
		if err := json.Unmarshal(params, &p); err == nil {
			ev := &ConfigWarningEvent{Summary: p.Summary}
			if p.Details != nil {
				ev.Details = *p.Details
			}
			if p.Path != nil {
				ev.Path = *p.Path
			}
			c.logger.Warn("codexcli: codex config warning",
				"summary", ev.Summary, "path", ev.Path, "details", ev.Details)
			c.broadcastEvent(ev)
		}
	case schema.MethodDeprecationNotice:
		var p schema.DeprecationNoticeNotification
		if err := json.Unmarshal(params, &p); err == nil {
			ev := &DeprecationNoticeEvent{Summary: p.Summary}
			if p.Details != nil {
				ev.Details = *p.Details
			}
			c.logger.Warn("codexcli: codex deprecation notice",
				"summary", ev.Summary, "details", ev.Details)
			c.broadcastEvent(ev)
		}
	case schema.MethodThreadNameUpdated:
		var p schema.ThreadNameUpdatedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			ev := &ThreadNameUpdatedEvent{ThreadID: p.ThreadID}
			if p.ThreadName != nil {
				ev.Name = *p.ThreadName
			}
			c.deliver(p.ThreadID, ev)
		}
	case schema.MethodThreadDeleted:
		var p schema.ThreadDeletedNotification
		if err := json.Unmarshal(params, &p); err == nil {
			c.deliver(p.ThreadID, &ThreadDeletedEvent{ThreadID: p.ThreadID})
		}
	case schema.MethodServerRequestResolved:
		var p schema.ServerRequestResolvedNotification
		if err := json.Unmarshal(params, &p); err == nil && len(p.RequestID) > 0 {
			key := requestKey(p.RequestID)
			c.withdrawServerRequests(func(k string, _ *serverRequestState) bool { return k == key })
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
	// Decode and register on the read loop, before anything after this
	// request is read: a serverRequest/resolved right behind it must find
	// it. The handler runs on its own goroutine.
	req, decodeErr := decodeApprovalRequest(method, params)
	threadID, turnID := serverRequestScope(method, req, params)
	ctx, st := c.trackServerRequest(id, threadID, turnID)
	go c.handleServerRequest(ctx, st, method, id, params, req, decodeErr)
}

// serverRequestScope returns the thread and turn a server request belongs
// to. Legacy v1 approvals carry a call id, not a turn id, so their turn is
// unknown ("").
func serverRequestScope(method string, req ApprovalRequest, params json.RawMessage) (threadID, turnID string) {
	switch {
	case method == schema.MethodExecCommandApproval || method == schema.MethodApplyPatchApproval:
		if req != nil {
			return req.ThreadID(), ""
		}
	case req != nil:
		return req.ThreadID(), req.TurnID()
	}
	var scope struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &scope)
	return scope.ThreadID, scope.TurnID
}

func (c *Conn) handleServerRequest(ctx context.Context, st *serverRequestState, method string, id, params json.RawMessage, req ApprovalRequest, decodeErr error) {
	defer st.cancel()
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("codexcli: server request handler panicked: %v", r)
			c.logger.Error("codexcli: panic in server request handler", "method", method, "err", err)
			if tid := st.threadID; tid != "" {
				c.deliver(tid, &ErrorEvent{Err: err, Fatal: false})
			} else {
				c.broadcastEvent(&ErrorEvent{Err: err, Fatal: false})
			}
			if c.claimServerRequest(id, st) {
				_ = c.rpc.RespondError(id, -32000, err.Error())
			}
		}
	}()

	if decodeErr != nil {
		c.logger.Error("codexcli: failed to decode approval params",
			"method", method, "err", decodeErr)
		if c.claimServerRequest(id, st) {
			_ = c.rpc.RespondError(id, -32602, "invalid approval params: "+decodeErr.Error())
		}
		return
	}
	if req != nil {
		c.routeApproval(ctx, st, method, id, req)
		return
	}

	// Non-approval server request — broadcast for observability and
	// answer via the generic handler when configured.
	c.broadcastEvent(&UnknownServerRequestEvent{Method: method, Params: params})

	fn := c.options.serverRequestFunc
	if fn == nil {
		if c.claimServerRequest(id, st) {
			_ = c.rpc.RespondError(id, -32601, "codexcli: server request method not implemented: "+method)
		}
		return
	}
	result, err := fn(ctx, ServerRequest{Method: method, Params: params})
	if !c.claimServerRequest(id, st) {
		c.logger.Debug("codexcli: server request withdrawn by codex; not answering", "method", method)
		return
	}
	if err != nil {
		_ = c.rpc.RespondError(id, -32000, "server request handler error: "+err.Error())
		return
	}
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	_ = c.rpc.RespondRaw(id, result)
}

func (c *Conn) routeApproval(ctx context.Context, st *serverRequestState, method string, id json.RawMessage, req ApprovalRequest) {
	if tid := req.ThreadID(); tid != "" {
		c.deliver(tid, &ApprovalRequestEvent{Request: req})
	}

	fn := c.options.approvalFunc
	if fn == nil {
		fn = DenyAll
	}

	decision, err := fn(ctx, req)
	if !c.claimServerRequest(id, st) {
		c.logger.Debug("codexcli: approval withdrawn by codex; not answering", "method", method)
		return
	}
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

// serverRequestState is a server request nobody has answered or withdrawn.
// It lives in Conn.srvReqs until one side claims it: the handler to send
// its answer (claimServerRequest), or codex by withdrawing it
// (withdrawServerRequests). Exactly one wins, under srvReqMu.
type serverRequestState struct {
	threadID, turnID string
	cancel           context.CancelFunc
}

// requestKey normalises a JSON-RPC id so the id on a request and the
// requestId on serverRequest/resolved compare equal: 7 and "7" stay
// distinct, whitespace does not matter.
func requestKey(id json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, id); err != nil {
		return string(id)
	}
	return buf.String()
}

// trackServerRequest gives a server request its own context, cancelled
// when codex withdraws the request, when the handler finishes, or when the
// connection ends.
func (c *Conn) trackServerRequest(id json.RawMessage, threadID, turnID string) (context.Context, *serverRequestState) {
	ctx, cancel := context.WithCancel(c.ctx)
	st := &serverRequestState{threadID: threadID, turnID: turnID, cancel: cancel}
	c.srvReqMu.Lock()
	if c.srvReqs == nil {
		c.srvReqs = map[string]*serverRequestState{}
	}
	c.srvReqs[requestKey(id)] = st
	c.srvReqMu.Unlock()
	return ctx, st
}

// claimServerRequest reports whether the handler may still answer: true
// at most once, and never after codex withdrew the request.
func (c *Conn) claimServerRequest(id json.RawMessage, st *serverRequestState) bool {
	c.srvReqMu.Lock()
	defer c.srvReqMu.Unlock()
	key := requestKey(id)
	if c.srvReqs[key] != st {
		return false
	}
	delete(c.srvReqs, key)
	return true
}

// withdrawServerRequests takes back every pending request match accepts,
// so its answer is never sent, and cancels its handler's context.
func (c *Conn) withdrawServerRequests(match func(key string, st *serverRequestState) bool) {
	c.srvReqMu.Lock()
	var hit []*serverRequestState
	for key, st := range c.srvReqs {
		if match(key, st) {
			hit = append(hit, st)
			delete(c.srvReqs, key)
		}
	}
	c.srvReqMu.Unlock()
	for _, st := range hit {
		st.cancel()
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
