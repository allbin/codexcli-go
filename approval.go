package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/allbin/codexcli-go/schema"
)

// ApprovalRequest is the sealed interface implemented by every typed
// approval payload arriving as a server-initiated JSON-RPC request.
//
// Design note: a single generic callback (see ApprovalFunc) handles every
// approval kind. This mirrors the existing sealed Event surface so
// consumers stay on one type-switch pattern; per-kind functional options
// would have multiplied surface area without making forward-compat
// cheaper, since new approval kinds can simply add a new ApprovalRequest
// implementer plus a default branch.
type ApprovalRequest interface {
	approvalRequest()
	// Method returns the JSON-RPC method that delivered this approval
	// request (e.g. schema.MethodCommandExecutionRequestApproval). Useful
	// when callers need to log or branch by raw protocol method.
	Method() string
	// ThreadID returns the thread the approval relates to. May be empty
	// for legacy approvals that only carry a conversation id.
	ThreadID() string
	// TurnID returns the turn the approval relates to. May be empty for
	// legacy approvals.
	TurnID() string
}

// CommandExecutionApprovalRequest is delivered when the agent wants to
// run a shell command on a v2 turn. Fields are surfaced unchanged from
// the wire schema; consult the Params getter for fields not promoted
// directly (network amendments, exec policy amendments, etc.).
type CommandExecutionApprovalRequest struct {
	Params schema.CommandExecutionRequestApprovalParams
}

func (*CommandExecutionApprovalRequest) approvalRequest() {}

// Method returns the JSON-RPC method name.
func (*CommandExecutionApprovalRequest) Method() string {
	return schema.MethodCommandExecutionRequestApproval
}

// ThreadID returns the thread id.
func (r *CommandExecutionApprovalRequest) ThreadID() string { return r.Params.ThreadID }

// TurnID returns the turn id.
func (r *CommandExecutionApprovalRequest) TurnID() string { return r.Params.TurnID }

// FileChangeApprovalRequest is delivered when the agent proposes a file
// change on a v2 turn.
type FileChangeApprovalRequest struct {
	Params schema.FileChangeRequestApprovalParams
}

func (*FileChangeApprovalRequest) approvalRequest() {}

// Method returns the JSON-RPC method name.
func (*FileChangeApprovalRequest) Method() string { return schema.MethodFileChangeRequestApproval }

// ThreadID returns the thread id.
func (r *FileChangeApprovalRequest) ThreadID() string { return r.Params.ThreadID }

// TurnID returns the turn id.
func (r *FileChangeApprovalRequest) TurnID() string { return r.Params.TurnID }

// PermissionsApprovalRequest is delivered by the request_permissions tool.
type PermissionsApprovalRequest struct {
	Params schema.PermissionsRequestApprovalParams
}

func (*PermissionsApprovalRequest) approvalRequest() {}

// Method returns the JSON-RPC method name.
func (*PermissionsApprovalRequest) Method() string { return schema.MethodPermissionsRequestApproval }

// ThreadID returns the thread id.
func (r *PermissionsApprovalRequest) ThreadID() string { return r.Params.ThreadID }

// TurnID returns the turn id.
func (r *PermissionsApprovalRequest) TurnID() string { return r.Params.TurnID }

// ExecCommandApprovalRequest is the legacy v1 shell-command approval.
type ExecCommandApprovalRequest struct {
	Params schema.ExecCommandApprovalParams
}

func (*ExecCommandApprovalRequest) approvalRequest() {}

// Method returns the JSON-RPC method name.
func (*ExecCommandApprovalRequest) Method() string { return schema.MethodExecCommandApproval }

// ThreadID returns the conversation id (legacy field maps to thread id).
func (r *ExecCommandApprovalRequest) ThreadID() string { return r.Params.ConversationID }

// TurnID returns the call id (legacy turns don't carry a separate turn id).
func (r *ExecCommandApprovalRequest) TurnID() string { return r.Params.CallID }

