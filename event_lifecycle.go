package codexcli

import (
	"fmt"

	"github.com/allbin/codexcli-go/schema"
)

// Events in this file cover the thread-, plan-, and connection-level
// notifications codex added through the 0.14x releases. They were all
// surfacing as *UnknownEvent before.

// ThreadStatusChangedEvent corresponds to `thread/status/changed`. Codex
// brackets every turn with an active/idle pair, so this is the cheapest
// signal for driving a busy indicator without tracking turn lifecycles.
//
// Status is one of schema.ThreadStatusIdle, ThreadStatusActive,
// ThreadStatusNotLoaded, or ThreadStatusSystemError. StatusRaw keeps the
// full union for the active variant's flags.
type ThreadStatusChangedEvent struct {
	ThreadID  string
	Status    string
	StatusRaw []byte
}

func (*ThreadStatusChangedEvent) event() {}
func (e *ThreadStatusChangedEvent) String() string {
	return fmt.Sprintf("ThreadStatusChangedEvent{Thread: %s, Status: %s}", e.ThreadID, e.Status)
}

// TurnPlanUpdatedEvent corresponds to `turn/plan/updated` — the agent's
// todo plan, resent in full on every change. Render Plan as-is; do not
// try to diff against the previous event.
//
// Distinct from ContentDeltaEvent with Kind ContentDeltaPlan, which
// streams the text of a "plan" thread item.
type TurnPlanUpdatedEvent struct {
	ThreadID    string
	TurnID      string
	Plan        []schema.TurnPlanStep
	Explanation string
}

func (*TurnPlanUpdatedEvent) event() {}
func (e *TurnPlanUpdatedEvent) String() string {
	return fmt.Sprintf("TurnPlanUpdatedEvent{Turn: %s, steps: %d}", e.TurnID, len(e.Plan))
}

// FileChangePatchUpdatedEvent corresponds to
// `item/fileChange/patchUpdated` — the change set accumulated so far for
// a fileChange item still in progress. The item's own item/completed
// notification carries the authoritative final set.
type FileChangePatchUpdatedEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	Changes  []schema.FileUpdateChange
}

func (*FileChangePatchUpdatedEvent) event() {}
func (e *FileChangePatchUpdatedEvent) String() string {
	return fmt.Sprintf("FileChangePatchUpdatedEvent{Item: %s, changes: %d}", e.ItemID, len(e.Changes))
}

// ReasoningSummaryPartAddedEvent corresponds to
// `item/reasoning/summaryPartAdded` — a new summary block opened at
// SummaryIndex. The ContentDeltaEvent stream with Kind
// ContentDeltaReasoningSummary then fills it in at that index.
type ReasoningSummaryPartAddedEvent struct {
	ThreadID     string
	TurnID       string
	ItemID       string
	SummaryIndex int64
}

func (*ReasoningSummaryPartAddedEvent) event() {}
func (e *ReasoningSummaryPartAddedEvent) String() string {
	return fmt.Sprintf("ReasoningSummaryPartAddedEvent{Item: %s, Index: %d}", e.ItemID, e.SummaryIndex)
}

// ContextCompactedEvent corresponds to `thread/compacted` — codex
// summarised earlier history to stay inside the model's context window.
// Items emitted before this point remain valid, but the model no longer
// sees them verbatim.
type ContextCompactedEvent struct {
	ThreadID string
	TurnID   string
}

func (*ContextCompactedEvent) event() {}
func (e *ContextCompactedEvent) String() string {
	return fmt.Sprintf("ContextCompactedEvent{Thread: %s, Turn: %s}", e.ThreadID, e.TurnID)
}

// McpServerStatusEvent corresponds to `mcpServer/startupStatus/updated`.
// Codex emits several of these per configured MCP server while a
// connection warms up; Status is one of the schema.McpServer* constants.
//
// It is broadcast to every subscriber, since server startup is not scoped
// to one thread.
type McpServerStatusEvent struct {
	ThreadID string
	Name     string
	Status   string
	Err      string
}

func (*McpServerStatusEvent) event() {}
func (e *McpServerStatusEvent) String() string {
	return fmt.Sprintf("McpServerStatusEvent{Server: %s, Status: %s}", e.Name, e.Status)
}

// ModelReroutedEvent corresponds to `model/rerouted` — codex switched
// models mid-turn (capacity, safety, or policy). Anything a consumer
// derived from the requested model (pricing, context window) is stale
// from here on.
type ModelReroutedEvent struct {
	ThreadID  string
	TurnID    string
	FromModel string
	ToModel   string
	// ReasonRaw is the ModelRerouteReason union, left raw because the
	// variant set is still churning upstream.
	ReasonRaw []byte
}

func (*ModelReroutedEvent) event() {}
func (e *ModelReroutedEvent) String() string {
	return fmt.Sprintf("ModelReroutedEvent{%s -> %s}", e.FromModel, e.ToModel)
}

// WarningEvent corresponds to the `warning` and `guardianWarning`
// notifications — user-facing advisories that are not turn failures.
// Guardian reports Guardian=true and always carries a ThreadID.
type WarningEvent struct {
	ThreadID string
	Message  string
	Guardian bool
}

func (*WarningEvent) event() {}
func (e *WarningEvent) String() string {
	return fmt.Sprintf("WarningEvent{Guardian: %v, Message: %s}", e.Guardian, e.Message)
}

// ConfigWarningEvent corresponds to `configWarning` — codex found a
// problem in the user's config.toml. It arrives during connection setup,
// before any thread exists, so it is broadcast rather than thread-scoped.
//
// Worth surfacing: a malformed config silently changes model, sandbox, or
// approval behaviour, and this is the only notice the user gets.
type ConfigWarningEvent struct {
	Summary string
	Details string
	// Path is the config file that triggered the warning, when known.
	Path string
}

func (*ConfigWarningEvent) event() {}
func (e *ConfigWarningEvent) String() string {
	return fmt.Sprintf("ConfigWarningEvent{%s}", e.Summary)
}

// DeprecationNoticeEvent corresponds to `deprecationNotice` — codex
// announcing that a protocol surface is going away. Log these: they are
// the earliest signal that this SDK needs updating for a newer CLI.
//
// Broadcast to every subscriber; it is connection-scoped.
type DeprecationNoticeEvent struct {
	Summary string
	Details string
}

func (*DeprecationNoticeEvent) event() {}
func (e *DeprecationNoticeEvent) String() string {
	return fmt.Sprintf("DeprecationNoticeEvent{%s}", e.Summary)
}
