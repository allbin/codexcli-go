package schema

import "encoding/json"

// Server-initiated approval request method names. The constants are
// authoritative — keep this list in sync with /tmp/codex-schema/ServerRequest.json.
const (
	// MethodCommandExecutionRequestApproval is the v2 server request method
	// sent when the agent wants to run a shell command and the policy
	// requires explicit user approval.
	MethodCommandExecutionRequestApproval = "item/commandExecution/requestApproval"
	// MethodFileChangeRequestApproval is the v2 server request method sent
	// when the agent wants to apply a file change (add, delete, update).
	MethodFileChangeRequestApproval = "item/fileChange/requestApproval"
	// MethodPermissionsRequestApproval is the v2 server request method sent
	// by the built-in request_permissions tool to request additional sandbox
	// permissions for the remainder of the turn or session.
	MethodPermissionsRequestApproval = "item/permissions/requestApproval"
	// MethodExecCommandApproval is the legacy v1 server request method,
	// sent for turns dispatched via the deprecated SendUserTurn/Message APIs.
	MethodExecCommandApproval = "execCommandApproval"
	// MethodApplyPatchApproval is the legacy v1 server request method for
	// patch approval on the deprecated APIs.
	MethodApplyPatchApproval = "applyPatchApproval"
	// MethodToolRequestUserInput is the experimental server request method
	// for the request_user_input tool. Not handled as an approval; the
	// dispatcher exposes it via UnknownServerRequest.
	MethodToolRequestUserInput = "item/tool/requestUserInput"
	// MethodMcpServerElicitationRequest is the MCP elicitation request method.
	MethodMcpServerElicitationRequest = "mcpServer/elicitation/request"
)

// CommandExecutionRequestApprovalParams is the params payload of an
// `item/commandExecution/requestApproval` server request. Optional fields
// stay as pointers so consumers can tell "absent" from "empty".
type CommandExecutionRequestApprovalParams struct {
	ThreadID    string  `json:"threadId"`
	TurnID      string  `json:"turnId"`
	ItemID      string  `json:"itemId"`
	ApprovalID  *string `json:"approvalId,omitempty"`
	Command     *string `json:"command,omitempty"`
	Cwd         *string `json:"cwd,omitempty"`
	Reason      *string `json:"reason,omitempty"`
	StartedAtMs int64   `json:"startedAtMs"`

	CommandActions                  json.RawMessage `json:"commandActions,omitempty"`
	NetworkApprovalContext          json.RawMessage `json:"networkApprovalContext,omitempty"`
	ProposedExecpolicyAmendment     []string        `json:"proposedExecpolicyAmendment,omitempty"`
	ProposedNetworkPolicyAmendments json.RawMessage `json:"proposedNetworkPolicyAmendments,omitempty"`
	AdditionalPermissions           json.RawMessage `json:"additionalPermissions,omitempty"`
	AvailableDecisions              json.RawMessage `json:"availableDecisions,omitempty"`
}

// FileChangeRequestApprovalParams is the params payload of an
// `item/fileChange/requestApproval` server request.
type FileChangeRequestApprovalParams struct {
	ThreadID    string  `json:"threadId"`
	TurnID      string  `json:"turnId"`
	ItemID      string  `json:"itemId"`
	Reason      *string `json:"reason,omitempty"`
	GrantRoot   *string `json:"grantRoot,omitempty"`
	StartedAtMs int64   `json:"startedAtMs"`
}

// PermissionsRequestApprovalParams is the params payload of an
// `item/permissions/requestApproval` server request.
type PermissionsRequestApprovalParams struct {
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	ItemID      string          `json:"itemId"`
	Cwd         string          `json:"cwd"`
	Reason      *string         `json:"reason,omitempty"`
	StartedAtMs int64           `json:"startedAtMs"`
	Permissions json.RawMessage `json:"permissions"`
}

// ExecCommandApprovalParams is the params payload of the legacy
// `execCommandApproval` server request.
type ExecCommandApprovalParams struct {
	ConversationID string          `json:"conversationId"`
	CallID         string          `json:"callId"`
	Command        []string        `json:"command"`
	Cwd            string          `json:"cwd"`
	ParsedCmd      json.RawMessage `json:"parsedCmd"`
	Reason         *string         `json:"reason,omitempty"`
	ApprovalID     *string         `json:"approvalId,omitempty"`
}

// ApplyPatchApprovalParams is the params payload of the legacy
// `applyPatchApproval` server request.
type ApplyPatchApprovalParams struct {
	ConversationID string          `json:"conversationId"`
	CallID         string          `json:"callId"`
	FileChanges    json.RawMessage `json:"fileChanges"`
	GrantRoot      *string         `json:"grantRoot,omitempty"`
	Reason         *string         `json:"reason,omitempty"`
}

// TurnInterruptParams is the request payload for `turn/interrupt`.
type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
}

// TurnInterruptResponse is the empty `{}` response for turn/interrupt.
type TurnInterruptResponse struct{}
