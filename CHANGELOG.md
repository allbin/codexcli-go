# Changelog

All notable changes to `codexcli-go` are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Install the latest release with:

```
go get github.com/allbin/codexcli-go@latest
```

or pin a specific version (e.g. `@v0.12.0`).

## [Unreleased]

### Fixed

- **Cancelling `Update` on unix no longer kills a started `npm install -g`,
  which could leave no codex on PATH.** Cancellation sent SIGINT to npm's
  process group and killed it five seconds later. npm answers SIGINT by
  rolling back, and the rollback can take longer than that; a kill during
  it left no `bin/codex`, only a stray `bin/.codex-*` link and
  `lib/node_modules/@openai/.codex-*`. Reproduced through `Update` against
  throwaway prefixes, npm 11.19.0 / node 24, 0.160.0 → 0.160.1: 3 of 37
  cancels 1–5.5 s into npm broke the install on a normally busy machine,
  and 6 of 6 under synthetic CPU and disk load, every one killed exactly
  five seconds after the cancel. Now a started npm install is never killed
  by cancellation, as on Windows since 0.12.0: on unix SIGINT is still
  sent, `Update` waits for npm to exit and reports what it did, and only an
  npm still running ten minutes after the cancellation is killed (five
  seconds after a last SIGINT). A cancelled call therefore returns when
  npm's rollback ends — under two seconds idle, up to about seven and a
  half under load — rather than within five; its worst case, for an npm
  that ignores the signal, is ten minutes after the cancellation rather
  than five seconds. After the change, 28 of 28 cancels idle and 12 of 12 under
  the same load left a working codex, 8 of those with npm still rolling
  back 5.1–7.3 s after the SIGINT. A cancellation before `npm install`
  starts still returns at once.
- An npm install that finished although `Update` was cancelled is reported
  as the success it is; on unix it came back as an `ErrUpdateFailed`.
  npm can also exit 1 on a late SIGINT with the new version already in
  place: that stays a failure, with `Changed` and `VersionAfter` saying so.

### Changed

- A failure caused by cancelling `Update` also matches `context.Canceled`
  or `context.DeadlineExceeded`, alongside `ErrUpdateFailed`.
- A cancelled standalone `codex update` keeps SIGINT followed by a kill
  five seconds later; `Update`'s doc now says why that is safe. Checked
  through `Update` in a sandboxed `HOME`/`CODEX_HOME`, 0.160.0 → 0.160.1:
  47 cancels from the start of the download to the end of unpacking, 18
  of them under CPU and disk load, all stopped within two seconds of the
  SIGINT with 0.160.0 still current, so the kill never landed; the installer switches releases by
  rename only once the new one is unpacked. It leaves a
  `releases/.staging.*` directory, which its next run removes.
- `TestLive_NPMUpdateCancel` and `TestLive_StandaloneUpdateCancel` repeat
  these checks against throwaway installs.

## [0.12.0] - 2026-10-06

`Update` acts for an npm-global install on Windows. The layout, the proof,
locked files and cancellation were verified on Windows 11 (node 24.20.0,
npm 11.6.0, codex 0.159.0 → 0.160.1) against the real install, read-only,
and throwaway prefixes, three runs each.

### Added

- **Windows npm-global installs are self-managed when proven.** The `.cmd`
  shim on PATH must run `%dp0%\node_modules\@openai\codex` beside it; npm is
  resolved once to an absolute path from the child's PATH, as 0.11.0's
  fallback does (normally `C:\Program Files\nodejs\npm.cmd`, else an
  `npm.cmd` in the prefix), and must be npm's own `npm.cmd` — running
  `node_modules\npm\bin\npm-cli.js` beside it — so Volta's `npm.exe` is
  refused; that npm's `prefix -g` must be the shim's directory, holding the package
  root the shim runs — compared after resolving symlinks and 8.3 names, as
  exact paths and with `os.SameFile`. Anything else stays manual with the
  reason named. `InstallInfo.SelfManaged` and `Update` share the verdict,
  writability of `<prefix>\node_modules` and `<prefix>` included. A prefix
  under a junction, or whose path has a UUID-shaped segment (npm 11 prints
  those as `***`), does not resolve and is refused. The install carries
  `--prefix <reported>`, as 0.11.0's PATH npm does: nvm-windows derives
  the prefix from the node directory it repoints on a switch.
- **`ErrUpdateInUse` / `UpdateInUseError`.** On Windows, immediately before
  npm starts, every file in the package tree and the codex shims is probed;
  one held by another process — a running codex.exe — refuses the update
  with nothing installed. Without it npm renames the tree aside under the
  running binary, installs the new version, exits 0 and leaves the old tree
  behind as `node_modules\@openai\.codex-<hash>`. `SelfManaged` does not
  account for it: it clears when the process exits.
- `TestLive_NPMUpdateWindowsThrowaway` (integration, opt-in with
  `CODEXCLI_LIVE_NPM_THROWAWAY`): mismatch refused, running codex blocked,
  update to latest with no second copy and nothing left behind.

### Changed

- **A started Windows `npm install -g` is not killed when `Update`'s context
  is cancelled.** A job-object kill a few seconds in left no `codex.cmd` on
  PATH and a half-extracted package, because npm retires the old tree and
  shims before downloading the new one and only rolls back on a signal
  Windows cannot deliver. `Update` waits for npm and reports what it did,
  bounded at ten minutes from start. A cancellation before npm starts still
  stops there. Unix is unchanged.
