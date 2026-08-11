package schema

import (
	"encoding/json"
	"strings"
)

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
	// Extensions declares MCP extension settings, keyed by extension name
	// (e.g. "openai/form"). Shapes are extension-defined.
	Extensions map[string]any `json:"extensions,omitempty"`
	// RequestAttestation opts into `attestation/generate` server requests.
	// Only set it if you also register a handler for that method via
	// codexcli.WithServerRequestHandler.
	RequestAttestation bool `json:"requestAttestation,omitempty"`
	// McpServerOpenaiFormElicitation is the legacy opt-in for the
	// "openai/form" MCP extension.
	//
	// Deprecated: declare "openai/form" in Extensions instead.
	McpServerOpenaiFormElicitation bool `json:"mcpServerOpenaiFormElicitation,omitempty"`
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
// ("untrusted", "on-request", "never"); the "granular" variant is an
// object. RawMessage keeps both forms addressable without hand-rolling
// every shape — callers that need the granular form can unmarshal further.
//
// Note "on-failure" was accepted by older codex releases and was dropped
// from the enum; send one of the constants below instead.
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

// Bare-string AskForApproval variants accepted by codex 0.147.
const (
	// ApprovalUntrusted prompts for anything outside the trusted-command
	// allowlist.
	ApprovalUntrusted = "untrusted"
	// ApprovalOnRequest prompts only when the agent explicitly escalates.
	ApprovalOnRequest = "on-request"
	// ApprovalNever never prompts; blocked actions simply fail.
	ApprovalNever = "never"
)

// AskForApprovalString returns the bare-string variant if present
// (ApprovalUntrusted, ApprovalOnRequest, ApprovalNever), or "" for the
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

// GranularApproval is the object form of an approval policy: per-category
// toggles that decide which agent actions require explicit approval.
//
// The server only accepts this form when the connection opted into the
// experimental API (codexcli.WithExperimentalAPI); without it, thread/start
// rejects the policy with "askForApproval.granular requires experimentalApi
// capability".
//
// McpElicitations, Rules, and SandboxApproval are required by the wire
// protocol and always serialized. RequestPermissions and SkillApproval
// default to false and are omitted when false.
type GranularApproval struct {
	McpElicitations    bool `json:"mcp_elicitations"`
	Rules              bool `json:"rules"`
	SandboxApproval    bool `json:"sandbox_approval"`
	RequestPermissions bool `json:"request_permissions,omitempty"`
	// SkillApproval gates skill use. Note: codex has no skill-specific
	// approval request type — when this fires it reuses an existing
	// request shape (e.g. commandExecution for a skill the model reads
	// from disk), which the SDK's approval routing already handles.
	SkillApproval bool `json:"skill_approval,omitempty"`
}

// NewGranularApproval constructs the granular ("object") approval variant.
// Pair it with codexcli.WithExperimentalAPI, or the server rejects it.
func NewGranularApproval(g GranularApproval) AskForApproval {
	b, _ := json.Marshal(struct {
		Granular GranularApproval `json:"granular"`
	}{Granular: g})
	return AskForApproval{Raw: b}
}

// Granular returns the granular variant and true when the policy is in
// object form, or a zero value and false for the bare-string variant.
func (a AskForApproval) Granular() (GranularApproval, bool) {
	var wrap struct {
		Granular *GranularApproval `json:"granular"`
	}
	if err := json.Unmarshal(a.Raw, &wrap); err != nil || wrap.Granular == nil {
		return GranularApproval{}, false
	}
	return *wrap.Granular, true
}

// SandboxMode is the legacy thread-level sandbox shorthand.
// Values: "read-only", "workspace-write", "danger-full-access".
type SandboxMode string

const (
	SandboxReadOnly       SandboxMode = "read-only"
	SandboxWorkspaceWrite SandboxMode = "workspace-write"
	SandboxDangerFull     SandboxMode = "danger-full-access"
)

