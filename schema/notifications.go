package schema

import "encoding/json"

// Server-to-client notification method names typed by this package since
// codex 0.14x. The lifecycle methods that predate them (thread/started,
// turn/*, item/*) are matched by literal in the dispatcher.
const (
	// MethodThreadStatusChanged reports a thread moving between idle and
	// active. It brackets every turn, so it is the cheapest signal for a
	// UI busy indicator.
	MethodThreadStatusChanged = "thread/status/changed"
	// MethodTurnPlanUpdated carries the agent's current todo plan.
	MethodTurnPlanUpdated = "turn/plan/updated"
	// MethodFileChangePatchUpdated carries the running patch for a
	// fileChange item before it completes.
	MethodFileChangePatchUpdated = "item/fileChange/patchUpdated"
	// MethodReasoningSummaryPartAdded marks the start of a new reasoning
	// summary block; subsequent summaryTextDelta events fill it in.
	MethodReasoningSummaryPartAdded = "item/reasoning/summaryPartAdded"
	// MethodThreadCompacted reports that codex compacted the thread's
	// context to stay within the model's window.
	MethodThreadCompacted = "thread/compacted"
	// MethodMcpServerStatusUpdated reports MCP server startup progress.
	MethodMcpServerStatusUpdated = "mcpServer/startupStatus/updated"
	// MethodModelRerouted reports codex switching models mid-turn.
	MethodModelRerouted = "model/rerouted"
	// MethodWarning is a user-facing advisory not tied to a failure.
	MethodWarning = "warning"
	// MethodGuardianWarning is a warning raised by the approvals guardian.
	MethodGuardianWarning = "guardianWarning"
	// MethodDeprecationNotice announces a protocol surface going away.
	MethodDeprecationNotice = "deprecationNotice"
	// MethodConfigWarning reports a problem in the user's config.toml. It
	// fires during connection setup, before any thread exists.
	MethodConfigWarning = "configWarning"
	// MethodThreadSettingsUpdated carries the thread's settings after a
	// change to model, effort, or another sticky turn setting. Codex
	// 0.153.4 only sends it on connections that negotiated experimentalApi.
	MethodThreadSettingsUpdated = "thread/settings/updated"
)

// ThreadSettings is the thread's full settings snapshot as reported by
// `thread/settings/updated`: what the next turn runs with unless its
// turn/start overrides something.
type ThreadSettings struct {
	Cwd               string            `json:"cwd"`
	ApprovalPolicy    AskForApproval    `json:"approvalPolicy"`
	ApprovalsReviewer ApprovalsReviewer `json:"approvalsReviewer"`
	// SandboxPolicy is the camelCase tagged SandboxPolicy union
	// ({"type":"dangerFullAccess"}, ...), left raw.
	SandboxPolicy           json.RawMessage `json:"sandboxPolicy"`
	ActivePermissionProfile json.RawMessage `json:"activePermissionProfile,omitempty"`
	Model                   string          `json:"model"`
	ModelProvider           string          `json:"modelProvider"`
	ServiceTier             *string         `json:"serviceTier"`
	// Effort is the reasoning effort as codex stores it, which is not
	// always the value sent to the model: "ultra" goes out as the model's
	// highest supported level.
	Effort *string `json:"effort"`
	// Summary is the reasoning summary mode ("auto", "concise",
	// "detailed", "none"), or nil when unset.
	Summary *string `json:"summary"`
	// CollaborationMode is {"mode": ..., "settings": {...}}; its settings
	// object is snake_case on the wire.
	CollaborationMode json.RawMessage `json:"collaborationMode,omitempty"`
	// MultiAgentMode is deprecated upstream and always
	// "explicitRequestOnly".
	MultiAgentMode json.RawMessage `json:"multiAgentMode,omitempty"`
	Personality    *Personality    `json:"personality"`
}

// ThreadSettingsUpdatedNotification is the params payload of
// `thread/settings/updated`.
type ThreadSettingsUpdatedNotification struct {
	ThreadId       string          `json:"threadId"`
	ThreadSettings json.RawMessage `json:"threadSettings"`
}

// Settings decodes the snapshot. ThreadSettings stays raw on the
// notification so fields added upstream survive for callers that
// re-decode it.
func (n ThreadSettingsUpdatedNotification) Settings() (ThreadSettings, error) {
	var s ThreadSettings
	err := json.Unmarshal(n.ThreadSettings, &s)
	return s, err
}