- `withPathPrefix` replaces a `Path` override case-insensitively on Windows
  instead of adding a second PATH beside it.

## [0.11.0] - 2026-10-06

`Update` acts for an npm-global install that a system npm writes through a
user-level prefix, the usual no-sudo setup. Checked live on npm 11.19.0.

### Changed

- **`Update` and `InstallInfo.SelfManaged` accept an npm-global install
  whose prefix has no npm of its own** — a system node with a user-level
  prefix in `.npmrc` (`prefix=~/.local`), the usual way to install globally
  without sudo. These were refused as manual before. When nothing exists at
  `<prefix>/bin/npm`, the first `npm` on the subprocess `PATH` (`WithEnv`'s
  `PATH` if set) is resolved once and run by that absolute path for
  `prefix -g`, `view` and `install -g`. It must resolve to npm's own
  `bin/npm-cli.js`, so Volta, asdf, mise and corepack shims stay manual
  (Volta answers `prefix -g` from npm's config but installs `-g` into its
  own image). Its `#!` node must be an executable file, and that node's
  directory goes first on the child's `PATH`. Its `install -g` carries
  `--prefix <what prefix -g reported>`, so a node switch between proof and
  install cannot move an execPath-derived prefix. The `prefix -g` match against
  the package root `PATH` runs is unchanged, as is everything when
  `<prefix>/bin/npm` exists. Checked on a system npm 11.19.0 / node 24 with
  `prefix=~/.local`: detection reports `SelfManaged` true (three runs), and
  a real update in a temp prefix moved 0.160.0 → 0.160.1 with no other
  prefix touched, while a client configured for another prefix was refused
  (three runs, `TestLive_NPMUpdateThrowawayPrefix`).
- A prefix whose path holds a UUID-shaped segment stays manual, on either
  npm: `npm prefix -g` prints that segment as `***`, so it resolves to
  nothing. This was already so; `Update`'s doc now says it.

## [0.10.0] - 2026-10-02

Threads can be deleted. What codex removes was checked live on 0.160.0,
three runs.

### Added

- **`Conn.DeleteThread(ctx, threadID)`** over `thread/delete`, with
  `ThreadDeletedEvent` for `thread/deleted`. On codex 0.160.0 it removes
  the thread's rollout file and its rows in `thread_history_1.sqlite`
  (`thread_items`, `thread_turns`, `thread_history_projection_state`) and
  `state_5.sqlite` (`threads`); only `logs_2.sqlite`'s tracing log still
  mentions the id, and a shell command's output was in no table
  afterwards. Deleting a thread also deletes the subagent threads it
  spawned, from the connection that ran them or from a fresh one.
  `ErrThreadNotFound` for an unknown or already deleted thread, including
  a subagent deleted with its parent. The deleted thread and its
  subagents leave the connection's bookkeeping (`Thread.Children`,
  `Conn.ChildThread`), so take `Children()` first.

## [0.9.0] - 2026-10-01

Subagents become visible and stop with their parent, threads can be
renamed, and request_user_input is typed. Codex behaviour checked live on
0.159.3, three runs each.

### Added