// ApprovalsReviewer selects who reviews approval requests. Defaults to
// ApprovalsReviewerUser.
type ApprovalsReviewer string

const (
	// ApprovalsReviewerUser routes approvals to the client (this SDK's
	// ApprovalFunc).
	ApprovalsReviewerUser ApprovalsReviewer = "user"
	// ApprovalsReviewerAuto lets a codex subagent decide, applying a
	// risk-based framework. The client sees the outcome via the
	// item/autoApprovalReview/* notifications rather than an approval
	// request.
	ApprovalsReviewerAuto ApprovalsReviewer = "auto_review"
)

// Personality selects the assistant's conversational register. Only
// honored by models whose Model.SupportsPersonality is true.
type Personality string

const (
	PersonalityNone      Personality = "none"
	PersonalityFriendly  Personality = "friendly"
	PersonalityPragmatic Personality = "pragmatic"
)

// ThreadStartParams is the `thread/start` request payload.
//
// The commonly used surface is typed; the remainder of what the server
// accepts (collaboration mode, environments, permissions profile, etc.)
// rides along in Extra for forward compat and is promoted to typed fields
// as consumers need it.
type ThreadStartParams struct {
	Cwd            *string         `json:"cwd,omitempty"`
	Model          *string         `json:"model,omitempty"`
	ModelProvider  *string         `json:"modelProvider,omitempty"`
	ApprovalPolicy *AskForApproval `json:"approvalPolicy,omitempty"`
	Sandbox        *SandboxMode    `json:"sandbox,omitempty"`
	Ephemeral      *bool           `json:"ephemeral,omitempty"`
	ServiceTier    *string         `json:"serviceTier,omitempty"`

	// ApprovalsReviewer overrides where approval requests are routed.
	ApprovalsReviewer *ApprovalsReviewer `json:"approvalsReviewer,omitempty"`
	// Personality overrides the assistant's register for the thread.
	Personality *Personality `json:"personality,omitempty"`
	// BaseInstructions replaces the model's built-in system prompt.
	BaseInstructions *string `json:"baseInstructions,omitempty"`
	// DeveloperInstructions are appended to the system prompt rather than
	// replacing it — the right knob for per-product guidance.
	DeveloperInstructions *string `json:"developerInstructions,omitempty"`
	// Config overlays config.toml values for this thread only, using the
	// same dotted-path shape as the CLI's `-c key=value`.
	Config map[string]any `json:"config,omitempty"`

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
// thread/start, thread/resume, thread/fork, and thread/read.
type Thread struct {
	ID            string `json:"id"`
	SessionID     string `json:"sessionId"`
	Cwd           string `json:"cwd"`
	CliVersion    string `json:"cliVersion"`
	ModelProvider string `json:"modelProvider"`
	Ephemeral     bool   `json:"ephemeral"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
	Preview       string `json:"preview"`
	// Status is the thread's runtime status union; read it via
	// StatusType, or decode Status yourself for the active-flag detail.
	Status json.RawMessage `json:"status,omitempty"`
	Name   *string         `json:"name,omitempty"`
	Path   *string         `json:"path,omitempty"`

	// Source records which surface created the thread — one of "cli",
	// "vscode", "exec", "appServer", "unknown", or an object for the
	// custom/subAgent variants.
	Source json.RawMessage `json:"source,omitempty"`
	// ThreadSource is the optional client-supplied analytics
	// classification passed at thread/start.
	ThreadSource *string `json:"threadSource,omitempty"`
	// RecencyAt is the Unix-seconds timestamp used for recency ordering,
	// which can differ from UpdatedAt.
	RecencyAt *int64 `json:"recencyAt,omitempty"`
	// ForkedFromID is the source thread when this one came from
	// thread/fork.
	ForkedFromID *string `json:"forkedFromId,omitempty"`
	// ParentThreadID is set only when this thread is a subagent.
	ParentThreadID *string `json:"parentThreadId,omitempty"`
	// AgentRole and AgentNickname are assigned to sub-agents spawned via
	// codex's multi-agent collaboration tools.
	AgentRole     *string `json:"agentRole,omitempty"`
	AgentNickname *string `json:"agentNickname,omitempty"`
	// GitInfo is the repo metadata captured at thread creation.
	GitInfo json.RawMessage `json:"gitInfo,omitempty"`
	// Section is the persisted section the thread currently sits in.
	Section          json.RawMessage `json:"section,omitempty"`
	SectionEnteredAt *int64          `json:"sectionEnteredAt,omitempty"`
	// Turns is populated only on thread/resume, thread/rollback,
	// thread/fork, and thread/read (with includeTurns). It is empty on
	// thread/start.
	Turns []Turn `json:"turns,omitempty"`
}

// ThreadStatusType values for Thread.StatusType.
const (
	ThreadStatusNotLoaded   = "notLoaded"
	ThreadStatusIdle        = "idle"
	ThreadStatusActive      = "active"
	ThreadStatusSystemError = "systemError"
)

// StatusType returns the thread's status discriminator — one of the
// ThreadStatus* constants — or "" when absent or unparseable.
func (t Thread) StatusType() string { return statusType(t.Status) }

func statusType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var wrap struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &wrap) != nil {
		return ""
	}
	return wrap.Type
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

	// ApprovalsReviewer is the reviewer in effect for this thread.
	ApprovalsReviewer ApprovalsReviewer `json:"approvalsReviewer,omitempty"`
	// ReasoningEffort is the effort resolved for the thread, or nil when
	// the model has no reasoning levels.
	ReasoningEffort *string `json:"reasoningEffort,omitempty"`
	// InstructionSources lists the AGENTS.md-style files loaded into the
	// system prompt for this thread.
	InstructionSources []string `json:"instructionSources,omitempty"`
	// RuntimeWorkspaceRoots are the directories the sandbox treats as the
	// workspace.
	RuntimeWorkspaceRoots []string `json:"runtimeWorkspaceRoots,omitempty"`
	// ActivePermissionProfile is the named permission profile in effect,
	// when one is configured.
	ActivePermissionProfile json.RawMessage `json:"activePermissionProfile,omitempty"`
	// MultiAgentMode is the delegation policy: "explicitRequestOnly",
	// "proactive", or a {"custom": "..."} object.
	MultiAgentMode json.RawMessage `json:"multiAgentMode,omitempty"`
}

// UserInput is the per-turn input union. Type discriminates: "text",
// "image", "localImage", "audio", "localAudio", "skill", "mention".
//
// The same union is echoed back on the server side as the content blocks
// of a "userMessage" thread item (see ThreadItem.UserMessageContent), so
// this struct doubles as both the input and the read-back shape. Detail and
// TextElements are populated only on read-back; the input constructors
// leave them unset.
type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
	Name string `json:"name,omitempty"`
	// Detail is the image fidelity hint on "image"/"localImage" blocks.
	Detail *string `json:"detail,omitempty"`
	// TextElements are UI-defined spans within Text on "text" blocks (e.g.
	// rendered placeholders). Present on read-back, omitted on input.
	TextElements []TextElement `json:"text_elements,omitempty"`
}

// TextElement is a span within a text block's Text buffer that the UI
// renders or persists specially (an @-mention chip, a file pill, etc.).
type TextElement struct {
	ByteRange   ByteRange `json:"byteRange"`
	Placeholder *string   `json:"placeholder,omitempty"`
}

// ByteRange is a [Start, End) byte span within a parent text buffer.
type ByteRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// TextInput is a convenience constructor for the most common case.
func TextInput(text string) UserInput { return UserInput{Type: "text", Text: text} }

// ImageInput builds an "image" input block referencing a remote URL. The
// codex app-server fetches the URL; use LocalImageInput for a file on the
// machine running codex.
func ImageInput(url string) UserInput { return UserInput{Type: "image", URL: url} }

// LocalImageInput builds a "localImage" input block referencing a file
// path readable by the codex process.
func LocalImageInput(path string) UserInput { return UserInput{Type: "localImage", Path: path} }

// AudioInput builds an "audio" input block referencing a remote URL. Only
// accepted by models whose Model.InputModalities include "audio".
func AudioInput(url string) UserInput { return UserInput{Type: "audio", URL: url} }

// LocalAudioInput builds a "localAudio" input block referencing a file
// path readable by the codex process.
func LocalAudioInput(path string) UserInput { return UserInput{Type: "localAudio", Path: path} }

// SkillInput builds a "skill" input block that invokes a codex skill by
// name. Both name and path are required by the wire protocol; obtain them
// from a SkillMetadata returned by the skills/list RPC (see
// codexcli.Conn.ListSkills) rather than hand-constructing paths, since the
// path is resolved per working directory.
func SkillInput(name, path string) UserInput {
	return UserInput{Type: "skill", Name: name, Path: path}
}

// MentionInput builds a "mention" input block referencing a file by name
// and path, mirroring an editor @-mention. Both fields are required by the
// wire protocol.
func MentionInput(name, path string) UserInput {
	return UserInput{Type: "mention", Name: name, Path: path}
}

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

	// ServiceTier overrides the billing/throughput tier for this turn and
	// subsequent turns. Values come from Model.ServiceTiers.
	ServiceTier *string `json:"serviceTier,omitempty"`
	// ApprovalsReviewer overrides approval routing for this turn onward.
	ApprovalsReviewer *ApprovalsReviewer `json:"approvalsReviewer,omitempty"`
	// Personality overrides the assistant's register for this turn onward.
	Personality *Personality `json:"personality,omitempty"`
	// Summary overrides the reasoning-summary mode for this turn onward.
	Summary *string `json:"summary,omitempty"`
	// OutputSchema constrains the turn's final assistant message to a JSON
	// Schema. Encode the schema itself, not a wrapper.
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	// ClientUserMessageID is an opaque client-side id echoed back on the
	// resulting userMessage item as ThreadItem.ClientID, letting a UI
	// match its optimistic render to the server's item.
	ClientUserMessageID *string `json:"clientUserMessageId,omitempty"`

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

// Turn is the per-turn state object. Items carries only a display summary
// on turn/completed (ItemsView == TurnItemsSummary) and is empty on
// turn/started (TurnItemsNotLoaded); rely on item/* notifications for the
// canonical incremental view.
type Turn struct {
	ID     string       `json:"id"`
	Status TurnStatus   `json:"status"`
	Items  []ThreadItem `json:"items"`
	// Usage is no longer sent by codex app-server (removed from the Turn
	// schema after 0.133) and is therefore always nil against a current
	// CLI.
	//
	// Deprecated: subscribe to thread/tokenUsage/updated instead — see
	// codexcli.TokenUsageUpdatedEvent.
	Usage       *ThreadTokenUsage `json:"usage,omitempty"`
	StartedAt   *int64            `json:"startedAt,omitempty"`
	CompletedAt *int64            `json:"completedAt,omitempty"`
	DurationMs  *int64            `json:"durationMs,omitempty"`
	Error       *TurnError        `json:"error,omitempty"`
	// ItemsView reports how much of Items was loaded. One of
	// TurnItemsNotLoaded, TurnItemsSummary, TurnItemsFull.
	ItemsView TurnItemsView `json:"itemsView,omitempty"`
}

// TurnItemsView describes how much of a Turn's Items array was populated.
type TurnItemsView string

const (
	// TurnItemsNotLoaded means Items is intentionally empty.
	TurnItemsNotLoaded TurnItemsView = "notLoaded"
	// TurnItemsSummary means Items holds only a display summary.
	TurnItemsSummary TurnItemsView = "summary"
	// TurnItemsFull means Items holds every persisted item for the turn.
	TurnItemsFull TurnItemsView = "full"
)

// TurnError carries the failure payload on `turn.status: "failed"` and
// on the `error` notification mid-turn.
type TurnError struct {
	Message           string          `json:"message"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo,omitempty"`
	AdditionalDetails *string         `json:"additionalDetails,omitempty"`
}

// ThreadItem is the union carried in turn responses and item/*
// notifications. Type discriminates ("userMessage", "agentMessage",
// "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall",
// "reasoning", "plan", etc.).
//
// Common fields are promoted to typed fields above. Type-specific payloads
// that arrive as JSON arrays/objects are reachable via typed accessors:
// FileChanges() decodes a fileChange item's changes; CommandActions()
// decodes a commandExecution item's parsed intents and CommandLiteral()
// returns its unwrapped shell command. Raw preserves the full JSON for
// forward compatibility and for payloads without a typed accessor yet.
type ThreadItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`

	// AgentMessage / Plan fields. Phase is agentMessage-only and reports
	// whether the text is interim commentary or the turn's final answer
	// (MessagePhase* constants); it is nil when the provider omits it.
	Text  string  `json:"text,omitempty"`
	Phase *string `json:"phase,omitempty"`
	// MemoryCitation lists the stored memories an agentMessage drew on.
	MemoryCitation json.RawMessage `json:"memoryCitation,omitempty"`

	// UserMessage fields. Content is also used by reasoning items with a
	// different element shape; reach userMessage content via
	// UserMessageContent(), which guards on Type.
	Content json.RawMessage `json:"content,omitempty"`
	// ClientID echoes TurnStartParams.ClientUserMessageID.
	ClientID *string `json:"clientId,omitempty"`

	// Reasoning fields. Summary holds the human-readable summary blocks;
	// Content holds the raw reasoning text blocks (both []string).
	Summary []string `json:"summary,omitempty"`

	// CommandExecution fields. CommandActionsRaw holds the parsed-intent
	// array verbatim; decode it via the CommandActions() accessor.
	Command           *string         `json:"command,omitempty"`
	Cwd               *string         `json:"cwd,omitempty"`
	ExitCode          *int            `json:"exitCode,omitempty"`
	AggregatedOutput  *string         `json:"aggregatedOutput,omitempty"`
	Source            *string         `json:"source,omitempty"`
	CommandActionsRaw json.RawMessage `json:"commandActions,omitempty"`
	// ProcessID identifies the underlying PTY process, when codex ran the
	// command through one. Pairs with the terminalInteraction
	// notification.
	ProcessID *string `json:"processId,omitempty"`
	// ScriptPath and PluginID are set when the command resolves to a
	// single trusted first-party plugin script.
	ScriptPath *string `json:"scriptPath,omitempty"`
	PluginID   *string `json:"pluginId,omitempty"`

	// FileChange fields.
	Changes json.RawMessage `json:"changes,omitempty"`

	// McpToolCall fields.
	Tool      *string         `json:"tool,omitempty"`
	Server    *string         `json:"server,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	McpResult json.RawMessage `json:"result,omitempty"`
	McpError  json.RawMessage `json:"error,omitempty"`
	Namespace *string         `json:"namespace,omitempty"`
	// ReadOnlyHint mirrors the MCP tool's own read-only advertisement —
	// useful for deciding whether a call needs an approval prompt.
	ReadOnlyHint *bool `json:"readOnlyHint,omitempty"`
	// AppContext carries the MCP app resource backing this call.
	AppContext json.RawMessage `json:"appContext,omitempty"`

	// DynamicToolCall fields.
	Success      *bool           `json:"success,omitempty"`
	ContentItems json.RawMessage `json:"contentItems,omitempty"`

	// WebSearch fields. Action describes the specific browse step
	// (search / openPage / findInPage); Results is opaque JSON.
	Query   string          `json:"query,omitempty"`
	Action  json.RawMessage `json:"action,omitempty"`
	Results json.RawMessage `json:"results,omitempty"`

	// Path is the viewed image on an imageView item.
	Path *string `json:"path,omitempty"`
	// Review is the review name on enteredReviewMode / exitedReviewMode.
	Review *string `json:"review,omitempty"`

	// Shared fields across tool-like items.
	Status     *string `json:"status,omitempty"`
	DurationMs *int64  `json:"durationMs,omitempty"`

	// Raw preserves the full JSON for future-typing and forward compat.
	// Item shapes not projected above (hookPrompt, collabAgentToolCall,
	// subAgentActivity, imageGeneration) are reachable here.
	Raw json.RawMessage `json:"-"`
}

