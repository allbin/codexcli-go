# codexcli-go

Go client for the [`codex app-server`](https://github.com/openai/codex) JSON-RPC protocol. Mirrors the [`claudecli-go`](https://github.com/allbin/claudecli-go) public API so consumers can swap implementations by changing the import path.

**Status**: early. Approvals, MCP elicitation, fork/resume, dynamic tools, and the file/exec/account surfaces are not yet wired. The initialize/thread-start/turn-start happy path works end-to-end against codex CLI 0.130.0.

## Install

```
go get github.com/allbin/codexcli-go@<commit-SHA>
```

Pre-1.0 — consumers pin commit SHAs, no release tags. Requires the `codex` CLI on `PATH` (or override via `WithBinaryPath`) and `codex login` completed once for OAuth.

## Quick start

```go
client := codexcli.New(
    codexcli.WithCwd("/path/to/workspace"),
    codexcli.WithEphemeralThread(),
)
stream, err := client.Run(ctx, "Summarise this repo in one sentence.")
if err != nil {
    log.Fatal(err)
}
defer stream.Close()

for ev := range stream.Events() {
    switch e := ev.(type) {
    case *codexcli.AgentMessageDeltaEvent:
        fmt.Print(e.Delta)
    case *codexcli.TurnCompletedEvent:
        fmt.Printf("\nstatus=%s duration=%dms\n", e.Turn.Status, *e.Turn.DurationMs)
    }
}
```

A runnable demo lives at `cmd/codexdemo`:

```
go run ./cmd/codexdemo -prompt "Reply with 'Hello from codex.' and stop."
```

## Architecture

| File | Role |
|---|---|
| `client.go` | `Client`, `Conn` — spawn codex, run the handshake, dispatch requests, route notifications to per-thread subscribers. |
| `rpc.go` | JSON-RPC 2.0 framing over line-delimited JSON. Outbound request/response correlation by id; inbound dispatch to notify + request callbacks. |
| `executor.go` | `Executor` interface + `LocalExecutor`. Swap in fakes for tests or remote execution. |
| `option.go` | Functional options (`WithCwd`, `WithModel`, `WithEphemeralThread`, ...). Extras hatch via `WithThreadExtra` / `WithTurnExtra`. |
| `event.go` | Sealed `Event` interface + typed wire events (`TurnStartedEvent`, `ItemStartedEvent`, `AgentMessageDeltaEvent`, `ItemCompletedEvent`, `TurnCompletedEvent`, `ErrorEvent`, `UnknownEvent`). |
| `stream.go` | `Stream` — channel-of-events with lifecycle tracking and `Wait()` for blocking callers. |
| `thread.go` | `Thread` — start additional turns on the same conversation. |
| `schema/` | Hand-rolled Go types for the JSON Schema surface (`InitializeParams`, `ThreadStartParams`, `TurnStartParams`, `Turn`, `ThreadItem`, notification payloads). |
| `cmd/genschema/` | `go generate` target that runs `codex app-server generate-json-schema` to refresh raw schemas. Output ignored by git. |
| `cmd/codexdemo/` | End-to-end smoke test against the real codex CLI. |

## Protocol surprises hit in the first pass

- `type X json.RawMessage` does **not** inherit `MarshalJSON`/`UnmarshalJSON`. A named alias becomes a bare `[]byte` for the encoding/json runtime, which then base64-decodes the payload. Use a struct wrapper or define the methods explicitly — see `schema.AskForApproval`.
- codex app-server emits a lot of notifications outside the documented happy-path set: `mcpServer/startupStatus/updated`, `thread/status/changed`, `account/rateLimits/updated`, `thread/tokenUsage/updated`, `thread/started`. The SDK surfaces unrecognized notifications as `*UnknownEvent` so consumers don't break when codex adds new ones.
- `turn/completed` arrives with `items: []` even when item events were streamed. Rely on `item/*` notifications for the canonical incremental view, not the `Turn.Items` array.
- Server-initiated requests (`execCommandApproval`, `item/commandExecution/requestApproval`, etc.) currently auto-decline. Approval callback wiring is the next milestone.

## Regenerating the schema bundle

```
go generate ./schema
```

This shells out to `codex app-server generate-json-schema --out schema/v2_raw`. The output is git-ignored — re-run after a codex CLI bump and diff against the hand-rolled types in `schema/types.go`. A future pass will replace the hand-roll with a real Go code generator (likely `github.com/atombender/go-jsonschema`).

## Conventions

Match `claudecli-go`: functional options, sealed `Event` interface, ctx-first, `Executor` abstraction, `Client` / `Conn` split. Tests use in-memory fakes; race-clean.
