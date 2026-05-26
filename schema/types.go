package schema

import "encoding/json"

// ClientInfo identifies the calling product during the `initialize`
// handshake. Codex app-server uses this for OpenAI Compliance Logs
// routing, so consumers building enterprise integrations should set a
// stable, documented name.
type ClientInfo struct {
	Name    string  `json:"name"`
	Title   *string `json:"title,omitempty"`
	Version string  `json:"version"`
}

// InitializeCapabilities turns optional client features on or off.
type InitializeCapabilities struct {
	// ExperimentalApi opts into protocol surfaces flagged experimental
	// in the server-side README (e.g. dynamic tools, environments).
	ExperimentalApi bool `json:"experimentalApi,omitempty"`
	// OptOutNotificationMethods suppresses the named server-to-client
	// notification methods for this connection. Match is exact; unknown
	// names are accepted and ignored.
	OptOutNotificationMethods []string `json:"optOutNotificationMethods,omitempty"`
}

// InitializeParams is the `initialize` request payload (schema v1).
type InitializeParams struct {
	ClientInfo   ClientInfo              `json:"clientInfo"`
	Capabilities *InitializeCapabilities `json:"capabilities,omitempty"`
}

// InitializeResponse is the `initialize` reply.
type InitializeResponse struct {
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOs     string `json:"platformOs"`
	UserAgent      string `json:"userAgent"`
}

// AskForApproval is a tagged union. Simple variants are bare strings
// ("untrusted", "on-failure", "on-request", "never"); the "granular"
// variant is an object. RawMessage keeps both forms addressable without
// hand-rolling every shape — callers that need the granular form can
// unmarshal further.
//
// Implementation note: this is a struct wrapper rather than a named
// alias of json.RawMessage because Go does not inherit methods through
// a named-type definition. A bare `type AskForApproval json.RawMessage`
// would lose RawMessage's Marshal/UnmarshalJSON, making the underlying
// `[]byte` get base64-decoded on the wire.
type AskForApproval struct {
	Raw json.RawMessage
}

