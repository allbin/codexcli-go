package codexcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// BidiFixtureExecutor pairs three in-process pipes to simulate a codex
// subprocess for hermetic tests. Tests drive the server side directly:
// read what the client wrote off ServerReader, write scripted responses
// or notifications via the helper methods.
//
// Use this when a test needs to script the exact JSON-RPC exchange — it
// trades the convenience of fixture files for direct control over
// timing and ordering, which is essential for approval/interrupt cases
// where the server's response depends on what the client just sent.
type BidiFixtureExecutor struct {
	// ServerReader receives bytes the client wrote (the subprocess's stdin).
	ServerReader *bufio.Reader
	// ServerWriter delivers bytes to the client (the subprocess's stdout).
	// Closing it triggers EOF on the client side.
	ServerWriter io.WriteCloser
	// StderrWriter feeds bytes that surface on the client's stderr drain.
	StderrWriter io.WriteCloser

	stdoutR io.ReadCloser
	stdinR  io.ReadCloser
	stdinW  io.WriteCloser
	stderrR io.ReadCloser

	mu       sync.Mutex
	waitCh   chan struct{}
	waitErr  error
	doneOnce sync.Once

	startOnce sync.Once
}

// NewBidiFixtureExecutor wires the pipes and returns a fresh fixture.
// The returned executor satisfies the Executor interface; pass it to
// NewWithExecutor.
func NewBidiFixtureExecutor() *BidiFixtureExecutor {
	stdoutR, stdoutW := io.Pipe() // server -> client (stdout)
	stdinR, stdinW := io.Pipe()   // client -> server (stdin)
	stderrR, stderrW := io.Pipe() // server stderr -> client drain
	return &BidiFixtureExecutor{
		ServerReader: bufio.NewReader(stdinR),
		ServerWriter: stdoutW,
		StderrWriter: stderrW,

		stdoutR: stdoutR,
		stdinR:  stdinR,
		stdinW:  stdinW,
		stderrR: stderrR,

		waitCh: make(chan struct{}),
	}
}

// Start satisfies Executor. ctx is observed: cancellation marks the
// fixture as exited with the context error so the client's reap loop
// can complete.
func (e *BidiFixtureExecutor) Start(ctx context.Context, _ *StartConfig) (*Process, error) {
	e.startOnce.Do(func() {
		go func() {
			select {
			case <-ctx.Done():
				e.signalDone(ctx.Err())
			case <-e.waitCh:
			}
		}()
	})
	return &Process{
		Stdout: e.stdoutR,
		Stderr: e.stderrR,
		Stdin:  e.stdinW,
		Wait: func() error {
			<-e.waitCh
			return e.waitErr
		},
	}, nil
}

// signalDone fires waitCh exactly once; safe to call from many places.
// Also closes the server-side write pipes so the client's read loops
// see EOF and don't strand on the next ReadBytes call.
func (e *BidiFixtureExecutor) signalDone(err error) {
	e.doneOnce.Do(func() {
		e.mu.Lock()
		e.waitErr = err
		e.mu.Unlock()
		_ = e.ServerWriter.Close()
		_ = e.StderrWriter.Close()
		close(e.waitCh)
	})
}

// CloseFromServer simulates a clean server exit: close the client
// stdout/stderr pipes and mark Wait as returning nil.
func (e *BidiFixtureExecutor) CloseFromServer() {
	_ = e.ServerWriter.Close()
	_ = e.StderrWriter.Close()
	e.signalDone(nil)
}

// FailFromServer simulates an abrupt server exit; Wait will return
// the provided error so the client reports a non-normal exit.
func (e *BidiFixtureExecutor) FailFromServer(waitErr error) {
	_ = e.ServerWriter.Close()
	_ = e.StderrWriter.Close()
	e.signalDone(waitErr)
}

// ReadFrame pulls the next JSON-RPC line off the server-side reader.
// Returns io.EOF when the client closes stdin.
func (e *BidiFixtureExecutor) ReadFrame() (rpcFrame, error) {
	line, err := e.ServerReader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return rpcFrame{}, err
	}
	var f rpcFrame
	if uerr := json.Unmarshal(line, &f); uerr != nil {
		return rpcFrame{}, fmt.Errorf("decode rpc frame %q: %w", string(line), uerr)
	}
	return f, nil
}

// SendNotification writes a notification frame to the client.
func (e *BidiFixtureExecutor) SendNotification(method string, params any) error {
	return writeRPCFrame(e.ServerWriter, rpcFrame{Method: method, Params: mustRawJSON(params)})
}

// SendResponse writes a successful response with the given id and result.
func (e *BidiFixtureExecutor) SendResponse(id json.RawMessage, result any) error {
	return writeRPCFrame(e.ServerWriter, rpcFrame{ID: id, Result: mustRawJSON(result)})
}

// SendRawResponse writes a response whose result is pre-encoded.
func (e *BidiFixtureExecutor) SendRawResponse(id json.RawMessage, result json.RawMessage) error {
	return writeRPCFrame(e.ServerWriter, rpcFrame{ID: id, Result: result})
}

