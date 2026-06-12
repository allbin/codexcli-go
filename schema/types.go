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

	// AgentMessage / Plan fields.
	Text  string  `json:"text,omitempty"`
	Phase *string `json:"phase,omitempty"`

	// UserMessage / Reasoning fields. Both item types carry a "content"
	// array but with different element shapes; reach userMessage content
	// via UserMessageContent(), which guards on Type.
	Content json.RawMessage `json:"content,omitempty"`

	// CommandExecution fields. CommandActionsRaw holds the parsed-intent
	// array verbatim; decode it via the CommandActions() accessor.
	Command           *string         `json:"command,omitempty"`
	Cwd               *string         `json:"cwd,omitempty"`
	ExitCode          *int            `json:"exitCode,omitempty"`
	AggregatedOutput  *string         `json:"aggregatedOutput,omitempty"`
	Source            *string         `json:"source,omitempty"`
	CommandActionsRaw json.RawMessage `json:"commandActions,omitempty"`

	// FileChange fields.
	Changes json.RawMessage `json:"changes,omitempty"`

	// McpToolCall fields.
	Tool      *string         `json:"tool,omitempty"`
	Server    *string         `json:"server,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	McpResult json.RawMessage `json:"result,omitempty"`
	McpError  json.RawMessage `json:"error,omitempty"`
	Namespace *string         `json:"namespace,omitempty"`

	// Shared fields across tool-like items.
	Status     *string `json:"status,omitempty"`
	DurationMs *int64  `json:"durationMs,omitempty"`

	// Raw preserves the full JSON for future-typing and forward compat.
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
)

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
// discriminates the shape: "read"/"update" carry a Path, "search" carries
// a Query (and usually a Path), and "unknown" carries only the literal Cmd.
// Cmd, when present, holds the shell command codex derived the intent from.
type CommandAction struct {
	Type  string `json:"type"`
	Path  string `json:"path,omitempty"`
	Query string `json:"query,omitempty"`
	Cmd   string `json:"cmd,omitempty"`
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
// back to the literal Cmd recorded on the first parsed action. Returns ""
// when neither is present.
func (t *ThreadItem) CommandLiteral() string {
	if t.Command != nil && *t.Command != "" {
		return UnwrapShellCommand(*t.Command)
	}
	for _, a := range t.CommandActions() {
		if a.Cmd != "" {
			return UnwrapShellCommand(a.Cmd)
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