// ThreadItemType constants for the known item type discriminators.
const (
	ItemTypeUserMessage      = "userMessage"
	ItemTypeAgentMessage     = "agentMessage"
	ItemTypePlan             = "plan"
	ItemTypeReasoning        = "reasoning"
	ItemTypeCommandExecution = "commandExecution"
	ItemTypeFileChange       = "fileChange"
	ItemTypeMcpToolCall      = "mcpToolCall"
	ItemTypeDynamicToolCall  = "dynamicToolCall"
	// Item types added in codex 0.14x.
	ItemTypeHookPrompt          = "hookPrompt"
	ItemTypeCollabAgentToolCall = "collabAgentToolCall"
	ItemTypeSubAgentActivity    = "subAgentActivity"
	ItemTypeWebSearch           = "webSearch"
	ItemTypeImageView           = "imageView"
	ItemTypeImageGeneration     = "imageGeneration"
	ItemTypeSleep               = "sleep"
	ItemTypeEnteredReviewMode   = "enteredReviewMode"
	ItemTypeExitedReviewMode    = "exitedReviewMode"
	ItemTypeContextCompaction   = "contextCompaction"
)

// MessagePhase values for ThreadItem.Phase on agentMessage items.
// Providers do not emit these consistently — treat a nil Phase as
// "unknown" rather than assuming commentary.
const (
	MessagePhaseCommentary  = "commentary"
	MessagePhaseFinalAnswer = "final_answer"
)

