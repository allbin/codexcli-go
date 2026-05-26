package codexcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// rpcMessage is the on-wire shape of every JSON-RPC frame the codex
// app-server reads or writes. The transport intentionally omits the
// "jsonrpc":"2.0" header (matching MCP); we follow the same convention.
//
// A message is a request when both Method and ID are set, a notification
// when only Method is set, and a response when only ID + (Result|Error)
// are set.
type rpcMessage struct {
	ID     *json.RawMessage `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	cause   error           // original error, not serialized
}

func (e *rpcError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("codex rpc error %d: %s (data: %s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message)
}

func (e *rpcError) Unwrap() error { return e.cause }

// pendingResponse holds the channel a caller is blocked on waiting for
// a server response keyed by request id.
type pendingResponse struct {
	resultCh chan rpcMessage
}

// rpcConn is the framed JSON-RPC transport over a duplex stream pair.
//
// The reader runs in its own goroutine. Notifications go to onNotify,
// server-initiated requests go to onRequest. Responses correlate against
// pending requests by id.
//
// Lifecycle: Close cancels the conn context, which causes the writer to
// drop, the reader to drain stdout to EOF, and any in-flight callers
// blocked on Request to receive ErrClosed.
type rpcConn struct {
	w   io.Writer
	enc *json.Encoder
	r   *bufio.Reader

	writeMu sync.Mutex

	nextID  atomic.Int64
	pending sync.Map // id (string) -> *pendingResponse

	onNotify  func(method string, params json.RawMessage)
	onRequest func(method string, id json.RawMessage, params json.RawMessage)

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	closed    atomic.Bool
	readErr   atomic.Value // error
	lastRead  atomic.Int64 // unix nanos of most recent stdout frame
	doneCh    chan struct{}
}

// ErrClosed signals that the RPC connection was closed (process exit,
// explicit Close, or fatal read error) before a pending operation could
// complete.
var ErrClosed = errors.New("codexcli: rpc connection closed")

func newRPCConn(
	parent context.Context,
	r io.Reader,
	w io.Writer,
	onNotify func(method string, params json.RawMessage),
	onRequest func(method string, id json.RawMessage, params json.RawMessage),
) *rpcConn {
	ctx, cancel := context.WithCancel(parent)
	c := &rpcConn{
		w:         w,
		enc:       json.NewEncoder(w),
		r:         bufio.NewReaderSize(r, 1<<16),
		onNotify:  onNotify,
		onRequest: onRequest,
		ctx:       ctx,
		cancel:    cancel,
		doneCh:    make(chan struct{}),
	}
	c.enc.SetEscapeHTML(false)
	go c.readLoop()
	return c
}

func (c *rpcConn) readLoop() {
	defer close(c.doneCh)
	defer c.cancel()
	for {
		line, err := c.r.ReadBytes('\n')
		if len(line) > 0 {
			c.lastRead.Store(time.Now().UnixNano())
			c.dispatch(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.readErr.Store(err)
			}
			c.failPending(err)
			return
		}
		if c.closed.Load() {
			c.failPending(ErrClosed)
			return
		}
	}
}

func (c *rpcConn) dispatch(line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		// Drop unparseable frames silently — the read loop is the wrong
		// place to surface stream corruption. Surface via stderr capture.
		return
	}
	switch {
	case msg.Method != "" && msg.ID != nil:
		// Server-initiated request.
		if c.onRequest != nil {
			c.onRequest(msg.Method, *msg.ID, msg.Params)
		}
	case msg.Method != "":
		// Notification.
		if c.onNotify != nil {
			c.onNotify(msg.Method, msg.Params)
		}
	case msg.ID != nil:
		// Response.
		id := string(*msg.ID)
		v, ok := c.pending.LoadAndDelete(id)
		if !ok {
			return
		}
		p := v.(*pendingResponse)
		select {
		case p.resultCh <- msg:
		default:
		}
	}
}

func (c *rpcConn) failPending(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.pending.Range(func(k, v any) bool {
		p := v.(*pendingResponse)
		select {
		case p.resultCh <- rpcMessage{Error: &rpcError{Code: -32000, Message: err.Error(), cause: err}}:
		default:
		}
		c.pending.Delete(k)
		return true
	})
}

// Notify sends a notification (no response expected).
func (c *rpcConn) Notify(method string, params any) error {
	frame := struct {
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{Method: method, Params: params}
	return c.writeFrame(frame)
}

// Request sends a request and blocks until either the response arrives,
// the connection closes, or ctx is canceled.
func (c *rpcConn) Request(ctx context.Context, method string, params any, out any) error {
	if c.closed.Load() {
		return ErrClosed
	}
	id := c.nextID.Add(1)
	idJSON, _ := json.Marshal(id)
	rawID := json.RawMessage(idJSON)

	pending := &pendingResponse{resultCh: make(chan rpcMessage, 1)}
	c.pending.Store(string(rawID), pending)

	frame := struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params any             `json:"params,omitempty"`
	}{ID: rawID, Method: method, Params: params}

	if err := c.writeFrame(frame); err != nil {
		c.pending.Delete(string(rawID))
		return err
	}

	select {
	case resp := <-pending.resultCh:
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil || len(resp.Result) == 0 {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	case <-ctx.Done():
		c.pending.Delete(string(rawID))
		return ctx.Err()
	case <-c.ctx.Done():
		c.pending.Delete(string(rawID))
		return ErrClosed
	}
}

// Respond sends a successful response to an inbound server-initiated request.
func (c *rpcConn) Respond(id json.RawMessage, result any) error {
	frame := struct {
		ID     json.RawMessage `json:"id"`
		Result any             `json:"result"`
	}{ID: id, Result: result}
	return c.writeFrame(frame)
}

// RespondRaw sends a successful response whose result is pre-encoded JSON.
// Used by approval routing where the decision shape is opaque to rpcConn.
func (c *rpcConn) RespondRaw(id json.RawMessage, result json.RawMessage) error {
	frame := struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}{ID: id, Result: result}
	return c.writeFrame(frame)
}

// RespondError sends an error response to an inbound server-initiated request.
func (c *rpcConn) RespondError(id json.RawMessage, code int, message string) error {
	frame := struct {
		ID    json.RawMessage `json:"id"`
		Error rpcError        `json:"error"`
	}{ID: id, Error: rpcError{Code: code, Message: message}}
	return c.writeFrame(frame)
}

func (c *rpcConn) writeFrame(frame any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	return c.enc.Encode(frame)
}

// Close stops the read loop. Pending requests receive ErrClosed.
func (c *rpcConn) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.cancel()
	})
}

// Done returns a channel that closes when the read loop exits.
func (c *rpcConn) Done() <-chan struct{} { return c.doneCh }

// ReadErr returns the last non-EOF read error, if any.
func (c *rpcConn) ReadErr() error {
	if v := c.readErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// LastReadAt returns when the transport last received a stdout JSON-RPC
// frame. The zero value means no frame has been read yet.
func (c *rpcConn) LastReadAt() time.Time {
	n := c.lastRead.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}