// ApplyPatchApprovalRequest is the legacy v1 patch approval.
type ApplyPatchApprovalRequest struct {
	Params schema.ApplyPatchApprovalParams
}

func (*ApplyPatchApprovalRequest) approvalRequest() {}

// Method returns the JSON-RPC method name.
func (*ApplyPatchApprovalRequest) Method() string { return schema.MethodApplyPatchApproval }

// ThreadID returns the conversation id (legacy field maps to thread id).
func (r *ApplyPatchApprovalRequest) ThreadID() string { return r.Params.ConversationID }

// TurnID returns the call id (legacy turns don't carry a separate turn id).
func (r *ApplyPatchApprovalRequest) TurnID() string { return r.Params.CallID }

// ApprovalDecision is the sealed response interface. Each concrete type
// marshals to the wire shape the server expects for the matched request.
type ApprovalDecision interface {
	approvalDecision()
	// marshalDecision returns the JSON body to send back as the response
	// result for a given request method. Returning ErrApprovalNotSupported
	// signals the dispatcher to send a JSON-RPC method-not-found-style
	// error response instead.
	marshalDecision(method string) (json.RawMessage, error)
}

// ErrApprovalNotSupported is returned by ApprovalDecision implementations
// when a decision shape is not legal for the request method (e.g. a
// PermissionGrant returned for a CommandExecutionApprovalRequest).
var ErrApprovalNotSupported = errors.New("codexcli: approval decision not supported for this method")

// Accept approves the action with no policy mutation. Wire form depends
// on the request kind — for command/file-change approvals it serializes
// to `{"decision":"accept"}`; for legacy execCommandApproval it serializes
// to `{"decision":"approved"}`.
type Accept struct{}

func (Accept) approvalDecision() {}
func (Accept) marshalDecision(method string) (json.RawMessage, error) {
	switch method {
	case schema.MethodCommandExecutionRequestApproval,
		schema.MethodFileChangeRequestApproval:
		return decisionJSON("accept")
	case schema.MethodExecCommandApproval, schema.MethodApplyPatchApproval:
		return decisionJSON("approved")
	default:
		return nil, fmt.Errorf("%w: Accept on %s", ErrApprovalNotSupported, method)
	}
}

// AcceptForSession approves and caches the decision for the rest of the
// codex session (sticky approval).
type AcceptForSession struct{}

func (AcceptForSession) approvalDecision() {}
func (AcceptForSession) marshalDecision(method string) (json.RawMessage, error) {
	switch method {
	case schema.MethodCommandExecutionRequestApproval,
		schema.MethodFileChangeRequestApproval:
		return decisionJSON("acceptForSession")
	case schema.MethodExecCommandApproval, schema.MethodApplyPatchApproval:
		return decisionJSON("approved_for_session")
	default:
		return nil, fmt.Errorf("%w: AcceptForSession on %s", ErrApprovalNotSupported, method)
	}
}

// Decline denies the action; the agent continues the turn but skips this
// proposal.
type Decline struct{}

func (Decline) approvalDecision() {}
func (Decline) marshalDecision(method string) (json.RawMessage, error) {
	switch method {
	case schema.MethodCommandExecutionRequestApproval,
		schema.MethodFileChangeRequestApproval:
		return decisionJSON("decline")
	case schema.MethodExecCommandApproval, schema.MethodApplyPatchApproval:
		return decisionJSON("denied")
	default:
		return nil, fmt.Errorf("%w: Decline on %s", ErrApprovalNotSupported, method)
	}
}

// Cancel denies the action and immediately interrupts the turn.
type Cancel struct{}

func (Cancel) approvalDecision() {}
func (Cancel) marshalDecision(method string) (json.RawMessage, error) {
	switch method {
	case schema.MethodCommandExecutionRequestApproval,
		schema.MethodFileChangeRequestApproval:
		return decisionJSON("cancel")
	case schema.MethodExecCommandApproval, schema.MethodApplyPatchApproval:
		return decisionJSON("abort")
	default:
		return nil, fmt.Errorf("%w: Cancel on %s", ErrApprovalNotSupported, method)
	}
}