- **Subagent events on the parent's Stream.** Codex subagents are sibling
  threads on the connection, and every frame they sent was dropped. Child
  events now arrive on the root thread's Stream as `ChildThreadEvent`
  (`Child` identifies the child: thread id, parent, `AgentPath`,
  spawning `ToolCallID`, `SpawnTurnID`, `ActiveTurnID`; `Event` is the
  child's event). Frames that arrive before the parent's
  `subAgentActivity` names the child are held and replayed after it.
  Grandchildren route to the same root. A child's `TurnCompletedEvent`
  does not end the Stream. Events from a child after its root's Stream
  has ended are dropped.
- **`Thread.Children()`** and **`Conn.ChildThread(id)`** report the
  subagent tree this connection has seen.
- **`Thread.SetName(ctx, name)`** over `thread/name/set`, with
  `ThreadNameUpdatedEvent` for `thread/name/updated`. `ErrThreadEphemeral`
  for an ephemeral thread, which codex keeps no metadata for;
  `ErrThreadNotFound` for an unknown one.
- **request_user_input types.** `ServerRequest.UserInput()` decodes the
  request into `schema.ToolRequestUserInputParams` (questions with `ID`,
  `Header`, `Question`, `IsOther`, `IsSecret`, `Options`; `IsBlocking`).
  `schema.UserInputAnswers` builds the reply keyed by question id;
  `schema.UserInputDismissed` (`{"answers":{}}`) dismisses. A reply keyed
  any other way, such as `{"answers":{"text":"…"}}`, reaches the model as
  no answer, the same as a dismissal.
- `schema.ThreadItem.SubAgentActivity()` projection and the
  `SubAgentActivity*` kinds (codex 0.159.3 adds `completed`).

### Changed

- **`Thread.Interrupt` stops subagents.** Codex leaves a subagent running
  when its parent's turn is interrupted (observed twice on 0.159.3), so
  Interrupt now interrupts every descendant's running turn first,
  concurrently and each bounded by 3s, then the thread's own.
- **`Thread.Interrupt` with no active turn sends nothing** and returns
  nil. Codex 0.159.3 rejects `turn/interrupt` without a turn id
  ("missing field `turnId`"), so it had started returning an error,
  contrary to its doc.

## [0.8.1] - 2026-10-01

### Fixed

- **v0.8.0 does not build** (`thread.go`: cannot receive from non-channel
  `*subscription`): its tag missed one line of the subscription change.
  v0.8.0 is retracted; use v0.8.1, which is v0.8.0 as intended.

## [0.8.0] - 2026-10-01 [YANKED]

Retracted: the tag does not build. Use 0.8.1, which ships these changes.

Robustness for long-running hosts: withdrawn approvals are cancelled, Ping
makes a real round trip, and event delivery no longer drops events.
Behaviour checked live against codex 0.159.3, three runs each.

### Added

- **Per-request handler contexts.** `ApprovalFunc` and `ServerRequestFunc`
  each get their own ctx, cancelled when codex withdraws the request
  (`serverRequest/resolved`), when the request's turn completes, or when
  the connection closes. Before this the ctx was the connection's, so a
  host that showed an approval prompt kept it after the user pressed Stop:
  codex withdrew the request and nothing told the handler. A withdrawn
  request's answer is never sent: the handler and the withdrawal claim the
  request under one lock, so exactly one wins. Requests are registered on
  the read loop, so a withdrawal in the very next frame still lands. A
  legacy v1 approval, which carries a call id rather than a turn id, is
  withdrawn when any turn on its thread completes. Codex 0.159.3 sends
  `turn/completed` (interrupted) and then `serverRequest/resolved` on
  interrupt.
- **`Conn.Done()`** closes once the process has exited and `ExitError` is
  set, so an idle connection with no Stream learns that codex died.
- **`ErrPingTimeout`**, returned by `Ping` when codex does not answer.
- `schema.MethodServerRequestResolved` and
  `schema.ServerRequestResolvedNotification`. The notification is consumed
  and no longer surfaces as an `UnknownEvent`.

### Changed

- **`Conn.Ping` round-trips.** It sends `thread/loaded/list` (limit 1),
  an in-memory read, and returns nil on any answer, error responses
  included. A running app-server that stops answering now fails with
  `ErrPingTimeout` at the caller's timeout, and one whose process dies
  mid-probe returns its `ProcessExitError`; before, Ping only checked the
  process had not exited and passed a wedged app-server forever. Codex
  answered in about 1ms idle, mid-generation, with an approval pending
  and during a silent 15s command. A zero timeout now means 5s (was 1s).
  If codex stops reading stdin, Ping still returns at the timeout and
  leaves one goroutine blocked in the write until the process exits.
- **Event delivery is lossless.** Each Stream's subscription is an
  unbounded queue drained by its own goroutine; the read loop appends and
  never blocks. Before, the reader dropped an event whenever 64 were
  waiting, so a burst of output deltas could drop `turn/completed` and
  leave a Stream open forever, or drop an `item/completed`. The
  `ProcessExitEvent` is always the last event, after everything queued
  before it. A consumer that stops reading without closing its Stream now
  grows memory instead of losing events. Delivery to the subscriber is
  asynchronous: an event is queued when the notification is read and
  handed over by the pump.

### Fixed

- **`ResumeThread` returns `ErrThreadNotFound`** for codex 0.159.3's
  "no rollout found for thread id …", which it answers for an unknown or
  deleted thread. It was a generic error.

### Notes

- Codex sends nothing while a command runs silently: no progress and no
  heartbeat until `item/completed`. `item/mcpToolCall/progress` (MCP tool
  progress messages) and `item/commandExecution/terminalInteraction`
  (stdin written to an interactive command) exist and are not typed yet;
  both arrive as `UnknownEvent`. Neither is a liveness signal for a plain
  command; use `Ping`.

## [0.7.0] - 2026-10-01

Mid-turn messages over `turn/steer`, with distinct errors for "nothing to
inject into" and "this turn refuses input". Checked live against codex
0.159.3.

### Added

- **`Thread.SendMessage(ctx, prompt)` / `SendMessageWithInput(ctx, input)`**
  add a user message to the running turn over `turn/steer`, with
  `ActiveTurnID()` as `expectedTurnId`, and return that turn's id. The
  message arrives as a `userMessage` item on the stream `StartTurn`
  returned. Mirrors `claudecli-go.Session.SendMessage`.
- **`ErrNoActiveTurn`**: no turn is running, or it ended before codex
  received the message (`no active turn to steer`, or an
  `expected active turn id` mismatch). Nothing was delivered.
- **`ErrTurnNotSteerable`**: the running turn is a review or compaction
  turn (`codexErrorInfo.activeTurnNotSteerable`). Returned by
  `SendMessage`, and by `StartTurn`'s stream, since codex refuses
  `turn/start` during those turns too.
- `schema.TurnSteerParams`, `schema.TurnSteerResponse`,
  `schema.MethodTurnSteer`.
- `BidiFixtureExecutor.SendErrorResponseData` scripts an error response
  with a `data` payload.
- **`steer_live_test.go`** (build tag `integration`):
  `go test -tags integration -run TestLive_SendMessage -count=1 -v .`

### Changed

- `StartTurn` documents that a `turn/start` while a turn is active folds
  into that turn and returns its id. Use `SendMessage` mid-turn.
- **A Stream replaced by a later `StartTurn` on the same thread now ends**
  (`Wait` returns `ErrNoTurn`). It used to stay open with no events until
  its context was cancelled, and cancelling it then closed the newer
  Stream's subscription, so the newer Stream lost every event after that.

### Fixed

- **Send on a closing channel.** The reader delivered to a Stream's
  channel outside the lock `unsubscribe` closes it under, so a Stream
  closed by its context while a notification for its thread arrived could
  panic the reader goroutine with "send on closed channel". Found by the
  race detector.
- **`ActiveTurnID` no longer revives a finished turn.** When a turn's
  `turn/completed` was read before its `turn/start` response, the response
  set the finished turn as active again, and `Interrupt` and `SendMessage`
  then targeted it until the next turn started.

## [0.6.0] - 2026-09-26

Host-supplied skill directories: the `skills/extraRoots/set` RPC, which is
the only way to hand codex a skill root config.toml cannot express.
Semantics read from codex source at e72da2b, not yet observed live.

### Added

- **`Conn.SetSkillsExtraRoots(ctx, roots)`** wraps `skills/extraRoots/set`, the
  only way to give codex a skill directory config.toml does not know about.
  Connection-scoped and replace-not-append per codex source at e72da2b; a
  `skills/changed` notification follows each call. Roots must be absolute
  and are checked client-side. New schema types
  `SkillsExtraRootsSetParams` / `SkillsExtraRootsSetResponse` and the
  `MethodSkillsExtraRootsSet` constant.

## [0.5.0] - 2026-09-25

A typed per-thread config overlay, so a consumer can give each thread its
own MCP servers, and a wrapper to read back which servers and tools a
thread sees. Checked against codex 0.156.1 with an in-test MCP server.

### Added

- **`WithThreadConfig(cfg map[string]any) Option`** sets the `config` field
  of `thread/start` and `thread/resume`, which codex applies like `-c`
  overrides for that thread only. Repeated calls, client defaults
  included, deep-merge: nested `map[string]any` values merge key by key, a
  later non-map value wins. Inputs are copied. A connect-time config is
  re-sent on `ResumeThread`. It is not sent on `turn/start`, and a
  `"config"` key in `WithThreadExtra` still replaces it. The config travels
  over stdin, never argv, so it can carry a bearer token.
- **`Conn.ListMcpServerStatus(ctx, threadID)`** wraps `mcpServerStatus/list`
  and follows pagination; `Conn.ListMcpServerStatusPage` sends one request.
  New schema types: `McpServerStatus` (`Name`, `Tools`, `AuthStatus`,
  `RuntimeStatus`, `ToolsError`, `ServerInfo`, `PluginId`, raw
  `Resources`/`ResourceTemplates`/`ServerCapabilities`, `Raw`, and
  `ToolNames()`), `McpTool`, `McpServerInfo`, `McpAuthStatus`,
  `McpServerConnectionStatus`, `ListMcpServerStatusParams`/`Response`,
  `McpServerStatusDetail`, and `MethodMcpServerStatusList`.
- **`mcp_config_live_test.go`** (build tag `integration`) runs an in-test
  streamable-HTTP MCP server in a sandboxed `CODEX_HOME`:
  `go test -tags integration -run TestLive_ThreadConfig -count=1 -v .`

### Documentation

- **What codex 0.156.1 does with an MCP overlay.** A nested `mcp_servers`
  overlay adds to the servers in `config.toml`. The token reached the server
  as the `Authorization` header, appeared in no process cmdline, and was
  written to no file under `CODEX_HOME`. Codex asks before every MCP tool
  call with an `mcpServer/elicitation/request` (`WithServerRequestHandler`,
  not `WithApprovalHandler`). An unanswered request fails the call, and so
  does approval policy `never`. `default_tools_approval_mode = "approve"` on
  the server runs its tools without asking.

## [0.4.0] - 2026-09-17

`Update` now updates an npm-global install when it can prove the npm it runs
writes the tree `PATH` executes, and `InstallInfo.SelfManaged` reports that
verdict. Also a typed read-back for reasoning-effort and other sticky turn
settings, and documentation of how a live effort change behaves, verified
against codex 0.153.4 by reading `reasoning.effort` off the model requests
codex sends. No change to what the SDK sends on the wire.

### Added

- **`Update` acts for a proven npm-global install** (npm, unix). It runs
  `<prefix>/bin/npm`, the npm beside the node that owns
  `lib/node_modules/@openai/codex`, never one found on `PATH`, with
  `<prefix>/bin` first on the child's `PATH`. It acts only when that npm's
  `npm prefix -g` holds the same package root the `PATH` entry resolves
  into. It resolves `npm view @openai/codex@latest version`, runs nothing
  when that is installed, otherwise installs that exact version, and
  requires the `PATH` entry to report it afterwards: a clean npm exit
  without that version is `ErrUpdateFailed`. Preflight probes
  `<prefix>/lib/node_modules` and `<prefix>/bin`; a failure is
  `ErrUpdateNotWritable`. A prefix mismatch or anything unresolvable stays
  `ErrManualUpdate`. pnpm, bun, Homebrew, winget, version-manager roots and
  unknown installs are unchanged.
- **`InstallInfo.SelfManaged`**, set by `Client.DetectInstall` from the same
  check `Update` makes: true for the standalone install and for an npm-global
  install that is proven and writable. For npm-global installs detection now
  runs `npm prefix -g` and a write probe.
- **`ManualUpdateError.Reason`** names the step of the npm proof that refused.
- **`UpdateResult.Updater`**, the executable that ran (`codex` or the prefix's
  npm). `UpdateResult.Path` stays the codex binary whose version is reported.
- **`npm_update_live_test.go`** (build tag `integration`) checks the proof
  against the codex on `PATH`; `CODEXCLI_LIVE_NPM_UPDATE=1` also runs the
  update.
- **`ThreadSettingsUpdatedEvent`** decodes `thread/settings/updated`. It
  carries typed `ThreadID`, `Model`, `ModelProvider`, `Effort`, `Summary`,
  and `ServiceTier` (empty for null) plus `SettingsRaw`, the full snapshot.
  Decode that with the new `schema.ThreadSettings` for cwd, approval, and
  sandbox fields. `schema.ThreadSettingsUpdatedNotification` and
  `schema.MethodThreadSettingsUpdated` back it. Codex 0.153.4 sends the
  notification only to connections opened with `WithExperimentalAPI`, and
  only when a `turn/start` changes a setting.
- **`effort_live_test.go`** (build tag `integration`) routes codex through a
  local proxy and asserts the effort on its model requests. Run it on a
  codex bump: `go test -tags integration -run TestLive_Effort -count=1 -v .`

### Changed

- **`thread/settings/updated` no longer surfaces as `*UnknownEvent`.**
  Upgrade note: code that matched `UnknownEvent{Method:
  "thread/settings/updated"}` must switch to `*ThreadSettingsUpdatedEvent`.

### Documentation

- **`WithEffort` spells out the stickiness rules.** Codex keeps a
  `turn/start` effort for later turns. A per-call `WithEffort` therefore
  sticks, unless the client was built with `WithEffort`, which the SDK
  re-sends on every turn, so the override lasts one turn. `WithEffort("")`
  sends nothing and reverts nothing. There is no reset to a default: codex
  ignores `effort: null` and keeps the effort across a model change. Codex
  does not validate levels, so a level the model rejects fails that turn and
  every later one until a valid level is sent. `"ultra"` is sent to the
  model as its highest level. Effort on a steering `turn/start` applies from
  the next turn, not the running one. The README has a new
  [Reasoning effort](README.md#reasoning-effort) section with the same
  rules as a table, and `WithModel`, `Thread.StartTurn`, and
  `Thread.Response` point at them.

## [0.3.2] - 2026-09-07

One fix: `UnwrapShellCommand` handles the double-quoted `bash -lc "…"`
envelope codex uses whenever a command contains a single quote, so
`CommandLiteral()` no longer leaks the outer quotes to consumers. No API
changes.

`SDKVersion` is `0.3.2` (was `0.3.1`).

### Fixed

- **`UnwrapShellCommand` now strips a double-quoted body.** Codex wraps a
  command in `bash -lc "…"` instead of `'…'` whenever the command itself
  contains a single quote — `sed -n '1,240p' f` and `rg -n 'x' .` are the
  everyday cases — and the unwrapper only knew the single-quoted form, so
  `ThreadItem.CommandLiteral()` returned the body with its outer double
  quotes still attached. Consumers surfaced that verbatim as the tool row's
  command (agentkit's codex connector showed `"sed -n '1,240p' f"`). Both
  wrappers are now stripped, decoding the escapes each permits (`'\''`
  inside single quotes; `\"`, `\\`, `\$`, `` \` `` and line continuations
  inside double quotes). Bare bodies (`bash -lc ls`) are unchanged.

## [0.3.1] - 2026-09-02

Process-lifecycle hardening, most of it Windows-specific: cancellation now
tears down codex's whole process tree instead of the single PID, the npm
`codex.cmd` shim no longer blocks spawns with awkward arguments, and the
`--version` probe stops flashing a console window. No API changes; Windows
builds gain a `golang.org/x/sys` dependency.

`SDKVersion` is `0.3.1` (was `0.3.0`).

### Fixed

- **Cancelling a connection now kills codex's whole process tree.**
  Previously context cancellation killed only the codex process — the MCP
  servers and turn shell commands it spawned survived until their own
  stdin-EOF handling exited them, or forever if wedged. `LocalExecutor` now
  confines each spawn: on unix the child gets its own process group
  (`Setpgid`) and cancellation sends SIGTERM to the group, with a kill after
  a 5s unwind; on Windows each spawn is placed in an anonymous kill-on-close
  job object — cancel calls `TerminateJobObject` (tree kill), and closing
  the handle after `Wait()` reaps stragglers that outlive a clean codex
  exit. If job creation or assignment fails, the executor degrades to the
  old single-PID kill rather than failing the spawn. Known caveat:
  assignment happens just after `Start()` (os/exec has no `CREATE_SUSPENDED`
  path), so a child forked in that instant could escape — in practice codex
  takes far longer to start MCP servers. There is no Windows CI, so the job
  path needs a manual smoke test on real Windows.

  Upgrade note: adds a `golang.org/x/sys` dependency (Windows builds only);
  no API changes.

- **Windows: the `--version` probe now suppresses its console window.** The
  executor, `Doctor` and `Update` spawns already set CREATE_NO_WINDOW, but
  the probe `DetectInstall` and `Update` run to read the installed version
  did not — so a windowless parent (a service, a GUI app) flashed a console
  on screen for each detection.

- **Windows: npm's `codex.cmd` shim is bypassed.** When the resolved binary
  is npm's cmd.exe shim and the layout confirms it wraps `@openai/codex`,
  the executor runs node on the wrapped `bin/codex.js` directly — os/exec
  refuses to start batch files with arguments cmd.exe cannot safely escape
  (the CVE-2024-24576 hardening), so an argument containing `%` or `"`
  failed at `Start()` behind a shim. The bypass also removes the cmd.exe
  layer from the process tree. Falls back to running the shim when node is
  missing or the layout is unconfirmed. The resolver is platform-neutral
  (`shim.go`) and unit-tested on linux; the live path needs a manual smoke
  test on real Windows.

- **Cancelling `Update` interrupts the whole process group.** On unix,
  SIGINT now goes to the updater's process group (previously the single
  codex process), so any children unwinding a staged download are
  interrupted too. On Windows — where no console interrupt is deliverable
  from a windowless parent — cancellation is an immediate job-object tree
  kill instead of a lone `codex.exe` kill that orphaned children; a
  cancelled Windows update may leave a staged partial download for the
  installer to clean up on its next run.

## [0.3.0] - 2026-08-27

Adds the two idle-connection reads a usage indicator needs: who is signed in,
and how much quota is left, neither of which required a running thread.
Additive: nothing existing changed shape.

`SDKVersion` is `0.3.0` (was `0.2.0`).

### Added

- **`Conn.AccountRateLimits(ctx)`** — reads the rate-limit snapshot on demand
  via `account/rateLimits/read`, returning the same `*schema.RateLimitSnapshot`
  that `RateLimitsUpdatedEvent` carries.

  The event only fires while a turn is running, so anything built on it goes
  blank when the connection is idle. This is the pull counterpart: it works on
  a connection that has never started a thread, which is what a usage
  indicator needs.

  Verified end-to-end against codex 0.148.0 on a live ChatGPT account with no
  thread open: both windows, plan type, credits and spend-control state came
  back populated.

- **`Conn.Account(ctx)`** — reads the signed-in account via `account/read`.
  Returns the new `*schema.Account`, a union discriminated on `Type`
  (`chatgpt` / `apiKey` / `amazonBedrock`); `Email` and `PlanType` are
  populated for `chatgpt` only. It never asks the server to refresh
  credentials.

- **`ErrMethodNotSupported`** — returned by both reads when the app-server
  does not implement the method, so a consumer degrades the feature to
  "unavailable" instead of reporting a failure.

  codex rejects an unrecognised method while deserializing its request union,
  so it answers `-32600 "Invalid request: unknown variant ..."` rather than
  the JSON-RPC spec's `-32601`. Classification matches both, plus the
  `requires experimentalApi capability` gate — and deliberately does *not*
  match a plain `-32600` for malformed params, which stays a real error.

- **`ErrNotSignedIn`** — returned by `Conn.Account` when the server reports a
  null account, rather than a nil-nil return the caller has to remember to
  check.

- **`schema.AccountRateLimitsReadResponse`** models the full reply, including
  the per-`limitId` bucket map and the reset-credit block. Neither is surfaced
  on `Conn` yet; the reset-credit shape stays `json.RawMessage` because it is
  still moving between codex releases.

- **`BidiFixtureExecutor.SendErrorResponse`** for scripting server-side
  rejections in tests.

## [0.2.0] - 2026-08-24

Closes the last asymmetry with `claudecli-go`, which shipped these two in
v0.6.0/v0.7.0. Additive: nothing existing changed shape.

`SDKVersion` is `0.2.0` (was `0.1.0`). It is sent as
`initialize.params.clientInfo.version` and is now also the User-Agent
`LatestPublished` identifies itself with.

### Added

- **`Update`** — runs codex's own updater for the install on PATH, and reports
  what actually happened.

  Verified end-to-end against codex 0.148.0 → 0.149.1 on a synthetic standalone
  install in a sandboxed `CODEX_HOME`: the real installer ran, the version
  moved, `Changed` came back true.

  Four things it refuses to get wrong, each of them learned rather than
  designed:

  - **Only the standalone install is updated from here.** Every other method
    comes back as `*ManualUpdateError` carrying `InstallInfo.UpdateCmd`
    **verbatim** — a normal outcome, and the common one in the wild. An empty
    `Command` means "tell the user to update manually" and must never be filled
    in with a guess. This is narrower than `codex update` itself, which also
    shells out to `npm install -g @openai/codex` for a node-managed install:
    codex reports `managed package root` and `npm update target` separately
    because they can differ, and when they do that "update" writes a second
    copy whose visibility depends on PATH order.

  - **It executes the PATH entry** recorded by detection, never the bare word
    `codex` (a second `exec.LookPath` can reach a different copy — `Doctor`
    reports two on an ordinary machine) and never the symlink-resolved path
    (which is the very release the update supersedes).

  - **Success is verified by re-reading the version, never by the exit code.**
    `codex update` was observed exiting 0 and printing "Update ran
    successfully!" while the command it shells out to was not installed at all.
    `VersionBefore`/`VersionAfter`/`Changed` are the answer; `ExitCode` is
    diagnostics.

  - **A writability preflight** returns `ErrUpdateNotWritable` before anything
    runs, because "cannot" and "tried and failed" render differently: one never
    offers the button, the other shows an error after a click. It probes the two
    directories the installer actually writes —
    `<CODEX_HOME>/packages/standalone/releases` and `$CODEX_INSTALL_DIR`
    (default `~/.local/bin`) — neither of which is necessarily the directory
    holding the binary on PATH.

  The result is returned **alongside** the error, never instead of it: a
  half-run update still has before/after numbers worth rendering.

  Surface: `Update(ctx, opts...)`, `(*Client).Update`, `UpdateResult`,
  `WithUpdateProgress` (plain-text lines, one per line the installer prints),
  `WithUpdateOutput`, `WithUpdateTimeout`, `ErrManualUpdate` /
  `ManualUpdateError`, `ErrUpdateNotWritable` / `UpdateNotWritableError`,
  `ErrUpdateFailed` / `UpdateFailedError`.

- **`LatestPublished`** — the published version for an install's own release
  stream, in one HTTP request.

  `Doctor` already answers this, but spends a CLI spawn, DNS and a WebSocket to
  the model provider (~1.2s) getting there. This reads the registry or release
  feed directly, chosen from the detected install: the npm registry's dist-tags
  for node-managed installs (never `npm view` — a server has no npm on PATH),
  codex's own `releases.openai.com/codex/channels/latest` for standalone with
  the GitHub releases API as the mirror the installer also accepts, and the
  Homebrew cask API for brew. Everything else returns `ErrPublishedUnknown`.

  **The verdict is three states, not a bool.** `Published.Status` is `behind`,
  `current`, or unknown — and unknown is the zero value, so a half-built result
  cannot read as good news. `Status.Known()` gates anything reassuring, and
  `StatusReason` says why there is no verdict. `Version` is reported either way,
  because "what is published?" stays a fair question when "am I behind?" has no
  honest answer. `claudecli-go` shipped a bare `UpdateAvailable bool` in v0.6.0
  and had to fix it in v0.7.0; the codex-specific version of that trap is the
  `alpha`/`beta` dist-tags, where semver would happily call a prerelease install
  "behind" a release it never tracked.

  `ErrPublishedUnknown` means "no trustworthy source for this install, and no
  substitute would be correct" — never "the lookup failed". A DNS failure or a
  503 is an ordinary wrapped error, because that one is worth retrying and this
  one never is.

  Surface: `LatestPublished(ctx, opts...)`, `(*Client).LatestPublished`,
  `Published`, `VersionStatus` (`VersionStatusUnknown`, `VersionStatusCurrent`,
  `VersionStatusBehind`) with `Known()`, `PublishedSource`,
  `ErrPublishedUnknown` / `PublishedUnknownError`, `WithPublishedHTTPClient`,
  `WithPublishedTimeout`.

### Notes for consumers

- `agentkit` maps these onto `InstallUpdatable` / `UpdateInstall` and
  `PublishedVersionReportable` / `PublishedVersion`. Its `manual` and `blocked`
  outcomes are `ErrManualUpdate` and `ErrUpdateNotWritable`; its `unverified` is
  a non-nil result with an empty `VersionAfter`.
- Tests need no codex on PATH: classification stays pure and the ambient
  operations (writability probe, updater exec, version probe, HTTP client) are
  injected, mirroring `claudecli-go`'s `updateEnv` / `installEnv`.

## [0.1.0] - 2026-08-24

First tagged release. Earlier consumers pinned commit SHAs; pin `@v0.1.0` from
here on. No breaking changes — everything below is additive.

### Added

- **`DetectInstall`** — read-only detection of how the `codex` CLI on PATH was
  installed, plus the command that updates *that* install.

  A host that wants to tell a user "you're on 0.148.0, 0.149.1 is published"
  also has to tell them how to update, and getting that wrong does not fail
  cleanly: `npm install -g @openai/codex` against a standalone install writes a
  second, complete copy into an npm prefix, and whichever copy PATH reaches
  first from then on is the one that answers `codex --version`. The version
  probe stops describing the binary that actually runs, and the copy the user
  reaches is still stale. Because that failure is silent, `InstallUnknown` with
  an empty `UpdateCmd` is the deliberate answer whenever the evidence is
  inconclusive.

  Offline and read-only: `exec.LookPath` + `filepath.EvalSymlinks`, package
  metadata next to the resolved path, codex's standalone-install symlink under
  `CODEX_HOME`, and one `codex --version`. No network, no session, no writes.

  Surface: `CLIPackageName`, `InstallMethod` (`InstallNPMGlobal`,
  `InstallVersionManager`, `InstallPackageManager`, `InstallNative`,
  `InstallUnknown`), `InstallSource`, `InstallInfo`, `ErrCLINotFound`,
  `CLINotFoundError`, `DetectInstall(ctx, opts...)` and
  `(*Client).DetectInstall`. Mirrors `claudecli-go`'s `install.go` so the two
  libraries read alike.

  Update commands were verified against codex 0.148.0 by running `codex update`
  over synthetic layouts in a throwaway container:

  | `Method` | Layout | `UpdateCmd` |
  |---|---|---|
  | `InstallNative` | `$CODEX_HOME/packages/standalone/releases/<v>` | `codex update` — re-runs the standalone installer |
  | `InstallNPMGlobal` | a `node_modules/@openai/codex` tree | `npm install -g` / `pnpm add -g` / `bun install -g …@latest`, per `PackageManager` |
  | `InstallPackageManager` | Homebrew cask, winget, mise | `brew upgrade --cask codex`, `winget upgrade OpenAI.Codex`, `mise upgrade codex` (asdf: none) |
  | `InstallVersionManager` | an fnm/nvm/volta root, no package metadata | none — the version manager owns the directory |
  | `InstallUnknown` | anything else, including a bare binary in a bin dir | none — `codex update` refuses these too |

- **`Doctor`** — `codex doctor --json` projected into a typed `*DoctorReport`,
  with `Installation` and `Updates` covering the two checks a consumer needs:
  the executable codex is actually running, which package manager owns it, the
  published version, and every codex copy found on PATH.

  Kept a separate entry point from `DetectInstall` because it **touches the
  network** — a provider WebSocket handshake and a registry lookup, ~1.4s wall
  clock on a healthy machine, however long the timeouts take on a broken one.
  Choose deliberately: a launch-time probe wants `DetectInstall`, a "diagnose
  my install" button wants this. Observed not to modify anything under
  `CODEX_HOME` across repeated runs, but that is an observation, not a
  guarantee codex offers.

  Surface: `Doctor(ctx, opts...)`, `(*Client).Doctor`, `DoctorReport`,
  `DoctorCheck`, `DoctorInstallation`, `DoctorUpdates`, `DoctorSchemaVersion`,
  `DoctorCheckInstallation`, `DoctorCheckUpdates`, `ErrDoctorFailed`.

- `(*Client).binaryPath` semantics are now exercised: `WithBinaryPath` is
  honoured by both new entry points, and a non-local `Executor` falls back to
  `codex`.

### Notes for consumers

- **The binary on PATH is not the binary that runs.** For an npm install, PATH
  points at a JS wrapper (`…/@openai/codex/bin/codex.js`) that spawns a vendored
  musl binary four directories deeper. Both walk up to a `package.json` naming
  `@openai/codex` — the platform sub-package is an npm alias of the same name —
  so that shared ancestor is the primary classifier and both entry points
  classify identically. `InstallInfo.RealPath` is the wrapper;
  `DoctorReport.Installation.CurrentExecutable` is the vendored binary. They
  differ by design, not by disagreement.

- **`InstallInfo.VersionManager` is set even for npm installs.** A global npm
  install hosted by fnm or nvm only updates for the node version currently
  active.

- **`InstallNative` means codex's standalone-installer layout only**, not any
  standalone binary. A bare executable in an ordinary bin dir is
  `InstallUnknown` with no command: codex itself reports it as "other" and
  `codex update` refuses it outright. This deliberately differs from
  `claudecli-go`, where a bare executable *is* native.

- **`DoctorReport` details are display labels, not an API.** Each check's
  `details` is a `map[string]string` of human-readable label to human-readable
  value, so any key can be renamed by a codex release that never bumps
  `schemaVersion`. The typed projections degrade to zero values rather than
  guess; check `DoctorReport.SchemaSupported`, and read `DoctorReport.Checks`
  for the payload verbatim. `GeneratedAt` is kept as a string because codex
  0.148.0 emits `"1787550860s since unix epoch"`, not RFC 3339.

- **Windows classification is unverified on real hardware.** A `.cmd`/`.bat`/
  `.ps1` shim is not a symlink and cannot be resolved further, so unless a
  sibling npm layout confirms the install it is reported as `InstallUnknown`
  rather than guessed at.

- **The winget package id `OpenAI.Codex` does not come from codex.** OpenAI
  publishes no winget package and the codex binary contains no winget strings;
  the id comes from the winget community repository. Everything else
  (`brew upgrade --cask codex`, the npm/pnpm/bun commands) is codex's own
  vocabulary.

### Changed

- `SDKVersion` is `0.1.0` (was `0.0.2`). It is sent as
  `initialize.params.clientInfo.version`, so codex-side logs will show the new
  value.
- README documents both entry points and gains an `install.go` / `doctor.go`
  row in the architecture table.

[Unreleased]: https://github.com/allbin/codexcli-go/compare/v0.12.0...HEAD
[0.12.0]: https://github.com/allbin/codexcli-go/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/allbin/codexcli-go/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/allbin/codexcli-go/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/allbin/codexcli-go/compare/v0.8.1...v0.9.0
[0.8.1]: https://github.com/allbin/codexcli-go/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/allbin/codexcli-go/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/allbin/codexcli-go/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/allbin/codexcli-go/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/allbin/codexcli-go/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/allbin/codexcli-go/compare/v0.3.2...v0.4.0
[0.3.2]: https://github.com/allbin/codexcli-go/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/allbin/codexcli-go/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/allbin/codexcli-go/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/allbin/codexcli-go/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/allbin/codexcli-go/releases/tag/v0.1.0