// MarshalJSON implements json.Marshaler.
func (a AskForApproval) MarshalJSON() ([]byte, error) {
	if len(a.Raw) == 0 {
		return []byte("null"), nil
	}
	return a.Raw, nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (a *AskForApproval) UnmarshalJSON(data []byte) error {
	a.Raw = append(a.Raw[:0], data...)
	return nil
}

// AskForApprovalString returns the bare-string variant if present
// ("untrusted", "on-failure", "on-request", "never"), or "" for the
// granular object form.
func (a AskForApproval) AskForApprovalString() string {
	var s string
	if err := json.Unmarshal(a.Raw, &s); err == nil {
		return s
	}
	return ""
}

// NewAskForApprovalString constructs a bare-string variant.
func NewAskForApprovalString(s string) AskForApproval {
	b, _ := json.Marshal(s)
	return AskForApproval{Raw: b}
}

// SandboxMode is the legacy thread-level sandbox shorthand.
// Values: "read-only", "workspace-write", "danger-full-access".
type SandboxMode string

const (
	SandboxReadOnly       SandboxMode = "read-only"
	SandboxWorkspaceWrite SandboxMode = "workspace-write"
	SandboxDangerFull     SandboxMode = "danger-full-access"
)

// ThreadStartParams is the `thread/start` request payload.
//
// Only the fields useful for the first-pass demo are typed; the rest of
// the server's accepted surface (collaboration mode, environments,
// permissions profile, etc.) ride along in Extra for forward compat and
// will be promoted to typed fields as consumers need them.
type ThreadStartParams struct {
	Cwd            *string         `json:"cwd,omitempty"`
	Model          *string         `json:"model,omitempty"`
	ModelProvider  *string         `json:"modelProvider,omitempty"`
	ApprovalPolicy *AskForApproval `json:"approvalPolicy,omitempty"`
	Sandbox        *SandboxMode    `json:"sandbox,omitempty"`
	Ephemeral      *bool           `json:"ephemeral,omitempty"`
	ServiceTier    *string         `json:"serviceTier,omitempty"`

	// Extra holds anything not modeled above. Merged into the JSON
	// object on marshal; consumer-supplied keys win over typed fields.
	Extra map[string]any `json:"-"`
}

// MarshalJSON merges Extra with typed fields so consumers can pass
// arbitrary protocol fields without waiting for typed support.
func (p ThreadStartParams) MarshalJSON() ([]byte, error) {
	return mergeMarshal(threadStartShape(p), p.Extra)
}

type threadStartShape ThreadStartParams

// Thread is the persisted conversation object returned by
// thread/start, thread/resume, thread/fork, and thread/read. Only the
// fields the demo path consumes are typed.
type Thread struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"sessionId"`
	Cwd           string          `json:"cwd"`
	CliVersion    string          `json:"cliVersion"`
	ModelProvider string          `json:"modelProvider"`
	Ephemeral     bool            `json:"ephemeral"`
	CreatedAt     int64           `json:"createdAt"`
	UpdatedAt     int64           `json:"updatedAt"`
	Preview       string          `json:"preview"`
	Status        json.RawMessage `json:"status,omitempty"`
	Name          *string         `json:"name,omitempty"`
	Path          *string         `json:"path,omitempty"`
}

// ThreadStartResponse is the `thread/start` / `thread/fork` reply.
type ThreadStartResponse struct {
	Thread         Thread          `json:"thread"`
	Cwd            string          `json:"cwd"`
	Model          string          `json:"model"`
	ModelProvider  string          `json:"modelProvider"`
	ApprovalPolicy AskForApproval  `json:"approvalPolicy"`
	Sandbox        json.RawMessage `json:"sandbox"`
	ServiceTier    *string         `json:"serviceTier,omitempty"`
}

// UserInput is the per-turn input union. Type discriminates: "text",
// "image", "localImage", "skill", "mention".
type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
	Name string `json:"name,omitempty"`
}

// TextInput is a convenience constructor for the most common case.
func TextInput(text string) UserInput { return UserInput{Type: "text", Text: text} }

// TurnStartParams is the `turn/start` request payload.
//
// Threading the same forward-compat strategy as ThreadStartParams:
// callers can drop into Extra for fields not yet promoted.
type TurnStartParams struct {
	ThreadId       string          `json:"threadId"`
	Input          []UserInput     `json:"input"`
	Cwd            *string         `json:"cwd,omitempty"`
	Model          *string         `json:"model,omitempty"`
	Effort         *string         `json:"effort,omitempty"`
	ApprovalPolicy *AskForApproval `json:"approvalPolicy,omitempty"`
	SandboxPolicy  json.RawMessage `json:"sandboxPolicy,omitempty"`

	Extra map[string]any `json:"-"`
}

func (p TurnStartParams) MarshalJSON() ([]byte, error) {
	return mergeMarshal(turnStartShape(p), p.Extra)
}

type turnStartShape TurnStartParams

// TurnStartResponse is the synchronous reply to `turn/start`. Streamed
// progress arrives separately via notifications.
type TurnStartResponse struct {
	Turn Turn `json:"turn"`
}

// TurnStatus values match the server enum.
type TurnStatus string

const (
	TurnInProgress  TurnStatus = "inProgress"
	TurnCompleted   TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnFailed      TurnStatus = "failed"
)

// Turn is the per-turn state object. Items is empty in turn/started and
// turn/completed notifications today; rely on item/* notifications for
// the canonical incremental view.
type Turn struct {
	ID          string            `json:"id"`
	Status      TurnStatus        `json:"status"`
	Items       []ThreadItem      `json:"items"`
	Usage       *ThreadTokenUsage `json:"usage,omitempty"`
	StartedAt   *int64            `json:"startedAt,omitempty"`
	CompletedAt *int64            `json:"completedAt,omitempty"`
	DurationMs  *int64            `json:"durationMs,omitempty"`
	Error       *TurnError        `json:"error,omitempty"`
	ItemsView   string            `json:"itemsView,omitempty"`
}

// TurnError carries the failure payload on `turn.status: "failed"` and
// on the `error` notification mid-turn.
type TurnError struct {
	Message           string          `json:"message"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo,omitempty"`
	AdditionalDetails *string         `json:"additionalDetails,omitempty"`
}

