// Command capture records a real codex app-server JSON-RPC transcript
// to a file for later replay in tests. It runs a single turn against a
// live codex installation, mirroring every framed line in both
// directions to a .jsonl file.
//
// Output format (one JSON object per line):
//
//	{"dir":"out","msg":{...frame...}}   // server -> client
//	{"dir":"in", "msg":{...frame...}}   // client -> server
//
// Usage:
//
//	go run ./cmd/capture -prompt "say hi" -out testdata/hello.jsonl
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

func main() {
	var (
		prompt = flag.String("prompt", "Reply with one short sentence saying hello.", "user prompt to send")
		out    = flag.String("out", "transcript.jsonl", "output file path")
		binary = flag.String("bin", "codex", "codex CLI binary")
		ctxArg = flag.Duration("timeout", 120*time.Second, "overall timeout")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *ctxArg)
	defer cancel()

	cmd := exec.CommandContext(ctx, *binary, "app-server", "--listen", "stdio://")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	logger := &transcriptLogger{f: f}

	// stdoutEvents fans the raw lines into a channel for in-protocol
	// synchronization (matching a response by id) while continuing to
	// log every line to the transcript.
	stdoutEvents := make(chan json.RawMessage, 64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stdoutEvents)
		r := bufio.NewReader(stdout)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				logger.write("out", line)
				cp := append([]byte(nil), line...)
				stdoutEvents <- json.RawMessage(cp)
			}
			if err != nil {
				if err != io.EOF {
					fmt.Fprintln(os.Stderr, "stdout read:", err)
				}
				return
			}
		}
	}()

	writeFrame := func(method string, params any, id any) {
		obj := map[string]any{"method": method}
		if id != nil {
			obj["id"] = id
		}
		if params != nil {
			obj["params"] = params
		}
		body, _ := json.Marshal(obj)
		body = append(body, '\n')
		_, _ = stdin.Write(body)
		logger.write("in", body)
	}

	awaitResponse := func(id int) json.RawMessage {
		for line := range stdoutEvents {
			var f struct {
				ID     *int            `json:"id,omitempty"`
				Result json.RawMessage `json:"result,omitempty"`
				Error  json.RawMessage `json:"error,omitempty"`
			}
			if err := json.Unmarshal(line, &f); err != nil {
				continue
			}
			if f.ID != nil && *f.ID == id {
				if len(f.Error) > 0 {
					log.Fatalf("rpc error on id %d: %s", id, string(f.Error))
				}
				return f.Result
			}
		}
		log.Fatalf("stdout closed before response for id %d", id)
		return nil
	}

	writeFrame("initialize", map[string]any{
		"clientInfo": map[string]any{"name": "codexcli_capture", "version": "0.0.1"},
	}, 1)
	awaitResponse(1)
	writeFrame("initialized", struct{}{}, nil)

	writeFrame("thread/start", map[string]any{
		"ephemeral": true,
		"cwd":       mustCwd(),
	}, 2)
	threadStartResult := awaitResponse(2)

	var threadResp struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(threadStartResult, &threadResp); err != nil {
		log.Fatal("decode thread/start:", err)
	}

	writeFrame("turn/start", map[string]any{
		"threadId": threadResp.Thread.ID,
		"input":    []map[string]any{{"type": "text", "text": *prompt}},
	}, 3)
	awaitResponse(3)

	// Wait for turn/completed to land before tearing down. We watch the
	// transcript channel for the notification rather than parse every
	// frame.
loop:
	for line := range stdoutEvents {
		var f struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(line, &f); err == nil {
			if f.Method == "turn/completed" {
				break loop
			}
		}
	}
	_ = stdin.Close()
	wg.Wait()
	_ = cmd.Wait()
	fmt.Fprintf(os.Stderr, "captured transcript -> %s\n", *out)
}

type transcriptLogger struct {
	mu sync.Mutex
	f  *os.File
}

func (l *transcriptLogger) write(dir string, line []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Strip trailing newline before re-wrapping
	raw := json.RawMessage(line)
	// The frame line already includes a trailing \n; trim it for clean
	// embedding in the wrapper object.
	for len(raw) > 0 && (raw[len(raw)-1] == '\n' || raw[len(raw)-1] == '\r') {
		raw = raw[:len(raw)-1]
	}
	obj := struct {
		Dir string          `json:"dir"`
		Msg json.RawMessage `json:"msg"`
	}{Dir: dir, Msg: raw}
	body, _ := json.Marshal(obj)
	body = append(body, '\n')
	_, _ = l.f.Write(body)
}

func mustCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	return wd
}
