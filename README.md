# codexcli-go

Go client for the [`codex app-server`](https://github.com/openai/codex) JSON-RPC protocol. Mirrors the [`claudecli-go`](https://github.com/allbin/claudecli-go) public API so consumers can swap implementations by changing the import path.

**Status**: pre-1.0. The core protocol surface is covered: initialize, thread start/resume, turn lifecycle, approvals, content deltas (agent message, command output, reasoning, plan), token usage, rate limits, aggregated diffs, and skills (discover, toggle, invoke). MCP elicitation, fork, dynamic tools, and the file/exec/account RPC surfaces are not yet wired. Tested against codex CLI 0.133.0+.

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
    case *codexcli.ContentDeltaEvent:
        // Command output, reasoning, plan deltas, etc.
        fmt.Printf("[%s] %s", e.Kind, e.Delta)
    case *codexcli.TurnCompletedEvent:
        fmt.Printf("\nstatus=%s duration=%dms\n", e.Turn.Status, *e.Turn.DurationMs)
        if e.Turn.Usage != nil {
            fmt.Printf("tokens: %d total\n", e.Turn.Usage.Total.TotalTokens)
        }
    }
}
```

A runnable demo lives at `cmd/codexdemo`:

```
go run ./cmd/codexdemo -prompt "Reply with 'Hello from codex.' and stop."
```

## Resuming a thread

```go
conn, _ := client.Connect(ctx)
defer conn.Close()

thread, err := conn.ResumeThread(ctx, savedThreadID)
if errors.Is(err, codexcli.ErrThreadNotFound) {
    thread, _ = conn.NewThread(ctx) // fall back to fresh thread
}