// ThreadItem is the union carried in turn responses and item/*
// notifications. Type discriminates ("userMessage", "agentMessage",
// "commandExecution", "fileChange", "mcpToolCall", etc.). Only the
// "agentMessage" shape is broken out today — everything else stays as
// Raw and consumers can drill in by type.
type ThreadItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`

	// AgentMessage-specific fields. Populated when Type == "agentMessage".
	Text string `json:"text,omitempty"`

	// Raw preserves the full JSON for future-typing and forward compat.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps both the typed projection and the raw bytes.
func (t *ThreadItem) UnmarshalJSON(data []byte) error {
	type alias ThreadItem
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*t = ThreadItem(a)
	t.Raw = append(t.Raw[:0], data...)
	return nil
}

// TokenUsageBreakdown holds per-turn or aggregate token counts.
type TokenUsageBreakdown struct {
	TotalTokens           int64 `json:"totalTokens"`
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
}

// ThreadTokenUsage is the top-level usage snapshot emitted by the server
// on thread/tokenUsage/updated and carried in turn/completed.
type ThreadTokenUsage struct {
	Last               TokenUsageBreakdown `json:"last"`
	Total              TokenUsageBreakdown `json:"total"`
	ModelContextWindow *int64              `json:"modelContextWindow,omitempty"`
}

// RateLimitWindow describes a single rate-limit tier.
type RateLimitWindow struct {
	UsedPercent       int    `json:"usedPercent"`
	ResetsAt          *int64 `json:"resetsAt,omitempty"`
	WindowDurationMin *int64 `json:"windowDurationMins,omitempty"`
}

// CreditsSnapshot carries the user's credit balance state.
type CreditsSnapshot struct {
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance,omitempty"`
}

// RateLimitSnapshot is the aggregate rate-limit payload from
// account/rateLimits/updated.
type RateLimitSnapshot struct {
	Primary              *RateLimitWindow `json:"primary,omitempty"`
	Secondary            *RateLimitWindow `json:"secondary,omitempty"`
	Credits              *CreditsSnapshot `json:"credits,omitempty"`
	PlanType             *string          `json:"planType,omitempty"`
	LimitID              *string          `json:"limitId,omitempty"`
	LimitName            *string          `json:"limitName,omitempty"`
	RateLimitReachedType *string          `json:"rateLimitReachedType,omitempty"`
}

// ThreadResumeParams is the `thread/resume` request payload.
type ThreadResumeParams struct {
	ThreadId       string          `json:"threadId"`
	Cwd            *string         `json:"cwd,omitempty"`
	Model          *string         `json:"model,omitempty"`
	ModelProvider  *string         `json:"modelProvider,omitempty"`
	ApprovalPolicy *AskForApproval `json:"approvalPolicy,omitempty"`
	Sandbox        *SandboxMode    `json:"sandbox,omitempty"`
	ServiceTier    *string         `json:"serviceTier,omitempty"`

	Extra map[string]any `json:"-"`
}

// MarshalJSON merges Extra with typed fields.
func (p ThreadResumeParams) MarshalJSON() ([]byte, error) {
	return mergeMarshal(threadResumeShape(p), p.Extra)
}

type threadResumeShape ThreadResumeParams

// ThreadResumeResponse is the `thread/resume` reply. It shares the same
// shape as ThreadStartResponse.
type ThreadResumeResponse = ThreadStartResponse

