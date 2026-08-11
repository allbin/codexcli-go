package codexcli

import (
	"encoding/json"
	"fmt"

	"github.com/allbin/codexcli-go/schema"
)

// Event is the sealed interface representing a stream event surfaced by
// Client.Run. Consumers use a type switch to dispatch.
//
// Mapping from the wire (codex app-server notifications) is in
// dispatchNotification. Untyped server notifications surface as
// *UnknownEvent so consumers can spot protocol drift between versions.
type Event interface {
	event()
}

// StartEvent is emitted by the client immediately before the first turn
// is dispatched. ThreadID is the freshly minted thread.
type StartEvent struct {
	ThreadID string
	Model    string
	Cwd      string
}

func (*StartEvent) event() {}
func (e *StartEvent) String() string {
	return fmt.Sprintf("StartEvent{Thread: %s, Model: %s}", e.ThreadID, e.Model)
}

// TurnStartedEvent corresponds to the `turn/started` notification: the
// server has accepted the turn and is now generating.
type TurnStartedEvent struct {
	ThreadID string
	Turn     schema.Turn
}

func (*TurnStartedEvent) event() {}
func (e *TurnStartedEvent) String() string {
	return fmt.Sprintf("TurnStartedEvent{Turn: %s}", e.Turn.ID)
}

// ItemStartedEvent corresponds to `item/started`. Each ThreadItem
// passes through item/started -> item/* deltas -> item/completed.
type ItemStartedEvent struct {
	ThreadID    string
	TurnID      string
	Item        schema.ThreadItem
	StartedAtMs int64
}

func (*ItemStartedEvent) event() {}
func (e *ItemStartedEvent) String() string {
	return fmt.Sprintf("ItemStartedEvent{Type: %s, ID: %s}", e.Item.Type, e.Item.ID)
}

// ItemCompletedEvent corresponds to `item/completed`. The Item carries
// the final state — for tool-like items this includes exit codes and
// status; for agentMessage items this is the accumulated text.
type ItemCompletedEvent struct {
	ThreadID string
	TurnID   string
	Item     schema.ThreadItem
	// CompletedAtMs is the Unix-milliseconds timestamp the server stamped
	// on completion. Subtract ItemStartedEvent.StartedAtMs for wall-clock
	// duration on items that carry no DurationMs.
	CompletedAtMs int64
}

func (*ItemCompletedEvent) event() {}
func (e *ItemCompletedEvent) String() string {
	return fmt.Sprintf("ItemCompletedEvent{Type: %s, ID: %s}", e.Item.Type, e.Item.ID)
}

// AgentMessageDeltaEvent corresponds to `item/agentMessage/delta` —
// streamed assistant text for an agentMessage item. Consumers
// concatenate deltas keyed by ItemID until item/completed delivers the
// authoritative text on Item.Text.
type AgentMessageDeltaEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	Delta    string
}

func (*AgentMessageDeltaEvent) event() {}
func (e *AgentMessageDeltaEvent) String() string {
	return fmt.Sprintf("AgentMessageDeltaEvent{Item: %s, len: %d}", e.ItemID, len(e.Delta))
}

// TurnCompletedEvent is the terminal event for a turn. Status is one of
// completed, interrupted, or failed; on failure Turn.Error is populated.
type TurnCompletedEvent struct {
	ThreadID string
	Turn     schema.Turn
}

func (*TurnCompletedEvent) event() {}
func (e *TurnCompletedEvent) String() string {
	return fmt.Sprintf("TurnCompletedEvent{Turn: %s, Status: %s}", e.Turn.ID, e.Turn.Status)
}

// ErrorEvent is emitted on transport failure, an `error` server
// notification, or process exit. Fatal=true means the underlying stream
// is no longer usable.
type ErrorEvent struct {
	Err   error
	Fatal bool
}

func (*ErrorEvent) event() {}
func (e *ErrorEvent) String() string {
	return fmt.Sprintf("ErrorEvent{Fatal: %v, Err: %v}", e.Fatal, e.Err)
}
func (e *ErrorEvent) Error() string { return e.Err.Error() }
func (e *ErrorEvent) Unwrap() error { return e.Err }

// StderrEvent carries a single line of subprocess stderr output.
type StderrEvent struct {
	Line string
}

func (*StderrEvent) event()           {}
func (e *StderrEvent) String() string { return fmt.Sprintf("StderrEvent{%s}", e.Line) }