stream, _ := thread.StartTurn(ctx, "Continue where we left off.")
```

## Listing available models

`ListModels` returns the model registry codex CLI caches on disk at `$CODEX_HOME/models_cache.json` (codex refreshes it from OpenAI's API on each launch). It's a pure file read — no subprocess, no network — so it's safe to call from a UI thread or a CLI flag handler.

```go
models, err := codexcli.ListModels(ctx)
if errors.Is(err, codexcli.ErrModelsCacheUnavailable) {
    // No cache yet — fall back to a built-in default list.
}
if err != nil {
    log.Fatal(err)
}
for _, m := range models {
    if m.Visibility != codexcli.VisibilityList {
        continue // codex flagged this model as hidden from pickers
    }
    fmt.Printf("%-20s %s\n", m.Slug, m.DisplayName)
}
```

For live model availability on a running connection, use `Conn.ListModels(ctx)` which queries the server via the `model/list` RPC.

Resolution order for the cache directory: `WithCodexHome(...)` option → `$CODEX_HOME` env var → `$HOME/.codex`.

## Skills

Unlike Claude Code — which pushes a flat list of skill names on its init event — codex does not advertise skills at thread start. Discovery is an explicit pull via the `skills/list` RPC, so call `Conn.ListSkills` when you actually need the list (e.g. to populate a picker), not on every connect.

```go
entries, err := conn.ListSkills(ctx, []string{"/path/to/repo"}, false /* forceReload */)
if err != nil {
    log.Fatal(err)
}
for _, e := range entries { // one entry per requested cwd
    for _, s := range e.Skills {
        fmt.Printf("%-20s [%s] enabled=%v — %s\n", s.Name, s.Scope, s.Enabled, s.Description)
    }
    for _, problem := range e.Errors { // skills that failed to parse
        log.Printf("skill error at %s: %s", problem.Path, problem.Message)
    }
}
```

`SkillMetadata` carries more than a name: `Scope` (user/repo/system/admin), the optional `Interface` presentation block (display name, default prompt, brand color, icons), and declared tool `Dependencies`. Pass `forceReload=true` to bypass codex's in-memory cache; otherwise rely on the `SkillsChangedEvent` (the `skills/changed` notification) to know when a re-list is worthwhile.

Invoke a skill in a turn by building a `skill` input. Prefer the discovery-aware `codexcli.SkillInput(meta)`, which pulls the name/path pair straight from a `SkillMetadata` and avoids mispairing a name with the wrong path when the same name is visible from multiple cwds:

```go
stream, err := thread.StartTurnInput(ctx, []schema.UserInput{
    codexcli.SkillInput(entries[0].Skills[0]),     // from ListSkills
    schema.TextInput("apply it to the open PR"),
})
```

The low-level primitive `schema.SkillInput(name, path)` is also available, alongside `schema.MentionInput`, `schema.ImageInput`, and `schema.LocalImageInput` for the other per-turn input variants.

Toggle a skill's enabled state with `Conn.SetSkillEnabledByName(ctx, name, enabled)` or `Conn.SetSkillEnabledByPath(ctx, path, enabled)` (use the path form to disambiguate when a name is visible from multiple scopes). Both return the *effective* enabled state after the write, which can differ from the requested value if another config layer overrides it.

When a turn is started with a skill input, codex echoes it back inside the `userMessage` thread item — `item.UserMessageContent()` decodes the content blocks (text/skill/mention/image) as the `UserInput` union, so a consumer rendering "what was sent" can see the skill block, not just the text.

### Skill approval

codex has **no skill-specific approval request type**. Attaching a `skill` input is self-authorizing — it just runs. When the model autonomously reads a skill it does so via a normal `commandExecution`, gated (if at all) by the command-approval path the SDK already routes. So there is nothing skill-specific to handle in your `ApprovalFunc`.

Skill approval *can* be gated through the granular approval policy, which is an experimental surface — it must be paired with `WithExperimentalAPI()` or the server rejects `thread/start` with `askForApproval.granular requires experimentalApi capability`:

```go
client := codexcli.New(
    codexcli.WithExperimentalAPI(),
    codexcli.WithApprovalPolicy(schema.NewGranularApproval(schema.GranularApproval{
        SandboxApproval: true,
        SkillApproval:   true,
    })),
)
```

Read a policy back with `policy.Granular()` (returns the toggles and `true` for the object form) or `policy.AskForApprovalString()` (the bare-string form: `"untrusted"`, `"on-failure"`, `"on-request"`, `"never"`).

## Events

The event stream surfaces typed events for the full server notification set:

| Event | Server notification | Description |
|---|---|---|
| `TurnStartedEvent` | `turn/started` | Turn accepted, generation starting |
| `TurnCompletedEvent` | `turn/completed` | Turn finished (completed/interrupted/failed), carries `Turn.Usage` |
| `ItemStartedEvent` | `item/started` | Item lifecycle begins |
| `ItemCompletedEvent` | `item/completed` | Item finished with final state |
| `AgentMessageDeltaEvent` | `item/agentMessage/delta` | Streaming assistant text |
| `ContentDeltaEvent` | `item/commandExecution/outputDelta`, `item/fileChange/outputDelta`, `item/reasoning/textDelta`, `item/reasoning/summaryTextDelta`, `item/plan/delta` | Streaming content — discriminate on `Kind` |
| `TurnDiffUpdatedEvent` | `turn/diff/updated` | Aggregated unified diff for the turn |
| `TokenUsageUpdatedEvent` | `thread/tokenUsage/updated` | Token usage snapshot |
| `RateLimitsUpdatedEvent` | `account/rateLimits/updated` | Rate limit status (broadcast to all subscribers) |
| `SkillsChangedEvent` | `skills/changed` | Local skill files changed — re-run `Conn.ListSkills` (broadcast to all subscribers) |
| `ErrorEvent` | `error` | Recoverable or fatal error |
| `ApprovalRequestEvent` | (server requests) | Approval request surfaced alongside callback dispatch |
| `ProcessExitEvent` | (internal) | Subprocess terminated |
| `UnknownEvent` | (any unrecognized) | Forward-compat for new server notifications |

## Approvals

Register a handler with `WithApprovalHandler`. The callback receives a sealed
`ApprovalRequest`; `Method()`, `ThreadID()`, `TurnID()`, and `ItemID()` are
available on every kind without a type switch. `ItemID()` correlates the
approval with the eventual `item/started` / `item/completed` notifications —
it returns the v2 thread item id for command-execution, file-change, and
permissions approvals, and `""` for the legacy v1 (`execCommandApproval`,
`applyPatchApproval`) kinds, which only carry a call id. Type-switch on the
concrete request when you need kind-specific fields (the proposed command,
diff, requested permissions, etc.).

## Command output

Codex app-server streams a `commandExecution` item's stdout/stderr via
`item/commandExecution/outputDelta` notifications and, in practice, leaves
`aggregatedOutput` null on the completed item. By default the SDK passes
items through unchanged, so consumers reconstruct output from
`ContentDeltaEvent` (`Kind == ContentDeltaCommandOutput`) themselves.

Pass `WithAccumulatedOutput()` to have the SDK buffer those deltas keyed by
item id and populate `ItemCompletedEvent.Item.AggregatedOutput` from the
buffer when the server left it empty (a non-empty server value always wins).
The per-item buffer is drained on completion.

## Architecture

| File | Role |
|---|---|
| `client.go` | `Client`, `Conn` — spawn codex, run the handshake, dispatch requests, route notifications to per-thread subscribers. Panic recovery on approval/request handlers. |
| `rpc.go` | JSON-RPC 2.0 framing over line-delimited JSON. Outbound request/response correlation by id; inbound dispatch to notify + request callbacks. Error chain preservation. |
| `executor.go` | `Executor` interface + `LocalExecutor`. Swap in fakes for tests or remote execution. |
| `option.go` | Functional options (`WithCwd`, `WithModel`, `WithEphemeralThread`, ...). Extras hatch via `WithThreadExtra` / `WithTurnExtra`. |
| `event.go` | Sealed `Event` interface + typed events for the full notification set. |
| `stream.go` | `Stream` — channel-of-events with lifecycle tracking and `Wait()` for blocking callers. |
| `thread.go` | `Thread` — start additional turns on the same conversation. |
| `approval.go` | Sealed `ApprovalRequest` / `ApprovalDecision` interfaces, typed approval routing. |
| `models.go` | `ListModels` (file-based) and `Conn.ListModels` (live RPC). |
| `skills.go` | `Conn.ListSkills` / `Conn.SetSkillEnabled*` (live RPCs) and the `SkillInput(meta)` convenience. |
| `schema/` | Hand-written Go types mirroring the JSON Schema surface. See [Updating the protocol](#updating-the-protocol) for why these are hand-written. |
| `cmd/genschema/` | `go generate` target that runs `codex app-server generate-json-schema` to refresh the raw schema bundle for diffing. |
| `cmd/codexdemo/` | End-to-end smoke test against the real codex CLI. |
| `cmd/capture/` | Records live JSON-RPC transcripts for test fixtures. |

## Updating the protocol

When codex CLI releases a new version with protocol changes:

### 1. Refresh the raw schema bundle

```
go generate ./schema
```

This runs `codex app-server generate-json-schema --out schema/v2_raw`, dumping one JSON Schema Draft 7 file per RPC method (~200 files).

### 2. Diff against the hand-written types

```
# See what changed in the schema
git diff schema/v2_raw/
```

Compare the updated schema files against `schema/types.go`. Look for:
- **New required fields** on existing types (e.g. new fields on `Turn`, `ThreadItem`)
- **New notification methods** in `ServerNotification.json` — these need dispatch in `client.go:dispatchNotification()`
- **New server request methods** in `ServerRequest.json` — these may need approval routing
- **New item types** in the `ThreadItem` oneOf — add type constants and typed fields
- **Changed enums** (new `TurnStatus` values, new `SandboxMode` variants, etc.)

### 3. Update the Go types

Add new fields to the structs in `schema/types.go`. For new notification methods, add:
1. A notification params struct in `schema/types.go`
2. A typed `Event` implementation in `event.go`
3. A dispatch case in `dispatchNotification()` in `client.go`

### 4. Why not code generation?

We evaluated `go-jsonschema` (atombender) and `oapi-codegen` against this schema bundle. Both produce non-idiomatic output: `go-jsonschema` generates ~700 lines per notification type with 40+ custom `UnmarshalJSON` methods; the `oneOf` discriminated unions (used by `ThreadItem`, `SandboxPolicy`, etc.) produce nested type aliases rather than idiomatic Go patterns. `oapi-codegen` targets OpenAPI 3, not raw JSON Schema Draft 7.

The hand-written types are intentionally minimal — they promote the fields consumers need and use `json.RawMessage` for forward compatibility on the rest. This means new codex fields are automatically preserved in `Raw`/`Extra` without breaking callers, and only need explicit promotion when consumers start using them.

## Protocol notes

- `type X json.RawMessage` does **not** inherit `MarshalJSON`/`UnmarshalJSON`. A named alias becomes a bare `[]byte` for the encoding/json runtime, which then base64-decodes the payload. Use a struct wrapper or define the methods explicitly — see `schema.AskForApproval`.
- `turn/completed` arrives with `items: []` even when item events were streamed. Rely on `item/*` notifications for the canonical incremental view, not the `Turn.Items` array.
- `account/rateLimits/updated` is connection-scoped, not thread-scoped — it broadcasts to all subscribers.

## Conventions

Match `claudecli-go`: functional options, sealed `Event` interface, ctx-first, `Executor` abstraction, `Client` / `Conn` split. Tests use in-memory fakes; race-clean.
