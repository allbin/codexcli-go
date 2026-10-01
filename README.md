# codexcli-go

Go client for the [`codex app-server`](https://github.com/openai/codex) JSON-RPC protocol. Mirrors the [`claudecli-go`](https://github.com/allbin/claudecli-go) public API so consumers can swap implementations by changing the import path.

**Status**: pre-1.0. The core protocol surface is covered: initialize, thread start/resume, turn lifecycle, approvals, content deltas (agent message, command output, reasoning, plan), thread status, turn plans, token usage, rate limits, aggregated diffs, MCP server startup status, and skills (discover, toggle, invoke). MCP elicitation, fork, dynamic tools, realtime/audio, and the file/exec/account/plugin RPC surfaces are not yet wired. Tested end-to-end against codex CLI 0.147.0; a normal turn produces no `UnknownEvent`. The [reasoning effort](#reasoning-effort) behaviour below was verified against 0.153.4.

## Install

```
go get github.com/allbin/codexcli-go@v0.9.0
```

Pre-1.0, but tagged from v0.1.0 onward — pin a tag rather than a commit SHA. See the [CHANGELOG](CHANGELOG.md). Requires the `codex` CLI on `PATH` (or override via `WithBinaryPath`) and `codex login` completed once for OAuth.

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
    case *codexcli.TokenUsageUpdatedEvent:
        // Token usage arrives on its own notification, not on the turn.
        fmt.Printf("tokens: %d total\n", e.TokenUsage.Total.TotalTokens)
    case *codexcli.TurnCompletedEvent:
        fmt.Printf("\nstatus=%s duration=%dms\n", e.Turn.Status, *e.Turn.DurationMs)
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

## Mid-turn messages

`Thread.SendMessage` adds a user message to the turn already running, over `turn/steer`. The message lands as a `userMessage` item in that turn, the model sees it at its next step, and events keep arriving on the stream `StartTurn` returned. It returns the turn's id; it never starts a turn.

```go
stream, _ := thread.StartTurn(ctx, "Refactor the parser.")
// ... later, while the turn is still running:
_, err := thread.SendMessage(ctx, "Also keep the public API unchanged.")
switch {
case errors.Is(err, codexcli.ErrNoActiveTurn):
    // Nothing running, or the turn ended before codex got the message.
    stream, err = thread.StartTurn(ctx, "Also keep the public API unchanged.")
case errors.Is(err, codexcli.ErrTurnNotSteerable):
    // A review or compaction turn: codex refuses new input until it ends.
    // Buffer the message and send it after TurnCompletedEvent.
}
```

Do not use `StartTurn` for this. A `turn/start` while a turn is active does not start a turn: codex folds the input into the running turn and returns its id, the new stream takes the thread's events, and the old stream ends without a `TurnCompletedEvent`. During a review or compaction turn codex refuses `turn/start` too, and the stream fails with `ErrTurnNotSteerable`.

`steer_live_test.go` re-checks this against the codex on `PATH` (two small turns and one compaction):

```
go test -tags integration -run TestLive_SendMessage -count=1 -v .
```

## Subagents

Codex subagents are separate threads on the same connection. A parent
learns of one from a `subAgentActivity` item in its turn; the child never
sends `thread/started`, and its first frame can arrive just before that
item. The library holds frames from unnamed threads until a
`subAgentActivity` names them, then delivers every child event on the root
thread's Stream wrapped in `ChildThreadEvent`, which carries the child's
thread id, parent, `agentPath` (`/root/alpha`), spawning tool-call id,
spawn turn, and active turn. Grandchildren route to the same root.
`Thread.Children()` and `Conn.ChildThread(id)` report the tree; an
approval a child asks for reaches the handler with the child's thread id,
so `Conn.ChildThread` says where it came from.

Codex does not stop subagents when their parent's turn is interrupted: on
0.159.3 the child kept running after the parent's turn ended `interrupted`.
`Thread.Interrupt` therefore interrupts every descendant's running turn
first, concurrently and 3s each at most, then the thread's own.

Child events reach the root's current Stream only. Once the parent's turn
completes its Stream ends, and a child still running after that has
nowhere to deliver.

## Thread names

`Thread.SetName` sets the title codex keeps for a thread
(`thread/name/set`), with or without a turn running; codex answers with
`thread/name/updated`, surfaced as `ThreadNameUpdatedEvent`. A name set on
a new thread before its first turn is announced when that turn starts.
Ephemeral
threads keep no metadata: codex refuses and `SetName` returns
`ErrThreadEphemeral`.

## Questions to the user (`request_user_input`)

Codex's ask-the-user tool arrives as the server request
`item/tool/requestUserInput` at the `WithServerRequestHandler` handler.
`req.UserInput()` decodes it. Answer with
`schema.UserInputAnswers(map[questionID][]string)`: one string per chosen
option label or free-text answer. Keys must be the question ids; codex
0.159.3 treats anything else, including `{"answers":{"text":"…"}}`, as no
answer at all. To dismiss, return `schema.UserInputDismissed()`
(`{"answers":{}}`); the model is told it got no answer and the turn goes
on. The tool is only offered in the Plan collaboration mode
(`turn/start` `collaborationMode {mode:"plan"}`, which needs
`WithExperimentalAPI`) or with codex's underDevelopment feature
`default_mode_request_user_input`.

## Reasoning effort

Codex keeps the effort a `turn/start` carries for every later turn on the thread. The SDK layers its own options on top of that, and the two interact in ways that are easy to get wrong. Observed on codex 0.153.4 by reading `reasoning.effort` off the model requests codex sends:

| You call | Sent on `turn/start` | The model runs at |
|---|---|---|
| `StartTurn(ctx, p, WithEffort("high"))` | `"high"` | high, on this turn and every plain turn after it |
| `StartTurn(ctx, p)` on a client built without `WithEffort` | nothing | whatever the thread last ran at |
| `StartTurn(ctx, p)` on a client built with `WithEffort("low")` | `"low"`, every time | low, so a per-call override from the previous turn is gone |
| `StartTurn(ctx, p, WithEffort(""))` | nothing, even with a connect-time level | whatever the thread last ran at; `""` reverts nothing |
| `WithTurnExtra(map[string]any{"effort": nil})` | `null` | unchanged; codex ignores it |

To switch a live session's effort, send the new level on every turn, or once on a client with no connect-time `WithEffort`. There is no reset to the model default. Changing model keeps the effort, so going back means sending the level you want. `Thread.Response().ReasoningEffort` is the level a new thread started with; after `ResumeThread` it is the level the thread was left at, override included.

Pick levels from `schema.Model.SupportedReasoningEfforts`. Codex does not validate them. A level the model rejects (`"minimal"` on gpt-5.6-sol, `"max"` on gpt-5.5) starts the turn and fails it with `invalid_request_error`, and because the level sticks, every later turn fails the same way until one sends a valid level. `"ultra"` is codex's own level: codex stores it as `ultra` and sends the model's highest level (`max` on gpt-5.6-sol, `xhigh` on gpt-5.5).

Reading the level back:

- On a connection opened with `WithExperimentalAPI`, codex sends `thread/settings/updated` whenever a `turn/start` changes a setting, just before the `turn/start` response. It arrives on that turn's stream as `ThreadSettingsUpdatedEvent`. Re-sending the current level produces no event.
- Without `WithExperimentalAPI`, codex sends nothing. The level in force is the last one you sent, or the thread's starting level.

Known limitations:

- **No mid-turn change.** Effort on a `turn/start` that steers a running turn does not reach that turn; it applies from the next turn. On an experimental connection the settings event still fires at steer time with the new level, so for the rest of the running turn it reports a level the model is not using. The experimental `turn/settings/update` does change a running turn, but codex refuses it unless the `step_model_switching` feature is enabled, and that feature is `underDevelopment`.
- **The event needs `experimentalApi`.** Codex 0.153.4 does not send `thread/settings/updated` to stable connections.
- **No between-turn setter.** On experimental connections codex also accepts `thread/settings/update`, which changes effort without starting a turn and, like `turn/start`, accepts any string. The SDK does not wrap it. The event it triggers arrives when no turn is streaming, and the SDK drops events that have no subscriber.

`effort_live_test.go` re-checks the stickiness rules and the event against the codex on `PATH` (it spends seven small turns):

```
go test -tags integration -run TestLive_Effort -count=1 -v .
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

Resolution order for the cache directory: `WithCodexHome(...)` option → `$CODEX_HOME` env var → `$HOME/.codex`.

For live model availability on a running connection, use `Conn.ListModels(ctx)`, which queries the server via the `model/list` RPC and follows pagination to completion. Use `Conn.ListModelsPage` to page manually or to include hidden models.

```go
models, err := conn.ListModels(ctx)
for _, m := range models {
    fmt.Printf("%-20s %s (default=%v)\n", m.ID, m.DisplayName, m.IsDefault)
}
```

**The two registries are different shapes.** The cache file uses snake_case and is typed as `codexcli.ModelInfo` (`Slug`, `Visibility`, `Priority`); the RPC uses camelCase and is typed as `schema.Model` (`ID`, `Hidden`, `IsDefault`). They are not interchangeable — do not unmarshal one into the other.

## Account and rate limits

`RateLimitsUpdatedEvent` only fires while a turn is running, so a usage indicator built on the event stream goes blank the moment nothing is happening. `Conn.AccountRateLimits` is the pull version: it works on a connection that has never started a thread.

```go
conn, _ := client.Connect(ctx)
defer conn.Close()

snap, err := conn.AccountRateLimits(ctx)   // no thread needed
switch {
case errors.Is(err, codexcli.ErrMethodNotSupported):
    // Older codex — render the indicator as "unavailable", not as an error.
case err != nil:
    return err
default:
    fmt.Printf("5h window: %d%% used, resets at %v\n", snap.Primary.UsedPercent, *snap.Primary.ResetsAt)
}
```

The returned `*schema.RateLimitSnapshot` is the same type `RateLimitsUpdatedEvent` carries, so seed from this call on connect and keep refreshing from the event stream during a turn. Each read costs a round trip to OpenAI's backend on the server side — poll it, don't call it per render.

`Conn.Account` reports who codex is signed in as. It never refreshes credentials.

```go
acct, err := conn.Account(ctx)
if errors.Is(err, codexcli.ErrNotSignedIn) {
    // Nobody logged in — prompt for `codex login`.
}
// acct.Type is "chatgpt" | "apiKey" | "amazonBedrock"; Email and PlanType
// are populated for chatgpt only.
```

Both reads return `ErrMethodNotSupported` when the app-server does not implement the method, so a consumer can degrade the feature rather than treat it as a failure. codex rejects an unknown method while deserializing its request union, which surfaces as `-32600 "unknown variant ..."` rather than the spec's `-32601` — the classification handles both.

`Conn.AccountRateLimits` returns the server's backward-compatible single-bucket view. The full reply also carries a per-`limitId` map and a reset-credit block; those decode into `schema.AccountRateLimitsReadResponse` but are not surfaced on `Conn` because nothing needs them yet.

## Which codex will I run, and how do I update it?

`DetectInstall` reports the codex binary that would actually be spawned, how it was installed, and the command that updates *that* install. It is offline and read-only: `exec.LookPath` + `filepath.EvalSymlinks`, package metadata next to the resolved path, codex's standalone-install symlink under `CODEX_HOME`, and one `codex --version`. No subprocess beyond the version probe, no network, no session.

```go
info, err := codexcli.DetectInstall(ctx)
if errors.Is(err, codexcli.ErrCLINotFound) {
    // No codex installed — a normal state, not a failure.
}
if err != nil {
    log.Fatal(err)
}
fmt.Printf("%s (%s, %s)\n", info.RealPath, info.Version, info.Method)
if info.UpdateCmd != "" {
    fmt.Printf("update with: %s\n", info.UpdateCmd)
} else {
    fmt.Println("update manually — no command is known to be correct here")
}
```

**Never treat an empty `UpdateCmd` as "use npm".** Running `npm install -g @openai/codex` against a standalone install writes a *second* complete copy into an npm prefix; whichever copy `PATH` reaches first then answers `codex --version`, so the user is told a version that does not describe the binary their next session runs — and the copy they actually use is still stale. That failure is silent, which is why `InstallUnknown` with no command is the correct answer whenever the evidence is inconclusive.

Update commands were verified against codex 0.148.0 by running `codex update` over synthetic layouts in a throwaway container:

| `Method` | Layout | `UpdateCmd` |
|---|---|---|
| `InstallNative` | `$CODEX_HOME/packages/standalone/releases/<v>` | `codex update` — re-runs the standalone installer |
| `InstallNPMGlobal` | a `node_modules/@openai/codex` tree | `npm install -g` / `pnpm add -g` / `bun install -g …@latest`, per `PackageManager` |
| `InstallPackageManager` | Homebrew cask, winget, mise | `brew upgrade --cask codex`, `winget upgrade OpenAI.Codex`, `mise upgrade codex` (asdf: none) |
| `InstallVersionManager` | an fnm/nvm/volta root, no package metadata | none — the version manager owns the directory |
| `InstallUnknown` | anything else, including a bare binary in a bin dir | none — `codex update` refuses these too |

Two codex-specific wrinkles worth knowing:

- **The binary on `PATH` is not the binary that runs.** For an npm install, `PATH` points at a JS wrapper (`…/@openai/codex/bin/codex.js`) that spawns a vendored musl binary four directories deeper, under `…/node_modules/@openai/codex-linux-x64/vendor/…/bin/codex`. Both walk up to a `package.json` naming `@openai/codex` — the platform sub-package is an npm alias of the same name — so that shared ancestor, not any single directory name, is the primary classifier and both entry points classify identically.
- **`VersionManager` is set even for npm installs.** A global npm install hosted by fnm or nvm only updates for the node version currently active.

`ConfigMismatch` flags the dangerous state: a standalone install exists under `CODEX_HOME` that `PATH` does not reach, so updating the copy you found leaves the copy that runs untouched.

### `Doctor` — codex's own account, and it touches the network

`Doctor` shells out to `codex doctor --json` and returns a typed `*DoctorReport`. Keep it separate from `DetectInstall` in your head: it lets the CLI do everything it does, including a provider WebSocket handshake and a registry lookup for the published version — ~1.4s wall clock on a healthy machine, however long the timeouts take on a broken one. A launch-time probe wants `DetectInstall`; a "diagnose my install" button wants this.

```go
report, err := codexcli.Doctor(ctx)
if err != nil {
    log.Fatal(err) // errors.Is(err, codexcli.ErrDoctorFailed) for "could not run it"
}
fmt.Println(report.OverallStatus, report.CodexVersion)
fmt.Println("published:", report.Updates.LatestVersion, report.Updates.LatestVersionStatus)
if len(report.Installation.PathEntries) > 1 {
    fmt.Println("more than one codex on PATH:", report.Installation.PathEntries)
}
```

Checks failing is a *successful* diagnosis — it comes back as a non-`ok` `OverallStatus`, not an error. `ErrDoctorFailed` means the command could not be run or printed nothing parseable.

**Only the top level of the payload is structured.** Each check's `details` is a map of display label to display value, so every key the typed projections read can be renamed by a codex release that never bumps `schemaVersion`. `DoctorReport.Installation` and `.Updates` therefore degrade to zero values rather than guess, and `DoctorReport.SchemaSupported` reports whether they were filled in at all; `DoctorReport.Checks` always holds the payload verbatim. `GeneratedAt` is kept as a string because codex 0.148.0 emits `"1787550860s since unix epoch"`, not RFC 3339.

`Installation.PathEntries` is the one to surface: more than one copy on `PATH` is exactly the state where a version number stops describing the binary that runs.

### `LatestPublished` — am I behind, in one HTTP request

`Doctor` answers this too, but spends a CLI spawn, DNS and a provider WebSocket getting there. `LatestPublished` reads the release feed directly, choosing it from the detected install.

```go
pub, err := codexcli.LatestPublished(ctx)
if errors.Is(err, codexcli.ErrPublishedUnknown) {
    // No trustworthy source for this install. A correct answer, not a failure —
    // and not worth retrying, unlike a DNS failure or a 503.
}
if err == nil {
    fmt.Printf("%s is published (%s)\n", pub.Version, pub.Source)
    if pub.Status.Known() {
        fmt.Println("you are", pub.Status) // "behind" or "current"
    } else {
        fmt.Println("no verdict:", pub.StatusReason)
    }
}
```

| `Method` | Source | Endpoint |
|---|---|---|
| `InstallNPMGlobal` | `npm-registry` | the `latest` dist-tag for `@openai/codex` — over HTTP, never `npm view` (a server has no npm on PATH) |
| `InstallNative` | `release-channel`, else `github-releases` | `releases.openai.com/codex/channels/latest`, falling back to the GitHub releases API the standalone installer also accepts for the same release |
| `InstallPackageManager` (Homebrew) | `homebrew-cask` | `formulae.brew.sh/api/cask/<cask>.json` — the cask decides what a brew install tracks, so there is no channel to report |
| everything else | — | `ErrPublishedUnknown`: borrowing npm's number for a version-manager install would be a wrong answer dressed as a right one |

**The verdict is three states, not a bool.** `Status` is `behind`, `current`, or unknown, and unknown is the zero value so a half-built result cannot read as good news. It is unknown when either version could not be read or parsed, and when the installed version is a **prerelease**: npm publishes `alpha` and `beta` dist-tags alongside `latest`, and semver would happily call `0.149.0-alpha.4.3` "behind" `0.149.1` — a release it was never following. `Version` is reported in every one of those cases, because "what is published?" stays a fair question when "am I behind?" has no honest answer.

## Updating the install

`Update` runs codex's own updater and tells you what actually happened.

```go
res, err := codexcli.Update(ctx, codexcli.WithUpdateProgress(func(line string) {
    fmt.Println(line) // "==> Downloading Codex CLI", one live line at a time
}))

var manual *codexcli.ManualUpdateError
switch {
case errors.As(err, &manual):
    // Not ours to update. A normal outcome, and the common one.
    // manual.Command is verbatim and may be empty — never fill it in.
case errors.Is(err, codexcli.ErrUpdateNotWritable):
    // Cannot, as opposed to tried and failed: don't offer the button at all.
case err != nil:
    // res is still non-nil here: a half-run update has numbers worth showing.
}
if res != nil && res.Changed {
    fmt.Printf("%s → %s\n", res.VersionBefore, res.VersionAfter)
}
```

Four things it deliberately will not get wrong:

- **The standalone install, and an npm install whose prefix is proven.** Everything else (pnpm, bun, Homebrew, version-manager roots, unknown binaries, and any npm install the proof refuses) comes back as `*ManualUpdateError` carrying `InstallInfo.UpdateCmd` verbatim. `codex update` shells out to whatever `npm` is on `PATH`, and codex reports `managed package root` and `npm update target` as separate facts because they can differ. When they do, that "update" writes a second copy whose visibility depends on `PATH` order. So for npm this package runs `<prefix>/bin/npm` (the npm beside the node that owns `lib/node_modules/@openai/codex`, never a `PATH` lookup) and only when that npm's `npm prefix -g` holds the same package root the `PATH` entry resolves into. It then pins `npm view @openai/codex@latest version`, installs that exact version, and requires the `PATH` entry to report it afterwards. `ManualUpdateError.Reason` names the step that refused.
- **`InstallInfo.SelfManaged` is the same verdict.** `Client.DetectInstall` sets it from the check `Update` makes, including write access to the npm prefix's `lib/node_modules` and `bin`, so a consumer can show an Update button exactly where `Update` will act. For npm-global installs detection therefore costs one `npm prefix -g` run.
- **It executes the `PATH` entry** detection recorded — not the bare word `codex` (a second `exec.LookPath` can reach a different copy), and not the resolved path (that is the release the update is about to supersede).
- **Success is verified by re-reading the version, never by the exit code.** `codex update` was observed exiting 0 and printing "Update ran successfully!" while the command it shells out to was not installed at all. Believe `Changed`, not `ExitCode`.
- **A writability preflight runs first**, over the two directories the installer actually writes — `<CODEX_HOME>/packages/standalone/releases` and `$CODEX_INSTALL_DIR` (default `~/.local/bin`). Neither is necessarily the directory holding the binary on `PATH`.

Verified end-to-end against codex 0.148.0 → 0.149.1 on a sandboxed `CODEX_HOME`: the real installer ran, the version moved, `Changed` came back true.

## Per-thread MCP servers

`WithThreadConfig` overlays `config.toml` values for the threads a connection starts or resumes. It is sent as the `config` field of `thread/start` and `thread/resume`, over the subprocess's stdin, so a secret in it never reaches argv the way a `-c key=value` argument would (`/proc/<pid>/cmdline` is world-readable).

```go
conn, err := codexcli.New().Connect(ctx, codexcli.WithThreadConfig(map[string]any{
    "mcp_servers": map[string]any{
        "agentique": map[string]any{
            "url":          mcpURL,
            "http_headers": map[string]any{"Authorization": "Bearer " + token},
            // Run this server's tools without an approval round-trip.
            "default_tools_approval_mode": "approve",
        },
    },
}))
thread, err := conn.NewThread(ctx)
servers, err := conn.ListMcpServerStatus(ctx, thread.ID) // what this thread sees
```

Repeated `WithThreadConfig` calls, including client defaults from `New`, deep-merge: nested maps merge key by key, a later non-map value wins. A connect-time config is re-sent on `ResumeThread`. A `"config"` key in `WithThreadExtra` replaces the whole map. A stdio server takes `command`, `args` and `env` instead of `url`; `bearer_token_env_var` or `env_http_headers` plus `WithEnv` keeps the token out of the request too.

`Conn.ListMcpServerStatus(ctx, threadID)` wraps `mcpServerStatus/list` and follows pagination. Each `schema.McpServerStatus` carries `Name`, `Tools` (by name; `ToolNames()` sorts them), `AuthStatus`, `RuntimeStatus`, `ToolsError`, `ServerInfo`, and `Raw`. Without a thread ID it lists only the process-level servers, not an overlay.

Observed against codex 0.156.1 by `mcp_config_live_test.go` (in-test HTTP MCP stub, sandboxed `CODEX_HOME`, repeated runs):

- The overlay adds to the servers `config.toml` defines; it does not replace them.
- The token reached the MCP server as `Authorization: Bearer <token>`, appeared in no process's cmdline, and was written to no file under `CODEX_HOME` (rollout, sqlite, config.toml).
- Threads on one connection, and on separate connections, each reached only their own server with their own token. A resumed thread gets only the config sent with `thread/resume`.
- Codex asks before every MCP tool call, as an `mcpServer/elicitation/request` (`_meta.codex_approval_kind: "mcp_tool_call"`) that reaches `WithServerRequestHandler`, not `WithApprovalHandler`. Answer `{"action":"accept","content":{}}` to run it. Unanswered the call fails with "user rejected MCP tool call"; under approval policy `never` it fails with "MCP tool call requires approval, but approval policy is never". `default_tools_approval_mode = "approve"` on the server skips the ask under either policy.

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

### Host-supplied skill directories

`config.toml` has no key for an extra skill directory; the only route is the `skills/extraRoots/set` RPC, wrapped by `Conn.SetSkillsExtraRoots`. Codex scans each root for `<root>/<name>/SKILL.md` and reports what it finds with scope `user`.

```go
if err := conn.SetSkillsExtraRoots(ctx, []string{"/srv/sessions/abc/skills"}); err != nil {
    log.Fatal(err)
}
thread, err := conn.NewThread(ctx) // first turn sees the skills
```

Read from codex source (commit e72da2b, 2026-09-26), not yet observed live:

- **Connection-scoped.** The roots sit on the one skills service the connection's thread manager owns, so every thread on the connection sees them, including threads already running: the next turn picks them up. A per-session directory therefore needs one connection per session; unlike `WithThreadConfig`, this cannot be isolated per thread.
- **Replace, not append.** Each call assigns the whole list and drops codex's skills cache. Resend the full set to add a root; an empty or nil slice clears every runtime root.
- **`skills/changed` follows every call**, delivered as `SkillsChangedEvent`. Re-run `Conn.ListSkills` afterwards if you hold a cached list.
- **Roots must be absolute.** Codex rejects relative paths at decode time; the wrapper checks `filepath.IsAbs` first and returns an error naming the offending root without sending anything.

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

Read a policy back with `policy.Granular()` (returns the toggles and `true` for the object form) or `policy.AskForApprovalString()` (the bare-string form: `schema.ApprovalUntrusted`, `schema.ApprovalOnRequest`, `schema.ApprovalNever`). Older codex releases also accepted `"on-failure"`; it was dropped from the enum.

## Events

The event stream surfaces typed events for the full server notification set:

| Event | Server notification | Description |
|---|---|---|
| `TurnStartedEvent` | `turn/started` | Turn accepted, generation starting |
| `TurnCompletedEvent` | `turn/completed` | Turn finished (completed/interrupted/failed) |
| `ItemStartedEvent` | `item/started` | Item lifecycle begins |
| `ItemCompletedEvent` | `item/completed` | Item finished with final state, plus `CompletedAtMs` |
| `AgentMessageDeltaEvent` | `item/agentMessage/delta` | Streaming assistant text |
| `ContentDeltaEvent` | `item/commandExecution/outputDelta`, `item/fileChange/outputDelta`, `item/reasoning/textDelta`, `item/reasoning/summaryTextDelta`, `item/plan/delta` | Streaming content — discriminate on `Kind` |
| `ReasoningSummaryPartAddedEvent` | `item/reasoning/summaryPartAdded` | A new reasoning summary block opened at `SummaryIndex` |
| `FileChangePatchUpdatedEvent` | `item/fileChange/patchUpdated` | In-progress change set for a `fileChange` item |
| `TurnDiffUpdatedEvent` | `turn/diff/updated` | Aggregated unified diff for the turn |
| `TurnPlanUpdatedEvent` | `turn/plan/updated` | The agent's todo plan, resent in full on every change |
| `ThreadStatusChangedEvent` | `thread/status/changed` | Thread went active/idle — brackets every turn |
| `ThreadSettingsUpdatedEvent` | `thread/settings/updated` | Settings the next turn runs with (effort, model, ...), sent when a `turn/start` changes one. Experimental connections only; see [Reasoning effort](#reasoning-effort) |
| `ContextCompactedEvent` | `thread/compacted` | Codex summarised earlier history to fit the context window |
| `TokenUsageUpdatedEvent` | `thread/tokenUsage/updated` | Token usage snapshot — the only source of usage since 0.14x |
| `RateLimitsUpdatedEvent` | `account/rateLimits/updated` | Rate limit status (broadcast to all subscribers) |
| `McpServerStatusEvent` | `mcpServer/startupStatus/updated` | MCP server startup progress (broadcast to all subscribers) |
| `ModelReroutedEvent` | `model/rerouted` | Codex switched models mid-turn |
| `WarningEvent` | `warning`, `guardianWarning` | User-facing advisory that is not a turn failure |
| `ConfigWarningEvent` | `configWarning` | A problem in the user's `config.toml`, raised at connect (broadcast) |
| `DeprecationNoticeEvent` | `deprecationNotice` | A protocol surface is going away — log these (broadcast) |
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

Each call gets its own context. It is cancelled when codex withdraws the
request (`serverRequest/resolved`), when the request's turn completes, or
when the connection closes. Interrupting a turn with an approval pending
triggers the first two: codex 0.159.3 sends `turn/completed` with status
`interrupted`, then `serverRequest/resolved`. Take the prompt off
the screen when the context ends; the handler's return value for a
withdrawn request is discarded. `WithServerRequestHandler` handlers get the
same context.

## Liveness

`Conn.Ping` sends `thread/loaded/list` and waits for the answer, so a
codex process that is running but no longer serving requests fails it
with `ErrPingTimeout`. Codex answers it in about a millisecond while idle,
mid-generation, with an approval pending, and while a command runs. Any
answer, including an error response, counts as alive.

Silence on the event stream proves nothing either way. While a command
runs and prints nothing, codex sends no notifications at all until it
finishes: no progress, no heartbeat. `item/mcpToolCall/progress` exists
for MCP tools that report progress, and is not typed by this library yet
(it arrives as `UnknownEvent`). Ping is the way to tell a long silent
command from a wedged app-server.

`Conn.Done()` closes when the process has exited, with `ExitError` set, so
an idle connection with no Stream open can still learn that codex died.

## Command output

Codex app-server streams a `commandExecution` item's stdout/stderr via
`item/commandExecution/outputDelta` notifications. It *usually* also sets
`aggregatedOutput` on the completed item — but not always (long or
truncated output, PTY-backed commands, and interrupted turns have all been
observed to leave it null), so a consumer that only reads
`aggregatedOutput` will intermittently show nothing.

Pass `WithAccumulatedOutput()` to have the SDK buffer those deltas keyed by
item id and populate `ItemCompletedEvent.Item.AggregatedOutput` from the
buffer when the server left it empty (a non-empty server value always wins).
The per-item buffer is drained on completion. Without the option, items pass
through unchanged and you reconstruct output from `ContentDeltaEvent`
(`Kind == ContentDeltaCommandOutput`) yourself.

## Architecture

| File | Role |
|---|---|
| `client.go` | `Client`, `Conn` — spawn codex, run the handshake, dispatch requests, route notifications to per-thread subscribers. Panic recovery on approval/request handlers. |
| `rpc.go` | JSON-RPC 2.0 framing over line-delimited JSON. Outbound request/response correlation by id; inbound dispatch to notify + request callbacks. Error chain preservation. |
| `executor.go` | `Executor` interface + `LocalExecutor`. Swap in fakes for tests or remote execution. Cancellation kills the whole process tree (codex + MCP servers + shell children): `Setpgid` + `kill(-pid, SIGTERM)` on unix, a kill-on-close job object + `TerminateJobObject` on Windows (`executor_windows.go`), where every spawn is also marked CREATE_NO_WINDOW. |
| `shim.go` | Resolves npm's Windows `.cmd` shim to the `bin/codex.js` it wraps, so the executor can run node on it directly (os/exec refuses batch-file args it cannot safely escape). |
| `option.go` | Functional options (`WithCwd`, `WithModel`, `WithEphemeralThread`, ...). Per-thread config overlay via `WithThreadConfig`. Extras hatch via `WithThreadExtra` / `WithTurnExtra`. |
| `event.go` | Sealed `Event` interface + the turn/item lifecycle events. |
| `event_lifecycle.go` | Thread-, plan-, and connection-level events added for codex 0.14x (status, plan, warnings, MCP startup, reroute). |
| `stream.go` | `Stream` — channel-of-events with lifecycle tracking and `Wait()` for blocking callers. |
| `thread.go` | `Thread` — start additional turns on the same conversation. |
| `approval.go` | Sealed `ApprovalRequest` / `ApprovalDecision` interfaces, typed approval routing. |
| `models.go` | `ListModels` (file-based) and `Conn.ListModels` (live RPC). |
| `install.go` | `DetectInstall` — offline, read-only classification of the codex binary on `PATH` and the command that updates it. |
| `doctor.go` | `Doctor` — typed projection of `codex doctor --json`. Touches the network; keep it off the launch path. |
| `published.go` | `LatestPublished` — the published version for an install's own release stream, in one HTTP request. Three-state verdict; never compares across streams. |
| `update.go` | `Update` — runs codex's own updater for a standalone install, the prefix's own npm for a proven npm install, refuses the rest with the command to display, and verifies by re-reading the version. |
| `npm_update.go` | The npm-global proof: the prefix's own npm, its `npm prefix -g` matched against the package root `PATH` runs, and the `SelfManaged` verdict shared with detection. |
| `mcp.go` | `Conn.ListMcpServerStatus` / `Conn.ListMcpServerStatusPage` (live RPC). Per-thread MCP servers are configured with `WithThreadConfig`. |
| `skills.go` | `Conn.ListSkills` / `Conn.SetSkillEnabled*` / `Conn.SetSkillsExtraRoots` (live RPCs) and the `SkillInput(meta)` convenience. |
| `schema/` | Hand-written Go types mirroring the JSON Schema surface: `types.go` (core), `notifications.go` (server notification payloads), `approvals.go`, `skills.go`, `model.go`, `mcp.go`. See [Updating the protocol](#updating-the-protocol) for why these are hand-written. |
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
1. A method constant and params struct in `schema/notifications.go`
2. A typed `Event` implementation in `event_lifecycle.go`
3. A dispatch case in `dispatchNotification()` in `client.go`
4. A case in `event_lifecycle_test.go` — the tests drive `dispatchNotification` directly with a raw payload, so no fixture plumbing is needed

### 4. Verify against the live CLI

The schema bundle is necessary but not sufficient — it does not tell you which
fields are actually populated, and it lists experimental-only fields alongside
stable ones. Record a real transcript and read it:

```
go run ./cmd/capture -prompt "Run 'echo hi' and reply in one sentence." -out /tmp/live.jsonl
```

This is how the 0.147 bump caught three regressions the schema diff alone had
made look cosmetic: `Turn.usage` vanishing, `commandActions.cmd` renaming to
`command`, and `model/list` renaming `models` to `data`. The last two were
silent — the fields simply decoded to empty.

Finish by confirming a normal turn produces no `UnknownEvent`.

### 5. Why not code generation?

We evaluated `go-jsonschema` (atombender) and `oapi-codegen` against this schema bundle. Both produce non-idiomatic output: `go-jsonschema` generates ~700 lines per notification type with 40+ custom `UnmarshalJSON` methods; the `oneOf` discriminated unions (used by `ThreadItem`, `SandboxPolicy`, etc.) produce nested type aliases rather than idiomatic Go patterns. `oapi-codegen` targets OpenAPI 3, not raw JSON Schema Draft 7.

The hand-written types are intentionally minimal — they promote the fields consumers need and use `json.RawMessage` for forward compatibility on the rest. This means new codex fields are automatically preserved in `Raw`/`Extra` without breaking callers, and only need explicit promotion when consumers start using them.

## Protocol notes

- `type X json.RawMessage` does **not** inherit `MarshalJSON`/`UnmarshalJSON`. A named alias becomes a bare `[]byte` for the encoding/json runtime, which then base64-decodes the payload. Use a struct wrapper or define the methods explicitly — see `schema.AskForApproval`.
- `Turn.Items` is never the full picture: `turn/started` sends `itemsView: "notLoaded"` with an empty array, and `turn/completed` sends `itemsView: "summary"` with only the final assistant message. Rely on `item/*` notifications for the canonical incremental view.
- `Turn.Usage` was removed from the wire after 0.133 and is always nil. Token usage only arrives via `thread/tokenUsage/updated` (`TokenUsageUpdatedEvent`), which fires more than once per turn — the last one wins.
- `account/rateLimits/updated`, `mcpServer/startupStatus/updated`, `skills/changed`, and `deprecationNotice` are connection-scoped, not thread-scoped — they broadcast to all subscribers.
- `commandActions` entries renamed their command field from `cmd` to `command`. `schema.CommandAction` decodes both and mirrors the value onto `Command` and the deprecated `Cmd`.
- `model/list` returns `{data, nextCursor}` — not `{models}`, as it did on 0.133 — and its entries are a camelCase shape unrelated to `models_cache.json`.

## Upgrading to codex 0.147

Three changes need consumer action; the rest are additive.

| Was | Now | Why |
|---|---|---|
| `conn.ListModels(ctx)` returned `[]json.RawMessage` | returns `[]schema.Model` | The RPC renamed `models` to `data`, so the old call silently returned nil. Entries are the camelCase RPC shape, **not** `ModelInfo`. |
| `e.Turn.Usage.Total.TotalTokens` | handle `*TokenUsageUpdatedEvent` | `usage` was removed from the `Turn` schema; the field is now always nil. |
| `action.Cmd` | `action.Command` | The wire key renamed `cmd` to `command`. `Cmd` still works — it is kept mirrored — but is deprecated. |

`schema.CommandExecutionRequestApprovalParams.AdditionalPermissions` and
`.AvailableDecisions` are now only sent when the connection opts in via
`WithExperimentalAPI()`; they decode as empty otherwise.

## Windows notes

- **Job-object assignment happens post-start** — os/exec offers no
  `CREATE_SUSPENDED` path, so the codex process is placed in the
  kill-on-close job just after `Start()` returns. A child codex spawned in
  that window would escape the job; in practice codex takes far longer than
  that to start MCP servers. If job creation or assignment fails, the
  executor degrades to killing only the codex process instead of failing
  the spawn.
- **npm's `codex.cmd` shim is bypassed** — when the layout confirms it wraps
  `@openai/codex`, the executor runs node on the wrapped `bin/codex.js`
  directly; os/exec refuses to start batch files with arguments cmd.exe
  cannot safely escape (the CVE-2024-24576 hardening). Falls back to running
  the shim when node is missing or the layout is unconfirmed.
- **Cancelling `Update` kills without grace** — unix cancellation sends
  SIGINT to the updater's process group so a staged download can be unwound
  before the kill lands; Windows has no console interrupt deliverable from a
  windowless parent, so cancellation there is an immediate job-object tree
  kill. A cancelled Windows update may leave a staged partial download for
  the installer to clean up on its next run.
- **An external kill reads as a crash** — there are no signals, so a codex
  terminated from outside (Task Manager, `taskkill`) exits with a plain code
  and `ProcessExitError.Reason` reports `crashed`, not `killed`.
  SDK-initiated termination still reports `context_canceled`.
- **No Windows CI** — the console-suppression, job-object and shim-bypass
  paths need a manual smoke test on real Windows: spawn a connection, cancel
  it, and confirm no survivors in Task Manager; same for a cancelled
  `Update`. The shim resolver itself is unit-tested on linux.

## Conventions

Match `claudecli-go`: functional options, sealed `Event` interface, ctx-first, `Executor` abstraction, `Client` / `Conn` split. Tests use in-memory fakes; race-clean.
