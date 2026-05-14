package codexcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// scriptExecutor wraps a /bin/sh -c invocation so process-death tests
// can spawn a real subprocess that we can SIGKILL. The script
// reads/writes JSON-RPC frames on stdin/stdout.
type scriptExecutor struct {
	script string
	mu     sync.Mutex
	last   *exec.Cmd
}

func (e *scriptExecutor) Start(ctx context.Context, _ *StartConfig) (*Process, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", e.script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.last = cmd
	e.mu.Unlock()
	return &Process{
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  stdin,
		Wait:   cmd.Wait,
	}, nil
}

func (e *scriptExecutor) cmd() *exec.Cmd {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last
}

// initHandshakeScript is the minimum sh fragment that responds to one
// initialize request, consumes the initialized notification, then
// performs whatever extra behavior the caller appends.
const initHandshakeScript = `read -r initLine
echo "{\"id\":1,\"result\":{\"codexHome\":\"/tmp\",\"platformFamily\":\"unix\",\"platformOs\":\"linux\",\"userAgent\":\"test\"}}"
read -r initdLine
`

// TestProcessExit_NormalClose simulates a clean exit: the subprocess
// closes stdout after the handshake, the reaper records ExitReasonNormal,
// and the event channel closes cleanly.
func TestProcessExit_NormalClose(t *testing.T) {
	exec := &scriptExecutor{script: initHandshakeScript + "exit 0"}
	client := NewWithExecutor(exec, WithEphemeralThread())

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Wait for the reaper to record the exit. We poll because there's
	// no synchronous "process is gone" signal short of reaching into
	// the conn internals.
	waitForExit(t, conn, 2*time.Second)

	exitErr := conn.ExitError()
	if exitErr == nil {
		t.Fatal("ExitError is nil after process exit")
	}
	if exitErr.Reason != ExitReasonNormal {
		t.Errorf("Reason = %s, want %s", exitErr.Reason, ExitReasonNormal)
	}
	if exitErr.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", exitErr.ExitCode)
	}
	if !errors.Is(exitErr, ErrProcessExited) {
		t.Errorf("ErrProcessExited match failed")
	}

	// Calling NewThread after exit returns the typed error.
	_, err = conn.NewThread(context.Background())
	if err == nil {
		t.Fatal("expected NewThread to fail after exit")
	}
	if !errors.Is(err, ErrProcessExited) {
		t.Errorf("err = %v, want ErrProcessExited", err)
	}

	_ = conn.Close()
}

// TestProcessExit_SIGKILL_MidTurn fires SIGKILL on the subprocess while
// it's mid-stream and verifies the typed exit error reaches the consumer.
func TestProcessExit_SIGKILL_MidTurn(t *testing.T) {
	startGoroutines := runtime.NumGoroutine()
	// Script: handshake, then a thread/start response, then sleep forever
	// (waiting to be killed). We pin a known thread id to compare.
	script := initHandshakeScript + `
read -r threadStartLine
echo '{"id":2,"result":{"thread":{"id":"thr_kill","sessionId":"s","cwd":"/tmp","cliVersion":"x","modelProvider":"o","ephemeral":true,"createdAt":1,"updatedAt":1,"preview":""},"cwd":"/tmp","model":"gpt-5","modelProvider":"o","approvalPolicy":"never","sandbox":{"mode":"read-only"}}}'
echo "stderr-tail-marker" >&2
sleep 60
`
	exe := &scriptExecutor{script: script}
	client := NewWithExecutor(exe, WithEphemeralThread(), WithCwd("/tmp"))

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	_, err = conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}

	// SIGKILL the subprocess.
	cmd := exe.cmd()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("no subprocess to kill")
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}

	waitForExit(t, conn, 3*time.Second)

	exit := conn.ExitError()
	if exit == nil {
		t.Fatal("missing exit error")
	}
	if exit.Reason != ExitReasonKilled {
		t.Errorf("Reason = %s, want %s", exit.Reason, ExitReasonKilled)
	}
	if exit.Signal != "SIGKILL" {
		t.Errorf("Signal = %q, want SIGKILL", exit.Signal)
	}
	if !errors.Is(exit, ErrProcessExited) {
		t.Errorf("ErrProcessExited match failed")
	}
	if !strings.Contains(exit.LastStderr, "stderr-tail-marker") {
		t.Errorf("LastStderr should include captured stderr; got %q", exit.LastStderr)
	}

	// Subsequent call returns typed error rather than hang/panic.
	_, err = conn.NewThread(context.Background())
	if !errors.Is(err, ErrProcessExited) {
		t.Errorf("post-death NewThread err = %v, want ErrProcessExited", err)
	}

	// Cleanup + goroutine leak check.
	_ = conn.Close()
	checkGoroutineLeak(t, startGoroutines)
}