// ContentDeltaEvent is a unified event for all streaming content deltas:
// command output, file change output, reasoning text, reasoning summaries,
// and plan deltas. Kind discriminates the source; consumers that only care
// about a subset can filter on Kind in a type switch.
type ContentDeltaEvent struct {
	Kind     ContentDeltaKind
	ThreadID string
	TurnID   string
	ItemID   string
	Delta    string

	// ContentIndex is set for reasoning text deltas.
	ContentIndex int64
	// SummaryIndex is set for reasoning summary text deltas.
	SummaryIndex int64
}

func (*ContentDeltaEvent) event() {}
func (e *ContentDeltaEvent) String() string {
	return fmt.Sprintf("ContentDeltaEvent{Kind: %s, Item: %s, len: %d}", e.Kind, e.ItemID, len(e.Delta))
}

// ContentDeltaKind classifies the source of a ContentDeltaEvent.
type ContentDeltaKind string

const (
	ContentDeltaCommandOutput    ContentDeltaKind = "command_output"
	ContentDeltaFileChangeOutput ContentDeltaKind = "file_change_output"
	ContentDeltaReasoningText    ContentDeltaKind = "reasoning_text"
	ContentDeltaReasoningSummary ContentDeltaKind = "reasoning_summary"
	ContentDeltaPlan             ContentDeltaKind = "plan"
)

// TurnDiffUpdatedEvent corresponds to `turn/diff/updated` — the
// aggregated unified diff across all file changes in the current turn.
type TurnDiffUpdatedEvent struct {
	ThreadID string
	TurnID   string
	Diff     string
}

func (*TurnDiffUpdatedEvent) event() {}
func (e *TurnDiffUpdatedEvent) String() string {
	return fmt.Sprintf("TurnDiffUpdatedEvent{Turn: %s, len: %d}", e.TurnID, len(e.Diff))
}

// RateLimitsUpdatedEvent corresponds to `account/rateLimits/updated`.
type RateLimitsUpdatedEvent struct {
	RateLimits schema.RateLimitSnapshot
}

func (*RateLimitsUpdatedEvent) event() {}
func (e *RateLimitsUpdatedEvent) String() string {
	return "RateLimitsUpdatedEvent{}"
}

// TokenUsageUpdatedEvent corresponds to `thread/tokenUsage/updated`.
type TokenUsageUpdatedEvent struct {
	ThreadID   string
	TurnID     string
	TokenUsage schema.ThreadTokenUsage
}

func (*TokenUsageUpdatedEvent) event() {}
func (e *TokenUsageUpdatedEvent) String() string {
	return fmt.Sprintf("TokenUsageUpdatedEvent{Thread: %s, Total: %d}",
		e.ThreadID, e.TokenUsage.Total.TotalTokens)
}

// SkillsChangedEvent corresponds to the `skills/changed` notification:
// codex detected a change in watched local skill files. It carries no
// payload — treat it as an invalidation signal and re-run Conn.ListSkills
// with your current parameters if you cache skill metadata.
//
// It is broadcast to every stream subscriber (it is not scoped to a single
// thread), mirroring RateLimitsUpdatedEvent.
type SkillsChangedEvent struct{}

func (*SkillsChangedEvent) event() {}
func (e *SkillsChangedEvent) String() string {
	return "SkillsChangedEvent{}"
}

// UnknownEvent is emitted for any server notification this SDK doesn't
// recognize. Preserves the method name and raw payload so consumers can
// keep working through protocol additions ahead of typed support.
type UnknownEvent struct {
	Method string
	Params json.RawMessage
}

func (*UnknownEvent) event() {}
func (e *UnknownEvent) String() string {
	return fmt.Sprintf("UnknownEvent{Method: %s, len: %d}", e.Method, len(e.Params))
}

// ApprovalRequestEvent surfaces an inbound approval request to stream
// consumers in parallel with the configured ApprovalFunc. Useful for UIs
// that want to render a pending-approval indicator while the callback
// computes a decision.
type ApprovalRequestEvent struct {
	Request ApprovalRequest
}

func (*ApprovalRequestEvent) event() {}
func (e *ApprovalRequestEvent) String() string {
	return fmt.Sprintf("ApprovalRequestEvent{Method: %s, Thread: %s, Turn: %s}",
		e.Request.Method(), e.Request.ThreadID(), e.Request.TurnID())
}

// UnknownServerRequestEvent surfaces a server-initiated JSON-RPC request
// that this SDK does not handle natively. The dispatcher always responds
// with a method-not-found error to keep the protocol moving; this event
// exists so consumers can log/observe drift.
type UnknownServerRequestEvent struct {
	Method string
	Params json.RawMessage
}

func (*UnknownServerRequestEvent) event() {}
func (e *UnknownServerRequestEvent) String() string {
	return fmt.Sprintf("UnknownServerRequestEvent{Method: %s, len: %d}", e.Method, len(e.Params))
}
