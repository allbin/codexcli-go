package codexcli

import (
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
)

// JSON-RPC error codes the app-server uses to reject a request outright.
// codex rejects an unrecognised method while deserializing the
// ClientRequest union, so it answers with InvalidRequest rather than the
// spec's MethodNotFound; both are treated as "not supported".
const (
	rpcCodeInvalidRequest = -32600
	rpcCodeMethodNotFound = -32601
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