// TestProcessExit_StdinWriteAfterDeath ensures Query-style calls after
// process death return a typed error promptly rather than blocking on
// the pipe write.
func TestProcessExit_StdinWriteAfterDeath(t *testing.T) {
	exe := &scriptExecutor{script: initHandshakeScript + "exit 0"}
	client := NewWithExecutor(exe, WithEphemeralThread())

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForExit(t, conn, 2*time.Second)

	start := time.Now()
	_, err = conn.NewThread(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error after death")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("NewThread took %v after death — should fail fast", elapsed)
	}
	if !errors.Is(err, ErrProcessExited) {
		t.Errorf("err = %v, want ErrProcessExited", err)
	}
	_ = conn.Close()
}

// TestProcessExit_CrashCapturesStderr makes the subprocess emit a panic
// line on stderr then crash; the captured tail should surface in
// ProcessExitError.LastStderr to aid debugging.
func TestProcessExit_CrashCapturesStderr(t *testing.T) {
	exe := &scriptExecutor{script: initHandshakeScript + `echo "panic: simulated crash" >&2
exit 17`}
	client := NewWithExecutor(exe, WithEphemeralThread())
	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitForExit(t, conn, 2*time.Second)

	exit := conn.ExitError()
	if exit.Reason != ExitReasonCrashed {
		t.Errorf("Reason = %s, want crashed", exit.Reason)
	}
	if exit.ExitCode != 17 {
		t.Errorf("ExitCode = %d, want 17", exit.ExitCode)
	}
	if !strings.Contains(exit.LastStderr, "panic: simulated crash") {
		t.Errorf("LastStderr missing panic line; got %q", exit.LastStderr)
	}
	if !strings.Contains(exit.Error(), "simulated crash") {
		t.Errorf("Error string should include stderr tail: %s", exit.Error())
	}
	_ = conn.Close()
}

// TestProcessExit_StreamEmitsExitEvent verifies a Stream consumer sees a
// terminal ProcessExitEvent when the subprocess dies mid-turn.
func TestProcessExit_StreamEmitsExitEvent(t *testing.T) {
	// Use the fixture executor here — easier to script the mid-turn
	// death than with sh.
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())

	threadID := "thr_die"
	turnID := "turn_die"

	go func() {
		id, _ := expectRequest(t, fix, "initialize")
		_ = fix.SendResponse(id, basicInitResponse())
		expectNotification(t, fix, "initialized")
		id, _ = expectRequest(t, fix, "thread/start")
		_ = fix.SendResponse(id, basicThreadStartResponse(threadID))
		id, _ = expectRequest(t, fix, "turn/start")
		_ = fix.SendResponse(id, map[string]any{
			"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
		})
		_ = fix.SendNotification("turn/started", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
		})
		// Process dies mid-turn.
		fix.FailFromServer(fmt.Errorf("simulated crash"))
	}()

	stream, err := client.Run(context.Background(), "do something")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	var sawExit bool
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				break loop
			}
			if e, ok := ev.(*ProcessExitEvent); ok {
				sawExit = true
				if !errors.Is(e.Err, ErrProcessExited) {
					t.Errorf("event err = %v, want ErrProcessExited", e.Err)
				}
			}
		case <-deadline:
			t.Fatal("timeout waiting for ProcessExitEvent")
		}
	}
	if !sawExit {
		t.Error("did not observe ProcessExitEvent")
	}
}

// waitForExit polls Conn.ExitError until the reaper has filled it.
func waitForExit(t *testing.T, conn *Conn, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if conn.ExitError() != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for process exit")
}

// minimal smoke-test to confirm scriptExecutor wiring works at all.
func TestScriptExecutor_Smoke(t *testing.T) {
	exe := &scriptExecutor{script: `echo hi; exit 0`}
	proc, err := exe.Start(context.Background(), &StartConfig{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	atomic.StoreInt32(new(int32), 0) // silence import
	defer proc.Stdout.Close()
	r := bufio.NewReader(proc.Stdout)
	line, _ := r.ReadString('\n')
	if !strings.HasPrefix(line, "hi") {
		t.Errorf("got %q", line)
	}
	if err := proc.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

// stable marker to satisfy unused imports.
var _ = json.RawMessage("{}")
var _ = os.Getenv
var _ = io.EOF
