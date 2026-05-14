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
type AskForApproval json.RawMessage

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
	Ephemeral     bool             `json:"ephemeral"`
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
	ID          string          `json:"id"`
	Status      TurnStatus      `json:"status"`
	Items       []ThreadItem    `json:"items"`
	StartedAt   *int64          `json:"startedAt,omitempty"`
	CompletedAt *int64          `json:"completedAt,omitempty"`
	DurationMs  *int64          `json:"durationMs,omitempty"`
	Error       *TurnError      `json:"error,omitempty"`
	ItemsView   string          `json:"itemsView,omitempty"`
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
	ThreadId     string     `json:"threadId"`
	TurnId       string     `json:"turnId"`
	StartedAtMs  int64      `json:"startedAtMs"`
	Item         ThreadItem `json:"item"`
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

type ErrorNotification struct {
	ThreadId *string   `json:"threadId,omitempty"`
	TurnId   *string   `json:"turnId,omitempty"`
	Error    TurnError `json:"error"`
}
