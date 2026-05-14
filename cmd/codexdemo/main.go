// Command codexdemo proves the end-to-end happy path: spawn codex
// app-server, run the handshake, start a thread, dispatch one turn, and
// print every event until turn/completed.
//
//	go run ./cmd/codexdemo -prompt "Reply with one short sentence."
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	codexcli "github.com/allbin/codexcli-go"
	"github.com/allbin/codexcli-go/schema"
)

func main() {
	var (
		prompt  = flag.String("prompt", "Reply with one short sentence saying hello.", "user prompt")
		binary  = flag.String("bin", "codex", "codex CLI binary")
		timeout = flag.Duration("timeout", 90*time.Second, "overall timeout")
		cwd     = flag.String("cwd", "", "thread cwd (defaults to process cwd)")
		model   = flag.String("model", "", "model override")
	)
	flag.Parse()

	if *cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			*cwd = wd
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := []codexcli.Option{
		codexcli.WithBinaryPath(*binary),
		codexcli.WithCwd(*cwd),
		codexcli.WithEphemeralThread(),
		codexcli.WithStderrCallback(func(line string) {
			fmt.Fprintln(os.Stderr, "[codex stderr]", line)
		}),
	}
	if *model != "" {
		opts = append(opts, codexcli.WithModel(*model))
	}

	client := codexcli.New(opts...)
	stream, err := client.Run(sigCtx, *prompt)
	if err != nil {
		log.Fatalf("Run: %v", err)
	}
	defer stream.Close()

	for ev := range stream.Events() {
		printEvent(ev)
	}

	turn, err := stream.Wait()
	if err != nil {
		log.Fatalf("Wait: %v", err)
	}
	fmt.Printf("\n=== Turn %s status=%s ===\n", turn.ID, turn.Status)
}

func printEvent(ev codexcli.Event) {
	switch e := ev.(type) {
	case *codexcli.StartEvent:
		fmt.Printf("[Start] thread=%s model=%s cwd=%s\n", e.ThreadID, e.Model, e.Cwd)
	case *codexcli.TurnStartedEvent:
		fmt.Printf("[TurnStarted] turn=%s status=%s\n", e.Turn.ID, e.Turn.Status)
	case *codexcli.ItemStartedEvent:
		fmt.Printf("[ItemStarted] type=%s id=%s\n", e.Item.Type, e.Item.ID)
	case *codexcli.AgentMessageDeltaEvent:
		fmt.Printf("[Delta] %s", e.Delta)
	case *codexcli.ItemCompletedEvent:
		text := ""
		if e.Item.Type == "agentMessage" {
			text = e.Item.Text
		}
		fmt.Printf("\n[ItemCompleted] type=%s id=%s text=%q\n", e.Item.Type, e.Item.ID, summarize(text))
	case *codexcli.TurnCompletedEvent:
		_ = printTurnSummary(e.Turn)
	case *codexcli.ErrorEvent:
		fmt.Printf("[Error fatal=%v] %v\n", e.Fatal, e.Err)
	case *codexcli.UnknownEvent:
		fmt.Printf("[Unknown] method=%s params=%s\n", e.Method, truncate(string(e.Params), 200))
	case *codexcli.StderrEvent:
		fmt.Printf("[stderr] %s\n", e.Line)
	default:
		fmt.Printf("[?] %T %v\n", ev, ev)
	}
}

func printTurnSummary(t schema.Turn) error {
	fmt.Printf("[TurnCompleted] id=%s status=%s items=%d", t.ID, t.Status, len(t.Items))
	if t.DurationMs != nil {
		fmt.Printf(" duration=%dms", *t.DurationMs)
	}
	fmt.Println()
	if t.Error != nil {
		fmt.Printf("  error: %s\n", t.Error.Message)
	}
	return nil
}

func summarize(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