// ConfigWarningNotification is the params payload of `configWarning` —
// codex found something wrong in config.toml. Path and Range locate the
// offending entry when codex can pin it down.
type ConfigWarningNotification struct {
	Summary string  `json:"summary"`
	Details *string `json:"details,omitempty"`
	Path    *string `json:"path,omitempty"`
	// Range is a TextRange within Path; left raw since consumers that
	// care about it are editor integrations with their own position type.
	Range json.RawMessage `json:"range,omitempty"`
}

// ThreadStatusChangedNotification is the params payload of
// `thread/status/changed`.
type ThreadStatusChangedNotification struct {
	ThreadId string `json:"threadId"`
	// Status is the ThreadStatus union; read the discriminator via
	// StatusType.
	Status json.RawMessage `json:"status"`
}

// StatusType returns the status discriminator — one of the ThreadStatus*
// constants — or "" when absent or unparseable.
func (n ThreadStatusChangedNotification) StatusType() string { return statusType(n.Status) }

// TurnPlanStep is one entry of the agent's todo plan.
type TurnPlanStep struct {
	Step string `json:"step"`
	// Status is "pending", "in_progress", or "completed".
	Status string `json:"status"`
}

// TurnPlanUpdatedNotification is the params payload of
// `turn/plan/updated` — the agent's full plan, resent on every change.
type TurnPlanUpdatedNotification struct {
	ThreadId    string         `json:"threadId"`
	TurnId      string         `json:"turnId"`
	Plan        []TurnPlanStep `json:"plan"`
	Explanation *string        `json:"explanation,omitempty"`
}

// FileChangePatchUpdatedNotification is the params payload of
// `item/fileChange/patchUpdated` — the in-progress change set for an item
// that has not completed yet.
type FileChangePatchUpdatedNotification struct {
	ThreadId string             `json:"threadId"`
	TurnId   string             `json:"turnId"`
	ItemId   string             `json:"itemId"`
	Changes  []FileUpdateChange `json:"changes"`
}

// ReasoningSummaryPartAddedNotification is the params payload of
// `item/reasoning/summaryPartAdded`.
type ReasoningSummaryPartAddedNotification struct {
	ThreadId     string `json:"threadId"`
	TurnId       string `json:"turnId"`
	ItemId       string `json:"itemId"`
	SummaryIndex int64  `json:"summaryIndex"`
}

// ContextCompactedNotification is the params payload of
// `thread/compacted`.
type ContextCompactedNotification struct {
	ThreadId string `json:"threadId"`
	TurnId   string `json:"turnId"`
}

// McpServerStartupState values for McpServerStatusUpdatedNotification.
const (
	McpServerStarting  = "starting"
	McpServerReady     = "ready"
	McpServerFailed    = "failed"
	McpServerCancelled = "cancelled"
)

// McpServerStatusUpdatedNotification is the params payload of
// `mcpServer/startupStatus/updated`. It fires several times per server
// during connection setup.
type McpServerStatusUpdatedNotification struct {
	// ThreadId is nil for connection-scoped servers started outside any
	// thread.
	ThreadId *string `json:"threadId,omitempty"`
	Name     string  `json:"name"`
	// Status is one of the McpServer* constants; unknown values are
	// passed through verbatim.
	Status        string          `json:"status"`
	Error         *string         `json:"error,omitempty"`
	FailureReason json.RawMessage `json:"failureReason,omitempty"`
}

// ModelReroutedNotification is the params payload of `model/rerouted` —
// codex switched models mid-turn (capacity, safety, or policy).
type ModelReroutedNotification struct {
	ThreadId  string          `json:"threadId"`
	TurnId    string          `json:"turnId"`
	FromModel string          `json:"fromModel"`
	ToModel   string          `json:"toModel"`
	Reason    json.RawMessage `json:"reason"`
}

// WarningNotification is the params payload of both `warning` and
// `guardianWarning`. ThreadId is always set on the guardian variant and
// optional on the plain one.
type WarningNotification struct {
	Message  string  `json:"message"`
	ThreadId *string `json:"threadId,omitempty"`
}

// DeprecationNoticeNotification is the params payload of
// `deprecationNotice` — codex announcing a protocol surface going away.
// Worth logging: it is the earliest warning that this SDK needs an update.
type DeprecationNoticeNotification struct {
	Summary string  `json:"summary"`
	Details *string `json:"details,omitempty"`
}
