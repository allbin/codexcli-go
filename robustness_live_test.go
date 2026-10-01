//go:build integration

package codexcli

// Live checks of server-request withdrawal, Ping and resume-not-found
// against the codex on PATH with a signed-in account. They spend two small
// turns and one 15-second command:
//
//	go test -tags integration -run TestLive_Robustness -count=1 -v .

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

func liveConn(t *testing.T, opts ...Option) *Conn {
	t.Helper()
	conn, err := New(append([]Option{WithEphemeralThread()}, opts...)...).Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func requirePing(t *testing.T, conn *Conn, when string) {
	t.Helper()
	start := time.Now()
	if err := conn.Ping(context.Background(), 2*time.Second); err != nil {
		t.Errorf("Ping %s: %v", when, err)
		return
	}
	t.Logf("Ping %s: %s", when, time.Since(start))
}

// TestLive_RobustnessApprovalWithdrawnOnInterrupt: interrupting a turn
// with an approval pending cancels the handler's ctx, and Ping answers
// while the approval waits.
func TestLive_RobustnessApprovalWithdrawnOnInterrupt(t *testing.T) {
	asked := make(chan context.Context, 1)
	handlerDone := make(chan struct{})
	approve := func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
		defer close(handlerDone)
		asked <- ctx
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return Decline{}, nil
		}
	}
	conn := liveConn(t,
		WithApprovalPolicy(schema.NewAskForApprovalString("untrusted")),
		WithSandbox(schema.SandboxReadOnly),
		WithApprovalHandler(approve))
	th, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	stream, err := th.StartTurn(context.Background(), "Run exactly this shell command and nothing else: touch "+t.TempDir()+"/probe")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()

	var ctx context.Context
	select {
	case ctx = <-asked:
	case <-time.After(90 * time.Second):
		t.Fatal("codex never asked for approval")
	}
	requirePing(t, conn, "with an approval pending")
	if err := th.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("approval ctx not cancelled after interrupt")
	}
	<-handlerDone
	turn, err := drainTurnObserving(stream, 30*time.Second, nil)
	if err != nil || turn.Status != schema.TurnInterrupted {
		t.Errorf("turn = %+v, %v; want interrupted", turn, err)
	}
	requirePing(t, conn, "after the interrupt")
}

// TestLive_RobustnessPingMidTurn: Ping answers while the model generates
// and while a command runs that prints nothing for 15 seconds, during
// which codex sends no notifications at all.
func TestLive_RobustnessPingMidTurn(t *testing.T) {
	conn := liveConn(t,
		WithApprovalPolicy(schema.NewAskForApprovalString("never")),
		WithSandbox(schema.SandboxDangerFull))
	th, err := conn.NewThread(context.Background())
	if err != nil {
		t.Fatalf("NewThread: %v", err)
	}
	requirePing(t, conn, "idle")
	stream, err := th.StartTurn(context.Background(),
		"Write the numbers 1 to 30, one per line. Then call your shell tool with the command `sleep 15 && echo finished`, wait for it, and reply with its output.")
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	defer stream.Close()

	deltas, pinged := 0, 0
	turn, err := drainTurnObserving(stream, 3*time.Minute, func(ev Event) {
		switch e := ev.(type) {
		case *AgentMessageDeltaEvent:
			if deltas++; deltas == 10 {
				requirePing(t, conn, "mid-generation")
			}
		case *ItemStartedEvent:
			if e.Item.Type == schema.ItemTypeCommandExecution && e.Item.Command != nil && strings.Contains(*e.Item.Command, "sleep 15") {
				go func() {
					for range 3 {
						time.Sleep(4 * time.Second)
						requirePing(t, conn, "during the silent command")
					}
				}()
				pinged = 3
			}
		}
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if turn.Status != schema.TurnCompleted {
		t.Fatalf("turn %s", turn.Status)
	}
	if pinged == 0 {
		t.Skip("the model did not run the sleep command; the silent-command check did not run")
	}
}

// TestLive_RobustnessResumeUnknownThread: codex answers thread/resume for
// an unknown thread with "no rollout found", which is ErrThreadNotFound.
func TestLive_RobustnessResumeUnknownThread(t *testing.T) {
	conn := liveConn(t)
	_, err := conn.ResumeThread(context.Background(), "01a0f6f0-2dae-7cc3-b640-08b7c810a0f5")
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("ResumeThread = %v, want ErrThreadNotFound", err)
	}
	t.Logf("codex said: %v", err)
}