// SendErrorResponse writes an error response for the given request id.
// Use it to script server-side rejections — an unknown method, a malformed
// param — that a test needs the client to classify.
func (e *BidiFixtureExecutor) SendErrorResponse(id json.RawMessage, code int, message string) error {
	return writeRPCFrame(e.ServerWriter, rpcFrame{
		ID:    id,
		Error: &rpcFrameError{Code: code, Message: message},
	})
}

// SendRequest writes a server-initiated request (e.g. an approval) and
// returns the id used so the test can later match the response.
func (e *BidiFixtureExecutor) SendRequest(id json.RawMessage, method string, params any) error {
	return writeRPCFrame(e.ServerWriter, rpcFrame{ID: id, Method: method, Params: mustRawJSON(params)})
}

// rpcFrame is the test-facing JSON-RPC frame used by ReadFrame / Send*.
// It mirrors rpcMessage but lives in the public package so test helpers
// can construct frames without reaching into rpc internals.
type rpcFrame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcFrameError  `json:"error,omitempty"`
}

type rpcFrameError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// IsRequest reports whether the frame is a client-initiated request.
func (f rpcFrame) IsRequest() bool { return f.Method != "" && f.ID != nil }

// IsNotification reports whether the frame is a notification.
func (f rpcFrame) IsNotification() bool { return f.Method != "" && f.ID == nil }

// IsResponse reports whether the frame is a response.
func (f rpcFrame) IsResponse() bool { return f.Method == "" && f.ID != nil }

func writeRPCFrame(w io.Writer, f rpcFrame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	_, err = w.Write(body)
	return err
}

func mustRawJSON(v any) json.RawMessage {
	if raw, ok := v.(json.RawMessage); ok {
		return raw
	}
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// --- Transcript-based fixture replay ---

// TranscriptFixtureExecutor replays a JSONL transcript file recorded by
// cmd/capture. Each line is a JSON object {"dir": "out"|"in", "msg": <rpcFrame>}.
//
// "out" lines (server -> client) are sent immediately. "in" lines
// (client -> server) are expected — the executor blocks until the
// client writes a frame whose method+id match, and then advances. This
// keeps replay deterministic even when the client is structurally
// equivalent to the recording but takes slightly different timing.
type TranscriptFixtureExecutor struct {
	transcript []transcriptEntry
	bidi       *BidiFixtureExecutor
	t          testingShim
}

type transcriptEntry struct {
	Direction string    `json:"dir"`
	Frame     rpcFrame  `json:"msg"`
	Sleep     int64     `json:"sleepMs,omitempty"`
	At        time.Time `json:"at,omitempty"`
}

// testingShim is satisfied by *testing.T; declared as an interface so
// the fixture package doesn't need to import testing.
type testingShim interface {
	Errorf(format string, args ...any)
	Helper()
}

// LoadTranscript reads a JSONL transcript from disk.
func LoadTranscript(path string) ([]transcriptEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []transcriptEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e transcriptEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("transcript line %q: %w", line, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// NewTranscriptFixtureExecutor wires a fixture executor that drives the
// pre-recorded exchange. Construct, pass to NewWithExecutor, then call
// Run to start the replay goroutine.
func NewTranscriptFixtureExecutor(t testingShim, entries []transcriptEntry) *TranscriptFixtureExecutor {
	return &TranscriptFixtureExecutor{
		transcript: entries,
		bidi:       NewBidiFixtureExecutor(),
		t:          t,
	}
}

// Start satisfies Executor.
func (e *TranscriptFixtureExecutor) Start(ctx context.Context, cfg *StartConfig) (*Process, error) {
	proc, err := e.bidi.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	go e.replay()
	return proc, nil
}

// replay walks the transcript: for "out" entries, emit immediately; for
// "in" entries, block on ReadFrame and verify method/id matches. Mismatch
// fails the test but doesn't crash the replay — it logs and advances.
func (e *TranscriptFixtureExecutor) replay() {
	for _, entry := range e.transcript {
		if entry.Sleep > 0 {
			time.Sleep(time.Duration(entry.Sleep) * time.Millisecond)
		}
		switch entry.Direction {
		case "out":
			if err := writeRPCFrame(e.bidi.ServerWriter, entry.Frame); err != nil {
				if !errors.Is(err, io.ErrClosedPipe) {
					e.t.Errorf("transcript: write %s: %v", entry.Frame.Method, err)
				}
				return
			}
		case "in":
			got, err := e.bidi.ReadFrame()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					e.t.Errorf("transcript: expected %s, got read err: %v", entry.Frame.Method, err)
				}
				return
			}
			if got.Method != entry.Frame.Method {
				e.t.Errorf("transcript: expected method %q, got %q", entry.Frame.Method, got.Method)
			}
		default:
			e.t.Errorf("transcript: unknown direction %q", entry.Direction)
		}
	}
	// Transcript exhausted: signal a clean server exit so the client's
	// reap loop can finalize.
	e.bidi.CloseFromServer()
}
