package codexcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors. Use errors.Is to test from any error in the chain.
var (
	// ErrNotInitialized is returned when a request hits the server
	// before the initialize/initialized handshake completes.
	ErrNotInitialized = errors.New("codexcli: not initialized")
	// ErrAlreadyInitialized is returned if initialize is sent twice on
	// the same connection.
	ErrAlreadyInitialized = errors.New("codexcli: already initialized")
	// ErrTurnFailed wraps a turn that ended with status: failed.
	ErrTurnFailed = errors.New("codexcli: turn failed")
	// ErrThreadNotFound is returned by ResumeThread when the server
	// cannot locate the requested thread (deleted, never existed, etc.).
	ErrThreadNotFound = errors.New("codexcli: thread not found")
	// ErrMethodNotSupported is returned when the app-server rejects a
	// request because it does not implement that method — an older codex,
	// a different server, or a method gated behind a capability this
	// connection did not negotiate. It is the signal to degrade the
	// feature to "unavailable" rather than treat it as a failure.
	ErrMethodNotSupported = errors.New("codexcli: method not supported by app-server")
	// ErrNotSignedIn is returned by Conn.Account when the app-server
	// answers `account/read` with no account, i.e. nobody is logged in.
	ErrNotSignedIn = errors.New("codexcli: no account signed in")
	// ErrNoActiveTurn is returned by Thread.SendMessage when the thread
	// has no turn to inject into: none was running, or the turn it saw
	// ended before codex received the message. Nothing was delivered.
	// Start a turn instead.
	ErrNoActiveTurn = errors.New("codexcli: no active turn")
	// ErrTurnNotSteerable is returned when the active turn refuses new
	// input: a review turn or a compaction turn. Codex refuses both
	// turn/steer and turn/start until that turn ends, so buffer the
	// message and send it once the turn completes.
	ErrTurnNotSteerable = errors.New("codexcli: active turn not steerable")
)

// classifyTurnInputError maps codex's rejections of turn/steer and
// turn/start onto the turn sentinels, or returns nil when err is neither.
// Messages observed live against codex 0.159.3:
//
//	turn/steer, idle thread:     -32600 "no active turn to steer"
//	turn/steer, other turn id:   -32600 "expected active turn id `a` but found `b`"
//	turn/steer, compaction turn: -32600 "cannot steer a compact turn",
//	                             data.codexErrorInfo.activeTurnNotSteerable.turnKind = "compact"
//	turn/start, compaction turn: -32603 "failed to submit turn input: ActiveTurnNotSteerable { turn_kind: Compact }"
func classifyTurnInputError(err error) error {
	var rerr *rpcError
	if !errors.As(err, &rerr) {
		return nil
	}
	if isNotSteerable(rerr) {
		return fmt.Errorf("%w: %s", ErrTurnNotSteerable, rerr.Message)
	}
	if rerr.Code != rpcCodeInvalidRequest {
		return nil
	}
	if rerr.Message == "no active turn to steer" || strings.HasPrefix(rerr.Message, "expected active turn id ") {
		return fmt.Errorf("%w: %s", ErrNoActiveTurn, rerr.Message)
	}
	return nil
}

// isNotSteerable reports an activeTurnNotSteerable rejection, from
// data.codexErrorInfo when present and the message otherwise. turn/start
// carries no data, only the Rust variant name in its message.
func isNotSteerable(rerr *rpcError) bool {
	var data struct {
		CodexErrorInfo struct {
			ActiveTurnNotSteerable *struct{} `json:"activeTurnNotSteerable"`
		} `json:"codexErrorInfo"`
	}
	if len(rerr.Data) > 0 && json.Unmarshal(rerr.Data, &data) == nil && data.CodexErrorInfo.ActiveTurnNotSteerable != nil {
		return true
	}
	switch rerr.Code {
	case rpcCodeInvalidRequest:
		return strings.HasPrefix(rerr.Message, "cannot steer a ")
	case rpcCodeInternalError:
		return strings.Contains(rerr.Message, "ActiveTurnNotSteerable")
	}
	return false
}

// JSON-RPC error codes the app-server uses to reject a request outright.
// codex rejects an unrecognised method while deserializing the
// ClientRequest union, so it answers with InvalidRequest rather than the
// spec's MethodNotFound; both are treated as "not supported".
const (
	rpcCodeInvalidRequest = -32600
	rpcCodeMethodNotFound = -32601
	rpcCodeInternalError  = -32603
)

// isMethodNotSupportedError reports whether an rpc error means the server
// will never serve this method, as opposed to failing to serve it now.
//
// Matching is deliberately message-based for -32600: codex reuses that
// code for genuinely malformed params too, and only the message
// distinguishes "unknown variant `foo/bar`" (verified live against codex
// 0.148) and "<method> requires experimentalApi capability" from a
// request the server understood but disliked.
func isMethodNotSupportedError(err error) bool {
	var rerr *rpcError
	if !errors.As(err, &rerr) {
		return false
	}
	if rerr.Code == rpcCodeMethodNotFound {
		return true
	}
	if rerr.Code != rpcCodeInvalidRequest {
		return false
	}
	msg := strings.ToLower(rerr.Message)
	for _, needle := range []string{
		"unknown variant", "method not found", "unknown method",
		"unsupported method", "requires experimentalapi capability",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// ExitError carries a non-zero process exit and any captured stderr.
type ExitError struct {
	ExitCode int
	Stderr   string
	Err      error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("codexcli: process exit %d: %s", e.ExitCode, e.Err)
	}
	if e.Stderr != "" {
		s := e.Stderr
		if len(s) > 256 {
			s = s[:256] + "..."
		}
		return fmt.Sprintf("codexcli: process exit %d: %s", e.ExitCode, s)
	}
	return fmt.Sprintf("codexcli: process exit %d", e.ExitCode)
}

func (e *ExitError) Unwrap() error { return e.Err }