// ModelListParams is the `model/list` request payload.
type ModelListParams struct{}

// ModelListResponse is the `model/list` reply.
type ModelListResponse struct {
	Models []json.RawMessage `json:"models"`
}

// Notification payload types — server -> client.

type ThreadStartedNotification struct {
	Thread Thread `json:"thread"`
}

type TurnStartedNotification struct {
	ThreadId string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

type TurnCompletedNotification struct {
	ThreadId string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

type ItemStartedNotification struct {
	ThreadId    string     `json:"threadId"`
	TurnId      string     `json:"turnId"`
	StartedAtMs int64      `json:"startedAtMs"`
	Item        ThreadItem `json:"item"`
}

type ItemCompletedNotification struct {
	ThreadId string     `json:"threadId"`
	TurnId   string     `json:"turnId"`
	Item     ThreadItem `json:"item"`
}

type AgentMessageDeltaNotification struct {
	ThreadId string `json:"threadId"`
	TurnId   string `json:"turnId"`
	ItemId   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// CommandExecutionOutputDeltaNotification is the params payload of
// `item/commandExecution/outputDelta` — streamed command stdout/stderr.
type CommandExecutionOutputDeltaNotification struct {
	ThreadId string `json:"threadId"`
	TurnId   string `json:"turnId"`
	ItemId   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// FileChangeOutputDeltaNotification is the params payload of
// `item/fileChange/outputDelta` — streamed file-change progress.
type FileChangeOutputDeltaNotification struct {
	ThreadId string `json:"threadId"`
	TurnId   string `json:"turnId"`
	ItemId   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// ReasoningTextDeltaNotification is the params payload of
// `item/reasoning/textDelta` — streamed chain-of-thought text.
type ReasoningTextDeltaNotification struct {
	ThreadId     string `json:"threadId"`
	TurnId       string `json:"turnId"`
	ItemId       string `json:"itemId"`
	Delta        string `json:"delta"`
	ContentIndex int64  `json:"contentIndex"`
}

// ReasoningSummaryTextDeltaNotification is the params payload of
// `item/reasoning/summaryTextDelta` — streamed reasoning summary.
type ReasoningSummaryTextDeltaNotification struct {
	ThreadId     string `json:"threadId"`
	TurnId       string `json:"turnId"`
	ItemId       string `json:"itemId"`
	Delta        string `json:"delta"`
	SummaryIndex int64  `json:"summaryIndex"`
}

// PlanDeltaNotification is the params payload of `item/plan/delta` —
// streamed proposed plan updates (experimental).
type PlanDeltaNotification struct {
	ThreadId string `json:"threadId"`
	TurnId   string `json:"turnId"`
	ItemId   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// TurnDiffUpdatedNotification is the params payload of `turn/diff/updated`
// — aggregated unified diff across all file changes in the turn.
type TurnDiffUpdatedNotification struct {
	ThreadId string `json:"threadId"`
	TurnId   string `json:"turnId"`
	Diff     string `json:"diff"`
}

// AccountRateLimitsUpdatedNotification is the params payload of
// `account/rateLimits/updated`.
type AccountRateLimitsUpdatedNotification struct {
	RateLimits RateLimitSnapshot `json:"rateLimits"`
}

// ThreadTokenUsageUpdatedNotification is the params payload of
// `thread/tokenUsage/updated`.
type ThreadTokenUsageUpdatedNotification struct {
	ThreadId   string           `json:"threadId"`
	TurnId     string           `json:"turnId"`
	TokenUsage ThreadTokenUsage `json:"tokenUsage"`
}

type ErrorNotification struct {
	ThreadId  *string   `json:"threadId,omitempty"`
	TurnId    *string   `json:"turnId,omitempty"`
	Error     TurnError `json:"error"`
	WillRetry bool      `json:"willRetry,omitempty"`
}
