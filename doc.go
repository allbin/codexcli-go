// Package codexcli wraps the `codex app-server` JSON-RPC protocol as a Go
// client. The public API mirrors github.com/allbin/claudecli-go: consumers
// can swap implementations by changing the import path.
//
// # Transport
//
// Stdio (newline-delimited JSON). Each line on stdin/stdout is a single
// JSON-RPC 2.0 message (request, response, or notification). The
// `"jsonrpc":"2.0"` header is intentionally omitted on the wire by the
// codex app-server, matching MCP-style framing.
//
// # Lifecycle
//
// Every connection begins with a single `initialize` request from the client
// and an `initialized` notification before any other call is accepted.
// Client.New starts the subprocess and performs the handshake before returning.
//
// # API Surface
//
// High-level: Client.Run(ctx, prompt) starts a thread, dispatches a turn,
// and streams typed events until turn/completed.
//
// Mid-level: Client.Connect(ctx) returns a *Conn for fine-grained control.
// Use Conn.NewThread to create threads, Conn.ResumeThread to resume a
// previously persisted thread, and Thread.StartTurn for multi-turn
// conversations.
//
// Environment: DetectInstall reports which codex binary would be spawned,
// how it was installed, and the command that updates that install — offline,
// read-only, and reporting InstallUnknown with no command rather than
// guessing. Doctor projects `codex doctor --json` into a typed struct and is
// deliberately kept separate because it touches the network.
//
// Install lifecycle: LatestPublished answers "am I behind?" in one HTTP
// request, with a three-state verdict that says "no verdict" rather than
// comparing an install against a release stream it does not follow. Update
// runs codex's own updater for a standalone install, refuses every other
// method with the command to show the user, and verifies the outcome by
// re-reading the version rather than trusting the exit code. The three are
// priced differently on purpose — offline, one request, and minutes — so a
// consumer can schedule around the cost.
//
// Account: Conn.AccountRateLimits and Conn.Account read rate-limit and
// sign-in state on demand, without a thread. They are the pull counterpart
// to RateLimitsUpdatedEvent, which the server only pushes mid-turn, so a
// usage indicator can render on an idle connection. Both report
// ErrMethodNotSupported when the app-server lacks the method, and Account
// reports ErrNotSignedIn when nobody is logged in — the two "nothing to
// show" outcomes, distinct from a failure.
//
// Models: ListModels reads the codex CLI's on-disk model cache
// ($CODEX_HOME/models_cache.json) and returns []ModelInfo. Conn.ListModels
// queries the running server via the model/list RPC for live availability
// and returns []schema.Model — a different projection of the registry, not
// interchangeable with ModelInfo.
//
// # Events
//
// The event stream surfaces typed events for the full notification set:
// turn lifecycle (TurnStartedEvent, TurnCompletedEvent), item lifecycle
// (ItemStartedEvent, ItemCompletedEvent), content deltas
// (AgentMessageDeltaEvent, ContentDeltaEvent for command output, file
// changes, reasoning, plan), thread status (ThreadStatusChangedEvent),
// plans (TurnPlanUpdatedEvent), token usage (TokenUsageUpdatedEvent), rate
// limits (RateLimitsUpdatedEvent), and aggregated diffs
// (TurnDiffUpdatedEvent). Anything unrecognized arrives as UnknownEvent.
//
// Note token usage is only available via TokenUsageUpdatedEvent — codex
// removed the usage field from the Turn object after 0.133.
//
// # Compatibility
//
// Verified end-to-end against codex CLI 0.147.0. See the README's
// "Upgrading to codex 0.147" section for the three changes that need
// consumer action.
//
// # Resume
//
// Conn.ResumeThread(ctx, threadID) sends thread/resume to rehydrate a
// previously persisted thread. Returns ErrThreadNotFound on recoverable
// errors so callers can fall back to NewThread.
package codexcli
