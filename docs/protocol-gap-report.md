# Codex app-server protocol gap report

What the `codex app-server` JSON-RPC protocol offers, what `codexcli-go` exposes today, and
what it would take to close the difference.

**Measured against**: `codex-cli 0.148.0` (the binary on `PATH` at the time of writing; the
repo's fixtures and docs currently target 0.147). Where 0.147 and 0.148 could differ, the
claim is marked.

## How each claim was verified

Every fact below carries one of these tags. Nothing here is inferred from documentation
alone.

| Tag | Meaning |
| --- | --- |
| `[live]` | Observed on a running `codex app-server` stdio session against this machine's account. |
| `[schema]` | Read out of `codex app-server generate-ts --experimental`, which the binary emits from its own Rust types. |
| `[capture]` | Read out of a recorded wire trace (ours, or t3code's `codexMultiAgentWire.json`). |
| `[t3code]` | Read out of t3code's client, treated as corroboration, not as proof. |
| `[unverified]` | Stated but not proven. Treat as a lead. |

Regenerating the primary sources:

```
codex app-server generate-ts --experimental --out /tmp/ts-exp   # full surface
codex app-server generate-ts --out /tmp/ts-stable               # non-experimental surface
codex app-server generate-json-schema --experimental --out /tmp/schema-exp
```

The diff between those two `--out` directories is the exact set of methods gated behind
`experimentalApi`.

## Executive summary

The adapter's capability list is mostly wrong in the conservative direction. Five of the six
"unsupported" capabilities are real protocol features that this library has not wired up.

| Adapter claim | Verdict | Protocol surface |
| --- | --- | --- |
| Subagents unsupported | **Unimplemented, not a limit** | Child threads run on the same connection; `subAgentActivity` + `collabAgentToolCall` items, per-child `turn/*` `[live]` |
| MidTurnSendMessage unsupported | **Unimplemented, not a limit** | `turn/steer` injects into the running turn. So does `turn/start` while a turn is active `[live]` |
| No compaction events | **Wrong** | `contextCompaction` item is what 0.148 emits; `thread/compacted` still exists but is deprecated `[live]` |
| Fork unsupported | **Unimplemented, not a limit** | `thread/fork`, with `lastTurnId` / `beforeTurnId` truncation `[live]` |
| ToolProgressTicks unsupported | **Partly a limit** | `item/mcpToolCall/progress` exists for MCP calls only; no generic per-tool tick `[schema]` |
| MaxTurns unsupported | **Genuine limit, but see `tokenBudget`** | No turn cap exists anywhere. `thread/goal/set` does offer a token budget with a `budgetLimited` terminal state `[schema]` |

Coverage today: **8 of 144 client request methods**, **27 of 74 server notifications**,
**7 of 11 server requests**.

---

## A. `experimentalApi: true`

**What it unlocks**: 46 additional client request methods and one additional server request.
It does not change the notification surface at all — `ServerNotification` is byte-identical
between the stable and experimental generators `[schema]`.

**How the gate behaves** `[live]`. Calling a gated method without the capability returns a
specific error rather than method-not-found:

```
thread/queue/list       ERROR -32600: thread/queue/list requires experimentalApi capability
collaborationMode/list  ERROR -32600: collaborationMode/list requires experimentalApi capability
mock/experimentalMethod ERROR -32600: mock/experimentalMethod requires experimentalApi capability
```

With `capabilities: {experimentalApi: true}` in `initialize`, the same calls get through to
real argument validation:

```
thread/queue/list       ERROR -32603: no rollout found for thread id 0000...
collaborationMode/list  OK {"data":[{"name":"Plan",...},{"name":"Default",...}]}
mock/experimentalMethod OK {"echoed":null}
```

`mock/experimentalMethod` is a purpose-built probe the server ships for exactly this check
— it is the cheapest way to assert the handshake took effect.

**There is no separate "V2 method surface" behind the flag.** The v1/v2 split is orthogonal:
v2 is the current type namespace (`thread/*`, `turn/*`, `item/*`), v1 is the legacy
`initialize` shape. Both generators emit both. `experimentalApi` gates individual methods,
not a version.

The flag also gates **fields**, not just methods. The granular form of `askForApproval` is
rejected on `thread/start` without it — already documented at `schema/types.go:114`.

**Do we send it?** Only on request. `codexcli.WithExperimentalAPI` (`option.go:79`) sets it,
and `resolveOptions` omits the whole `capabilities` block unless something opted in
(`option.go:265`). So the default is off. t3code sends `experimentalApi: true`
unconditionally in `buildCodexInitializeParams` `[t3code]`.

**Recommendation**: nothing on Agentique's roadmap needs the flag today — child threads,
`turn/steer`, `thread/fork` and compaction are all in the stable set. Leave the default off.
It becomes necessary if you want `thread/queue/*` (a real pending-message queue, see D) or
`thread/turns/list`.

---

## B. Child threads and agent fleets

The highest-value gap. Codex's subagents are **full app-server threads on the same
connection**, distinguished only by `threadId`.

### It is on by default

`experimentalFeature/list` `[live]`:

```
stable  enabled=true   default=true   multi_agent
stable  enabled=false  default=false  multi_agent_v2
removed enabled=false  default=false  multi_agent_mode
```

I captured a two-child fleet twice — once with `--enable multi_agent_v2` and once without —
and **the wire shape is identical** `[live]`. Agentique does not need the flag. (`ThreadStartParams.multiAgentMode` is deprecated in 0.148: *"Ignored. Use Ultra reasoning effort for proactive multi-agent behavior."* `[schema]`)

### Lifecycle, as captured live

Prompt: *"spawn two subagents in parallel: one replies ALPHA, one replies BETA"*. Root
thread `…e2c7`, children `…f8ae` and `…018c`. Noise frames elided:

```
  3 [ROOT ] thread/started            parentThreadId=null  source="vscode"
  7 [ROOT ] turn/started              turn=…08364c
 28 [ROOT ] item/completed agentMessage "I'll run the two requested agents in parallel…"
 29 [CHILD1] thread/status/changed    idle          <-- first sight of the child
 30 [ROOT ] item/started  subAgentActivity {kind:"started", agentThreadId:"…f8ae", agentPath:"/root/alpha"}
 33 [CHILD1] thread/status/changed    active
 34 [CHILD1] turn/started             turn=…5cbfa9  <-- child's own turn id
 38 [CHILD2] thread/status/changed    idle
 39 [ROOT ] item/started  subAgentActivity {kind:"started", agentThreadId:"…018c", agentPath:"/root/beta"}
 43 [CHILD2] turn/started             turn=…1b7de4
 50 [CHILD1] item/completed agentMessage "ALPHA"
 54 [CHILD1] turn/completed           turn=…5cbfa9
 55 [ROOT ] item/started  collabAgentToolCall {tool:"wait", status:"inProgress", senderThreadId:"…e2c7"}
 62 [CHILD2] item/completed agentMessage "BETA"
 66 [CHILD2] turn/completed           turn=…1b7de4
 84 [ROOT ] item/completed agentMessage "The two agents replied:\n\n- ALPHA\n- BETA"
 88 [ROOT ] turn/completed            turn=…08364c
```

### How a child is created and correlated

**Children never emit `thread/started`.** `[live]`, and matching t3code's 0.145 capture
`[capture]`. This is the single most important fact for anyone building a roster.

The registration signal is the parent-side item:

```jsonc
{ "type": "subAgentActivity",
  "id": "call_…",              // the model's tool-call id
  "kind": "started",           // "started" | "interacted" | "interrupted"
  "agentThreadId": "01a022d5-f8ae-…",
  "agentPath": "/root/alpha" } // hierarchical; parent is "/root"
```

`agentPath` is the only human-readable name on the wire in this capture — depth and identity
are both encoded in it. `SubAgentActivityKind` is exactly `started | interacted | interrupted`
`[schema]`.

Correlation to a parent turn: the `subAgentActivity` item arrives inside the parent's
`item/started` / `item/completed`, which carry `threadId` **and** `turnId`. That parent
`turnId` is the fleet's spawn turn. t3code stamps it on every synthetic child event so
separate fleets in one thread don't collapse into one group `[t3code]`.

**The ordering trap**: the child's first `thread/status/changed` (frame 29) arrives *before*
the parent's `subAgentActivity` announcing it (frame 30). A registry that only learns about
children from `subAgentActivity` is always one frame late, and the frame it misses is the
one that would have told it the thread exists. Buffer unknown-thread notifications briefly,
or resolve retroactively.

### Two other correlation paths

`Thread.parentThreadId` — *"only set if this thread is a subagent"* — plus `agentNickname`,
`agentRole`, and `sessionId` (*"shared by threads that belong to the same session tree"*)
`[schema]`. These are populated on `thread/read` and `thread/list`, not on the live stream,
since children never announce themselves.

`thread/list` accepts `parentThreadId` (direct children) or `ancestorThreadId` (descendants
at any depth, excluding the ancestor) `[schema]`, both accepted live without `experimentalApi`
`[live]`. That is the cheap way to reconstruct a roster after a reconnect.

`SessionSource` carries the spawn record on stored threads `[schema]`:

```ts
// v2 (current)
{ "subAgent": { "thread_spawn": { parent_thread_id, depth, agent_path,
                                  agent_nickname, agent_role } } }
// v1 (legacy) uses the key "subagent" — note the casing difference
```

Outer key is camelCase, inner keys snake_case. t3code reads exactly this shape `[t3code]`,
though it flags its own path as probe-gated and unconfirmed against a live binary.

### `collabAgentToolCall` under-delivers

The schema advertises a rich roster item: `tool` (`spawnAgent | sendInput | resumeAgent |
wait | closeAgent`), `receiverThreadIds`, `prompt`, `model`, `reasoningEffort`, and
`agentsStates` mapping thread id to `{status, message}` where status is `pendingInit |
running | interrupted | completed | errored | shutdown | notFound` `[schema]`.

In both live captures, only `tool: "wait"` ever appeared, and `receiverThreadIds` and
`agentsStates` were **both empty** `[live]`. No `spawnAgent` item was emitted for either
spawn. Do not build the roster on `collabAgentToolCall` — build it on `subAgentActivity`
plus per-child `thread/status/changed`, and treat `agentsStates` as a bonus when non-empty.

### Interrupt ordering

`TurnInterruptParams = { threadId, turnId }` `[schema]` — there is no thread-tree-wide
interrupt. Children are independent threads with independent turns, so interrupting the
parent leaves the fleet running `[t3code]`.

Correct sequence:

1. Interrupt every live **child** turn first, using the child's own `threadId` and the
   `turnId` from that child's `turn/started`.
2. Then interrupt the parent.

Bound step 1. The transport awaits a reply per request, so a wedged child would block the
parent interrupt indefinitely — precisely during the runaway fleet where stopping matters.
t3code uses a 3s per-child timeout, 10s overall, concurrency 8, then interrupts the parent
unconditionally `[t3code]`. That is a sound default.

`[unverified]`: I did not interrupt a live fleet, so the *observable effect* of the ordering
(what the parent does when its children die mid-`wait`) is not confirmed. The parameter
shape and the necessity of per-child calls are confirmed.

### Why codexcli-go drops all of this today

`Conn.deliver` (`client.go:521`) resolves a subscriber channel by thread id:

```go
ch := c.subs[threadID]
if ch == nil {
    return
}
```

Only threads created through `NewThread` / `ResumeThread` are registered in `c.subs`
(`client.go:508`). Child threads never are, so **every child-thread notification is silently
discarded** — no `UnknownEvent`, no log line. The parent's `subAgentActivity` and
`collabAgentToolCall` items do get delivered, but `schema.ThreadItem` projects no fields for
either; both are reachable only through `Raw` (`schema/types.go:592`).

So the roster is invisible on two independent counts, and the drop is the harder one.

---

## C. Compaction

**The adapter's claim is wrong, and the library's typing is stale.**

`thread/compacted` exists in the notification union, and `codexcli-go` types it
(`schema/notifications.go:23`, `ContextCompactedEvent`). But its params type is annotated
*"Deprecated: Use `ContextCompaction` item type instead."* `[schema]`

Live test: `thread/compact/start` on a real thread returned `{}`, then compaction surfaced as
its **own synthetic turn** carrying a `contextCompaction` item. No `thread/compacted`
notification fired at all `[live]`:

```
turn/started    turn=…4389b9
item/started    contextCompaction  id=01a022d7-93db-…
item/completed  contextCompaction
turn/completed  turn=…4389b9
```

So on 0.148 the library types the deprecated path and not the live one. `schema.ThreadItem`
does define `ItemTypeContextCompaction`, so the item does reach consumers as a generic
`ItemStartedEvent` — but nothing marks it as compaction to the adapter, and
`ContextCompactedEvent` will never fire.

`[unverified]`: whether 0.147 still emits `thread/compacted`. Keep the existing handler
either way; it costs nothing and the notification is still in the union.

Also relevant: `remote_compaction_v2` is `stable`/default-on `[live]`, and auto-compaction
produces the same item without any client request.

---

## D. Mid-turn message injection

**The protocol has a first-class mid-turn channel. Agentique's buffer-and-replay emulation
is unnecessary.**

`turn/steer` `[schema]`:

```ts
TurnSteerParams = { threadId, input: UserInput[], expectedTurnId,
                    clientUserMessageId?, additionalContext?, responsesapiClientMetadata? }
TurnSteerResponse = { turnId }
```

`expectedTurnId` is a required precondition — the call fails if it names anything other than
the currently active turn.

Live, against a turn mid-stream `[live]`:

```
--- wrong expectedTurnId ---
-32600: expected active turn id `0000…` but found `01a022d7-6ec9-…`

--- correct expectedTurnId ---
{"turnId":"01a022d7-6ec9-7c72-afde-de875385abb0"}     <-- same turn, not a new one
```

The steered text appeared as a `userMessage` item **inside the running turn**, and the model
obeyed it:

```
item/completed userMessage   turn=…85abb0  "Count slowly from 1 to 40…"
item/completed agentMessage  turn=…85abb0  "1\n2\n3\n…"
item/completed userMessage   turn=…85abb0  "Forget counting. Just reply with the single word STEERED."
item/completed userMessage   turn=…85abb0  "queued follow-up"
item/completed agentMessage  turn=…85abb0  "STEERED"
turn/completed               turn=…85abb0
```

A second finding from the same run: **`turn/start` during an active turn does not create a
turn.** It returned the *active* turn's id and its input landed as another `userMessage` in
that same turn `[live]`. This matches t3code's note that "Codex accepts follow-ups while the
current turn is still running" `[t3code]`.

That has a direct consequence for this library: `Thread.StartTurn` called mid-turn will
silently merge into the running turn and return a `*Stream` for a turn id that is not new.
Worth a doc comment regardless of whether `Steer` gets added.

**One limitation.** Not every turn is steerable. `CodexErrorInfo` carries an
`activeTurnNotSteerable: { turnKind }` variant, where `NonSteerableTurnKind = "review" |
"compact"` `[schema]`. So a turn started by `review/start`, and the synthetic turn compaction
runs in (see C), both reject steering. Any `Steer` wrapper must surface that as a distinct
error rather than a generic failure — falling back to buffer-and-replay is the right
behaviour there, and only there.

For a durable pending queue rather than immediate injection, `thread/queue/{add,list,update,
reorder,delete,start}` exists behind `experimentalApi`, with a `thread/queue/changed`
notification `[schema]`. `ThreadQueueAddParams = { threadId, input, clientUserMessageId }`.

---

## E. Fork, and tool-progress ticks

### Fork — real, stable, unimplemented

`thread/fork` works without `experimentalApi` `[live]`:

```jsonc
// thread/fork { threadId: "01a022d7-6e6b-…" }
{ "id": "01a022d9-dd4a-…",           // new thread
  "sessionId": "01a022d9-dd4a-…",     // new session tree
  "forkedFromId": "01a022d7-6e6b-…",  // provenance
  "parentThreadId": null,             // a fork is NOT a subagent
  "turns": 2 }                        // history carried over
```

`beforeTurnId` truncates: forking before the last turn returned 1 turn instead of 2 `[live]`.
`lastTurnId` does the inclusive-through variant and cannot be combined with `beforeTurnId`
`[schema]`. Fork emits `thread/started` for the new thread `[live]`, and accepts the full
`thread/start` override set (model, cwd, sandbox, permissions, instructions).

Two adjacent history operations: `thread/rollback { threadId, numTurns }` is stable but
deprecated — calling it emits a `deprecationNotice` notification saying so `[live]` — and its
replacement `thread/revert { threadId, beforeTurnId }` is behind `experimentalApi` `[schema]`.

### Tool progress — partly a real limit

There is no generic per-tool progress tick. What exists `[schema]`:

| Notification | Scope |
| --- | --- |
| `item/mcpToolCall/progress` | MCP tool calls only: `{threadId, turnId, itemId, message}` — a human-readable string, no percentage |
| `item/commandExecution/outputDelta` | Streaming stdout/stderr for shell commands |
| `item/fileChange/outputDelta`, `item/fileChange/patchUpdated` | Streaming patch application |
| `command/exec/outputDelta`, `process/outputDelta` | Client-driven exec, outside the turn loop |

The library handles the two `outputDelta` methods and `patchUpdated`, and **not**
`item/mcpToolCall/progress`. So "ToolProgressTicks unsupported" is accurate for built-in
tools and wrong for MCP tools.

`[unverified]`: I did not drive an MCP tool call that emits progress, so the notification's
delivery cadence is unconfirmed. The type is confirmed.

### MaxTurns — a genuine limit

No `maxTurns`, turn cap, iteration budget, or equivalent exists anywhere in the generated
surface — case-insensitive search across all 144 methods and every param type finds nothing
`[schema]`. An orchestrator must enforce a turn cap itself by counting `turn/completed`.

**But there is a budget mechanism, and it is not turn-shaped.** `thread/goal/set` accepts a
`tokenBudget`, and the resulting `ThreadGoal` tracks spend against it `[schema]`:

```ts
ThreadGoalSetParams = { threadId, objective?, status?, tokenBudget? }
ThreadGoal = { threadId, objective, status, tokenBudget, tokensUsed,
               timeUsedSeconds, createdAt, updatedAt }
ThreadGoalStatus = "active" | "paused" | "blocked" | "usageLimited"
                 | "budgetLimited" | "complete"
```

The thread transitions to `budgetLimited` on exhaustion, and `CodexErrorInfo` has a matching
`sessionBudgetExceeded` variant. Progress arrives on `thread/goal/updated`. The `goals`
feature is stable and default-on `[live]`.

For an orchestrator, a token budget is a better cost ceiling than a turn count anyway. This
is worth treating as the answer to "MaxTurns" rather than reporting the capability as simply
absent. `[unverified]`: I did not exercise the goal API against a live thread, so the
enforcement behaviour on exhaustion is unconfirmed.

---

## Full inventory

### Client requests — 8 of 144

Everything not listed as supported is **missing** (no Go entry point, no types). Nothing is
partially supported at the request level.

**Supported**: `initialize`, `model/list`, `skills/list`, `skills/config/write`,
`thread/start`, `thread/resume`, `turn/start`, `turn/interrupt`.

**Missing, stable (90)**:

`account/login/cancel`, `account/login/start`, `account/logout`,
`account/rateLimitResetCredit/consume`, `account/rateLimits/read`, `account/read`,
`account/sendAddCreditsNudgeEmail`, `account/usage/read`, `account/workspaceMessages/read`,
`app/installed`, `app/list`, `app/read`, `command/exec`, `command/exec/resize`,
`command/exec/terminate`, `command/exec/write`, `config/batchWrite`,
`config/mcpServer/reload`, `config/read`, `config/value/write`, `configRequirements/read`,
`experimentalFeature/enablement/set`, `experimentalFeature/list`,
`externalAgentConfig/detect`, `externalAgentConfig/import`,
`externalAgentConfig/import/readHistories`, `externalAgentConfig/import/recordHistory`,
`feedback/upload`, `fs/copy`, `fs/createDirectory`, `fs/getMetadata`, `fs/readDirectory`,
`fs/readFile`, `fs/remove`, `fs/unwatch`, `fs/watch`, `fs/writeFile`, `hooks/list`,
`marketplace/add`, `marketplace/remove`, `marketplace/upgrade`, `mcpServer/oauth/login`,
`mcpServer/resource/read`, `mcpServer/tool/call`, `mcpServerStatus/list`,
`modelProvider/capabilities/read`, `permissionProfile/list`, `plugin/install`,
`plugin/installed`, `plugin/list`, `plugin/read`, `plugin/share/checkout`,
`plugin/share/delete`, `plugin/share/list`, `plugin/share/save`,
`plugin/share/updateTargets`, `plugin/skill/read`, `plugin/uninstall`, `review/start`,
`skills/extraRoots/set`, `thread/approveGuardianDeniedAction`, `thread/archive`,
`thread/compact/start`, `thread/delete`, `thread/fork`, `thread/goal/clear`,
`thread/goal/get`, `thread/goal/set`, `thread/inject_items`, `thread/list`,
`thread/loaded/list`, `thread/metadata/update`, `thread/name/set`, `thread/read`,
`thread/rollback` *(deprecated)*, `thread/section/move`, `thread/shellCommand`,
`thread/unarchive`, `thread/unsubscribe`, `threadSection/create`, `threadSection/delete`,
`threadSection/list`, `threadSection/update`, `turn/steer`, `windowsSandbox/readiness`,
`windowsSandbox/setupStart`.

Plus four legacy v1 methods with no namespace prefix, all stable: `fuzzyFileSearch`,
`getAuthStatus`, `getConversationSummary`, `gitDiffToRemote`.

**Missing, behind `experimentalApi` (46)**:

`collaborationMode/list`, `environment/add`, `environment/info`, `environment/status`,
`fuzzyFileSearch/sessionStart`, `fuzzyFileSearch/sessionStop`, `fuzzyFileSearch/sessionUpdate`,
`memory/reset`, `mock/experimentalMethod`, `plugin/search`, `process/kill`,
`process/resizePty`, `process/spawn`, `process/writeStdin`, `remoteControl/client/list`,
`remoteControl/client/revoke`, `remoteControl/disable`, `remoteControl/enable`,
`remoteControl/pairing/start`, `remoteControl/pairing/status`, `remoteControl/status/read`,
`server/diagnostics`, `thread/backgroundTerminals/clean`, `thread/backgroundTerminals/list`,
`thread/backgroundTerminals/terminate`, `thread/decrement_elicitation`,
`thread/increment_elicitation`, `thread/items/list` *(declared but returns "not supported
yet"* `[live]`*)*, `thread/memoryMode/set`, `thread/queue/add`, `thread/queue/delete`,
`thread/queue/list`, `thread/queue/reorder`, `thread/queue/start`, `thread/queue/update`,
`thread/realtime/appendAudio`, `thread/realtime/appendSpeech`, `thread/realtime/appendText`,
`thread/realtime/listVoices`, `thread/realtime/start`, `thread/realtime/stop`,
`thread/revert`, `thread/search`, `thread/searchOccurrences`, `thread/settings/update`,
`thread/turns/list`.

### Server notifications — 27 of 74

**Handled** (typed event): `thread/started` (logged only, not surfaced), `thread/status/changed`,
`thread/tokenUsage/updated`, `thread/compacted` *(deprecated by the server, see C)*,
`turn/started`, `turn/completed`, `turn/diff/updated`, `turn/plan/updated`, `item/started`,
`item/completed`, `item/agentMessage/delta`, `item/plan/delta`,
`item/commandExecution/outputDelta`, `item/fileChange/outputDelta`,
`item/fileChange/patchUpdated`, `item/reasoning/textDelta`,
`item/reasoning/summaryTextDelta`, `item/reasoning/summaryPartAdded`,
`account/rateLimits/updated`, `skills/changed`, `mcpServer/startupStatus/updated`,
`model/rerouted`, `warning`, `guardianWarning`, `configWarning`, `deprecationNotice`, `error`.

**Missing** (falls through to `UnknownEvent`, so forward-compatible but untyped) — ranked by
relevance to a session orchestrator:

| Notification | Why it matters |
| --- | --- |
| `item/mcpToolCall/progress` | MCP tool progress ticks (E) |
| `thread/queue/changed` | Pending-message queue depth (D) |
| `thread/name/updated` | Codex renames threads itself; UI title drift |
| `thread/closed` | Thread teardown — relevant for child cleanup |
| `thread/reverted` | History mutation the UI must mirror |
| `thread/goal/updated`, `thread/goal/cleared` | `goals` is stable/default-on |
| `turn/moderationMetadata` | Refusal / moderation signal |
| `model/verification`, `model/safetyBuffering/updated` | Model-side stalls that look like hangs |
| `item/autoApprovalReview/started`, `item/autoApprovalReview/completed` | Guardian auto-approval (`guardian_approval` is default-on) |
| `item/commandExecution/terminalInteraction` | PTY interaction |
| `serverRequest/resolved` | A server request was answered elsewhere — cancels a pending prompt |
| `hook/started`, `hook/completed` | `hooks` is default-on |
| `thread/archived`, `thread/unarchived`, `thread/deleted` | Session list sync |
| `thread/settings/updated` | Mid-session model/effort changes |
| `mcpServer/oauthLogin/completed` | MCP auth flow |
| `account/updated`, `account/login/completed` | Auth state |
| `fs/changed` | Only if `fs/watch` is used |
| `app/list/updated`, `skills/changed` | Catalog changes |
| `command/exec/outputDelta`, `process/outputDelta`, `process/exited` | Client-driven exec |
| `rawResponse/completed`, `rawResponseItem/completed` | Raw Responses API passthrough |
| `externalAgentConfig/import/*`, `remoteControl/status/changed`, `windows*`, `thread/environment/*`, `thread/realtime/*`, `fuzzyFileSearch/*` | Out of scope for Agentique |

### Server requests — 7 of 11

**Handled**: `item/commandExecution/requestApproval`, `item/fileChange/requestApproval`,
`item/permissions/requestApproval`, `item/tool/requestUserInput`,
`mcpServer/elicitation/request`, plus the two legacy approval methods `execCommandApproval`
and `applyPatchApproval` — both of which are **still present** in 0.148's `ServerRequest`
union `[schema]`, so `approval.go` is not carrying dead branches.

**Missing**: `item/tool/call` (dynamic tools — the client executes a tool the model called),
`attestation/generate` (opt-in via `requestAttestation`), `account/chatgptAuthTokens/refresh`,
`currentTime/read` (experimental).

---

## Ranked backlog

Ordered by value to a session orchestrator. Signatures follow this repo's conventions:
`Conn` methods take `ctx` first, thread-scoped operations hang off `*Thread`, wire unions stay
`json.RawMessage` behind typed accessors.

### 1. Child-thread visibility (subagent roster)

Unblocks a Codex agent roster in Agentique. Everything else on this list is smaller.

Three separable pieces:

**1a. Stop dropping child traffic.** `Conn.deliver` needs a fallback for unknown thread ids
instead of a silent `return`. Options: a connection-level subscriber, or auto-registering a
child on first sight.

```go
// WithChildThreadEvents delivers notifications for threads this client did
// not create — Codex subagents run as sibling threads on the same connection.
// Without it they are dropped.
func WithChildThreadEvents(enabled bool) Option

// ChildThreadEvent wraps a notification addressed to a thread other than the
// subscriber's own. Kind is the wire method.
type ChildThreadEvent struct {
    ParentThreadID string // "" until the spawn is correlated
    ThreadID       string
    Inner          Event
}
```

**1b. Project the two roster items.** Both are `Raw`-only today.

```go
// SubAgentActivity returns the subAgentActivity projection, or nil if the
// item is another type. Kind is one of SubAgentActivity{Started,Interacted,
// Interrupted}; AgentPath is hierarchical ("/root/alpha").
func (i *ThreadItem) SubAgentActivity() *SubAgentActivity

type SubAgentActivity struct {
    Kind          string `json:"kind"`
    AgentThreadID string `json:"agentThreadId"`
    AgentPath     string `json:"agentPath"`
}

// CollabAgentToolCall returns the collabAgentToolCall projection, or nil.
// Note: codex 0.148 emits only tool="wait", with ReceiverThreadIDs and
// AgentsStates empty. Do not build a roster on this alone.
func (i *ThreadItem) CollabAgentToolCall() *CollabAgentToolCall

type CollabAgentToolCall struct {
    Tool              string                     `json:"tool"`
    Status            string                     `json:"status"`
    SenderThreadID    string                     `json:"senderThreadId"`
    ReceiverThreadIDs []string                   `json:"receiverThreadIds"`
    Prompt            *string                    `json:"prompt,omitempty"`
    Model             *string                    `json:"model,omitempty"`
    ReasoningEffort   *string                    `json:"reasoningEffort,omitempty"`
    AgentsStates      map[string]CollabAgentState `json:"agentsStates"`
}
```

**1c. Fleet-aware interrupt.** Must exist for Stop to work at all during a fleet.

```go
// InterruptTree interrupts every live child turn spawned under t, then t's
// own turn. Child interrupts are bounded by perChild and by the ctx
// deadline; a wedged child never blocks the parent interrupt.
func (t *Thread) InterruptTree(ctx context.Context, perChild time.Duration) error

// Children returns the child threads observed on this connection under t,
// newest first.
func (t *Thread) Children() []ChildThread

type ChildThread struct {
    ThreadID    string
    AgentPath   string // "/root/alpha"
    SpawnTurnID string // parent turn that spawned the fleet
    ActiveTurn  string // "" when idle
    Status      string // ThreadStatus discriminator
}
```

Reconnect path:

```go
// ListChildThreads enumerates stored descendants of a thread. Set recursive
// for ancestorThreadId semantics (any depth) rather than direct children.
func (c *Conn) ListChildThreads(ctx context.Context, threadID string, recursive bool) ([]schema.Thread, error)
```

Implementation note: buffer notifications from unknown thread ids for a beat. The child's
first `thread/status/changed` reliably precedes the `subAgentActivity` that names it.

### 2. `turn/steer` — mid-turn injection

Removes Agentique's buffer-and-replay emulation entirely.

```go
// Steer injects input into the turn already running on t, without waiting
// for an idle boundary. It fails if turnID is not the active turn — pass
// t.ActiveTurnID() unless you are guarding against a race. The returned id
// is the same running turn, not a new one.
func (t *Thread) Steer(ctx context.Context, turnID string, input []schema.UserInput) (string, error)
```

Pair it with a doc comment on `StartTurn` warning that a mid-turn call merges into the
running turn `[live]` rather than starting a new one.

### 3. Compaction as an item

Small, and the current typing is actively misleading.

```go
// ContextCompaction reports that codex compacted the thread's context.
// codex 0.148 delivers this as a contextCompaction item inside a synthetic
// turn; the older thread/compacted notification is deprecated.
type ContextCompactionEvent struct {
    ThreadID string
    TurnID   string
    ItemID   string
}

// Compact asks codex to compact the thread's context now. Compaction runs
// as its own turn and surfaces a contextCompaction item.
func (t *Thread) Compact(ctx context.Context) error
```

### 4. `thread/fork`

```go
// ForkThread branches threadID into a new thread carrying its history.
// The result's ForkedFromID records provenance; ParentThreadID stays nil —
// a fork is not a subagent.
func (c *Conn) ForkThread(ctx context.Context, threadID string, opts ...ForkOption) (*Thread, error)

// ForkBeforeTurn truncates the fork to the turns preceding turnID.
func ForkBeforeTurn(turnID string) ForkOption
// ForkThroughTurn keeps turns up to and including turnID. Mutually
// exclusive with ForkBeforeTurn.
func ForkThroughTurn(turnID string) ForkOption
```

### 5. History and session listing

Needed for any UI that shows sessions it did not create.

```go
func (c *Conn) ReadThread(ctx context.Context, threadID string, includeTurns bool) (*schema.Thread, error)
func (c *Conn) ListThreads(ctx context.Context, params schema.ThreadListParams) (*schema.ThreadListResponse, error)
func (c *Conn) SetThreadName(ctx context.Context, threadID, name string) error
func (c *Conn) ArchiveThread(ctx context.Context, threadID string) error
func (c *Conn) DeleteThread(ctx context.Context, threadID string) error
```

Plus the `thread/name/updated` notification — codex renames threads on its own, so a cached
title goes stale.

### 6. MCP tool progress

```go
// McpToolProgressEvent corresponds to `item/mcpToolCall/progress`. Message
// is a human-readable status string; there is no numeric progress.
type McpToolProgressEvent struct {
    ThreadID, TurnID, ItemID, Message string
}
```

### 7. Thread goals as a cost ceiling

The nearest thing the protocol has to `MaxTurns` (E), and `goals` is default-on.

```go
// SetGoal attaches an objective and optional token budget to t. The thread
// moves to ThreadGoalStatusBudgetLimited once TokensUsed passes the budget.
// Pass 0 for tokenBudget to leave it unbounded.
func (t *Thread) SetGoal(ctx context.Context, objective string, tokenBudget int64) (*schema.ThreadGoal, error)
func (t *Thread) Goal(ctx context.Context) (*schema.ThreadGoal, error)
func (t *Thread) ClearGoal(ctx context.Context) error

// GoalUpdatedEvent corresponds to `thread/goal/updated`.
type GoalUpdatedEvent struct {
    ThreadID string
    TurnID   string // "" when not tied to a turn
    Goal     schema.ThreadGoal
}
```

### 8. Guardian and hook lifecycle

`guardian_approval` and `hooks` are both stable and default-on `[live]`, and their
notifications currently land as `UnknownEvent`: `item/autoApprovalReview/started`,
`item/autoApprovalReview/completed`, `hook/started`, `hook/completed`. A turn that stalls in
auto-approval review looks like a hang without them.

### 9. `serverRequest/resolved`

Tells the client a pending server request was answered by someone else. Without it a UI can
leave an approval prompt on screen forever. Cheap.

### 10. Everything else

`review/start`, `thread/shellCommand`, `thread/inject_items`, the `fs/*` and `command/exec`
families, plugins, marketplace, accounts. None of it is on Agentique's path today.

---

## Corrections to existing repo docs

- `README.md` says "fork … not yet wired" — accurate, but it belongs in the backlog above
  rather than reading as a protocol limitation.
- `schema/notifications.go:21-23` describes `thread/compacted` as the live compaction signal.
  On 0.148 it is deprecated in favour of the `contextCompaction` item (C).
- `schema/types.go:592` lists `collabAgentToolCall` and `subAgentActivity` among shapes
  "reachable via Raw". They are the subagent surface, so they deserve accessors (backlog 1b).
- Fixtures and doc comments target 0.147; this report is measured on 0.148. The deltas found
  are all additive, but the version skew is worth closing.

## Loose ends

- Interrupting a live fleet was not exercised — only the parameter shape and the necessity of
  per-child calls are confirmed `[unverified]`.
- `item/mcpToolCall/progress` was never observed on the wire `[unverified]`.
- Whether 0.147 still emits `thread/compacted` was not tested `[unverified]`.
- `collabAgentToolCall.agentsStates` was empty in both captures. Whether it is ever populated
  — and under what conditions `spawnAgent` / `sendInput` / `closeAgent` items appear — is
  unknown `[unverified]`. t3code's 0.145 capture shows the same empty fields, so this may be
  aspirational schema rather than live behaviour.