// CommandExecutionSource values for ThreadItem.Source.
const (
	CommandSourceAgent                  = "agent"
	CommandSourceUserShell              = "userShell"
	CommandSourceUnifiedExecStartup     = "unifiedExecStartup"
	CommandSourceUnifiedExecInteraction = "unifiedExecInteraction"
)

// Item status values shared by commandExecution, fileChange, mcpToolCall,
// and dynamicToolCall items. "declined" only occurs on the first two.
const (
	ItemStatusInProgress = "inProgress"
	ItemStatusCompleted  = "completed"
	ItemStatusFailed     = "failed"
	ItemStatusDeclined   = "declined"
)

// ReasoningContent returns the raw reasoning text blocks of a "reasoning"
// item. Returns nil for other item types or on parse error.
func (t *ThreadItem) ReasoningContent() []string {
	if t.Type != ItemTypeReasoning || len(t.Content) == 0 {
		return nil
	}
	var out []string
	_ = json.Unmarshal(t.Content, &out)
	return out
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

// FileUpdateChange is a single entry in a fileChange item's changes array.
// KindRaw preserves the wire form of the change discriminator, which codex
// emits in several shapes; decode it via the Kind() accessor.
type FileUpdateChange struct {
	Path    string          `json:"path"`
	Diff    string          `json:"diff"`
	KindRaw json.RawMessage `json:"kind"`
}

// Kind normalizes the change discriminator to one of "add", "update", or
// "delete". It tolerates every shape codex has emitted: a bare string
// ("add"), an internally-tagged object ({"type":"add"}), and an
// externally-tagged enum ({"add":{…}}). Returns "" when absent or
// unrecognized.
func (c FileUpdateChange) Kind() string {
	if len(c.KindRaw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(c.KindRaw, &str) == nil {
		return str
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(c.KindRaw, &obj) == nil {
		if t, ok := obj["type"]; ok {
			var ts string
			if json.Unmarshal(t, &ts) == nil {
				return ts
			}
		}
		for k := range obj {
			return k
		}
	}
	return ""
}

// FileChanges parses the changes array when Type == "fileChange".
// Returns nil for non-fileChange items or on parse error.
func (t *ThreadItem) FileChanges() []FileUpdateChange {
	if len(t.Changes) == 0 {
		return nil
	}
	var out []FileUpdateChange
	_ = json.Unmarshal(t.Changes, &out)
	return out
}

// UserMessageContent parses the content blocks of a "userMessage" item
// into the UserInput union — the text, skill, mention, and image blocks
// the turn was started with, echoed back by the server. Returns nil for
// non-userMessage items (the "content" field is also used by reasoning
// items, with a different shape) or on parse error.
func (t *ThreadItem) UserMessageContent() []UserInput {
	if t.Type != ItemTypeUserMessage || len(t.Content) == 0 {
		return nil
	}
	var out []UserInput
	_ = json.Unmarshal(t.Content, &out)
	return out
}

// CommandAction is one entry of a commandExecution item's commandActions
// array — codex's parsed intent for the command it intends to run. Type
// discriminates the shape: "read" carries a Path plus a display Name,
// "listFiles" carries an optional Path, "search" carries a Query (and
// usually a Path), and "unknown" carries only the literal Command.
// Command is present on every variant and holds the shell command codex
// derived the intent from.
type CommandAction struct {
	Type string `json:"type"`
	// Command is the literal shell command behind this parsed intent.
	Command string `json:"command,omitempty"`
	// Name is the display label carried on "read" actions.
	Name  string `json:"name,omitempty"`
	Path  string `json:"path,omitempty"`
	Query string `json:"query,omitempty"`

	// Cmd is the pre-0.140 wire name for Command.
	//
	// Deprecated: use Command. Kept populated (in both directions) by
	// UnmarshalJSON so code written against either codex generation keeps
	// working, and so transcripts recorded before the rename still decode.
	Cmd string `json:"-"`
}

// UnmarshalJSON accepts both the current "command" key and the legacy
// "cmd" key, mirroring whichever is present onto both Command and Cmd.
func (a *CommandAction) UnmarshalJSON(data []byte) error {
	type alias CommandAction
	var v struct {
		alias
		LegacyCmd string `json:"cmd,omitempty"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*a = CommandAction(v.alias)
	if a.Command == "" {
		a.Command = v.LegacyCmd
	}
	a.Cmd = a.Command
	return nil
}

// MarshalJSON emits the current "command" key, falling back to a
// Cmd-only value set by callers constructing actions by hand.
func (a CommandAction) MarshalJSON() ([]byte, error) {
	type alias CommandAction
	if a.Command == "" {
		a.Command = a.Cmd
	}
	return json.Marshal(alias(a))
}

// ParseCommandActions decodes a commandActions / parsedCmd array into typed
// CommandAction entries. It is the shared parse behind ThreadItem,
// CommandExecutionRequestApprovalParams, and ExecCommandApprovalParams.
// Returns nil for empty input or on parse error.
func ParseCommandActions(raw json.RawMessage) []CommandAction {
	if len(raw) == 0 {
		return nil
	}
	var out []CommandAction
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// CommandActions parses the parsed-intent array on a commandExecution item.
// Returns nil for items without command actions or on parse error.
func (t *ThreadItem) CommandActions() []CommandAction {
	return ParseCommandActions(t.CommandActionsRaw)
}

// CommandLiteral returns the human-readable command for a commandExecution
// item, unwrapping the "<shell> -lc '…'" envelope codex wraps shell commands
// in (see UnwrapShellCommand). It prefers the item's Command field and falls
// back to the literal command recorded on the first parsed action. Returns ""
// when neither is present.
func (t *ThreadItem) CommandLiteral() string {
	if t.Command != nil && *t.Command != "" {
		return UnwrapShellCommand(*t.Command)
	}
	for _, a := range t.CommandActions() {
		if a.Command != "" {
			return UnwrapShellCommand(a.Command)
		}
	}
	return ""
}

// UnwrapShellCommand extracts the inner script from a "<shell> -lc '…'" (or
// "-c") invocation — e.g. "/usr/bin/bash -lc 'cat foo.txt'" yields
// "cat foo.txt" — decoding the POSIX '\” escape for embedded single quotes.
// Input that isn't wrapped is returned trimmed but otherwise unchanged.
func UnwrapShellCommand(cmd string) string {
	s := strings.TrimSpace(cmd)
	for _, flag := range []string{" -lc ", " -c ", " -lc\t", " -c\t"} {
		if i := strings.Index(s, flag); i >= 0 {
			return unquoteSingle(strings.TrimSpace(s[i+len(flag):]))
		}
	}
	return s
}

// unquoteSingle strips a surrounding pair of single quotes and decodes the
// POSIX '\” escape sequence for embedded single quotes.
func unquoteSingle(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, `'`)
	}
	return s
}

// TokenUsageBreakdown holds per-turn or aggregate token counts.
type TokenUsageBreakdown struct {
	TotalTokens           int64 `json:"totalTokens"`
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	// CacheWriteInputTokens counts input tokens written into the prompt
	// cache (billed separately from CachedInputTokens reads).
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
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

// SpendControlLimitSnapshot describes a per-user spend cap. Limit and Used
// are decimal currency amounts as strings.
type SpendControlLimitSnapshot struct {
	Limit            string `json:"limit"`
	Used             string `json:"used"`
	RemainingPercent int    `json:"remainingPercent"`
	ResetsAt         int64  `json:"resetsAt"`
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
	// IndividualLimit is the caller's personal spend cap within a
	// workspace, when one is configured.
	IndividualLimit *SpendControlLimitSnapshot `json:"individualLimit,omitempty"`
	// SpendControlReached is the backend-reported spend-control state.
	// nil means "unavailable", not "not reached".
	SpendControlReached *bool `json:"spendControlReached,omitempty"`
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

	ApprovalsReviewer     *ApprovalsReviewer `json:"approvalsReviewer,omitempty"`
	Personality           *Personality       `json:"personality,omitempty"`
	BaseInstructions      *string            `json:"baseInstructions,omitempty"`
	DeveloperInstructions *string            `json:"developerInstructions,omitempty"`
	Config                map[string]any     `json:"config,omitempty"`

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

// ModelListParams is the `model/list` request payload. All fields are
// optional; the zero value requests the first page of visible models.
type ModelListParams struct {
	// Cursor is the opaque pagination cursor from a previous reply's
	// NextCursor.
	Cursor *string `json:"cursor,omitempty"`
	// Limit is the page size. Nil lets the server pick.
	Limit *int `json:"limit,omitempty"`
	// IncludeHidden also returns models the default picker hides.
	IncludeHidden *bool `json:"includeHidden,omitempty"`
}

// ModelListResponse is the `model/list` reply.
//
// Note the field is "data", not "models" — codex renamed it (and reshaped
// the entries) between 0.133 and 0.147.
type ModelListResponse struct {
	Data []Model `json:"data"`
	// NextCursor is non-nil when more pages remain.
	NextCursor *string `json:"nextCursor,omitempty"`
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
	// CompletedAtMs is the Unix-milliseconds timestamp when the item's
	// lifecycle ended. Pairs with ItemStartedNotification.StartedAtMs.
	CompletedAtMs int64 `json:"completedAtMs"`
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