// PermissionGrant is the response to PermissionsApprovalRequest. The
// granted permissions are forwarded verbatim; the consumer is expected
// to construct the JSON subset matching the server's RequestPermissionProfile
// shape. Scope defaults to "turn".
type PermissionGrant struct {
	// Permissions is the granted subset (JSON object). Use json.RawMessage
	// directly so consumers can pass an arbitrary subset without us
	// having to mirror the full schema. May be nil to deny all.
	Permissions json.RawMessage
	// Scope is "turn" (default) or "session".
	Scope string
	// StrictAutoReview, when non-nil, reviews every subsequent command
	// in this turn even after the grant.
	StrictAutoReview *bool
}

func (PermissionGrant) approvalDecision() {}
func (p PermissionGrant) marshalDecision(method string) (json.RawMessage, error) {
	if method != schema.MethodPermissionsRequestApproval {
		return nil, fmt.Errorf("%w: PermissionGrant on %s", ErrApprovalNotSupported, method)
	}
	perms := p.Permissions
	if len(perms) == 0 {
		perms = json.RawMessage(`{}`)
	}
	payload := map[string]any{"permissions": perms}
	if p.Scope != "" {
		payload["scope"] = p.Scope
	}
	if p.StrictAutoReview != nil {
		payload["strictAutoReview"] = *p.StrictAutoReview
	}
	return json.Marshal(payload)
}

// RawDecision lets callers ship a raw JSON result body when none of the
// typed decisions fit (e.g. acceptWithExecpolicyAmendment). Body must be
// the full result object (with a top-level "decision" field if the
// request kind requires one).
type RawDecision struct {
	Body json.RawMessage
}

func (RawDecision) approvalDecision() {}
func (r RawDecision) marshalDecision(_ string) (json.RawMessage, error) {
	if len(r.Body) == 0 {
		return nil, errors.New("codexcli: RawDecision.Body is empty")
	}
	return r.Body, nil
}

// ApprovalFunc is the user-supplied callback. Returning a nil decision
// without an error is treated as Decline (a hard-fail default chosen so
// consumers who forget to handle a kind don't accidentally approve).
// Returning an error sends a JSON-RPC error response — the agent will
// surface that as a tool failure.
type ApprovalFunc func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)

// DenyAll is a ready-made ApprovalFunc that declines every request. Useful
// as a deliberate "don't approve anything" guardrail in tests or
// non-interactive contexts.
func DenyAll(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
	return Decline{}, nil
}

func decisionJSON(value string) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"decision": value})
}

// decodeApprovalRequest deserializes the params bytes into a typed
// ApprovalRequest based on the method. Returns (nil, nil) for methods
// that aren't approvals so the caller can fall through to its own
// generic handler.
func decodeApprovalRequest(method string, params json.RawMessage) (ApprovalRequest, error) {
	switch method {
	case schema.MethodCommandExecutionRequestApproval:
		var p schema.CommandExecutionRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return &CommandExecutionApprovalRequest{Params: p}, nil
	case schema.MethodFileChangeRequestApproval:
		var p schema.FileChangeRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return &FileChangeApprovalRequest{Params: p}, nil
	case schema.MethodPermissionsRequestApproval:
		var p schema.PermissionsRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return &PermissionsApprovalRequest{Params: p}, nil
	case schema.MethodExecCommandApproval:
		var p schema.ExecCommandApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return &ExecCommandApprovalRequest{Params: p}, nil
	case schema.MethodApplyPatchApproval:
		var p schema.ApplyPatchApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return &ApplyPatchApprovalRequest{Params: p}, nil
	default:
		return nil, nil
	}
}

// UnknownServerRequest carries a server-initiated JSON-RPC request that
// the dispatcher does not understand. Hand back a typed event so the
// stream consumer can decide how to respond — by default the dispatcher
// emits a method-not-found error response.
type UnknownServerRequest struct {
	Method string
	Params json.RawMessage
}
