// Package codexcli wraps the `codex app-server` JSON-RPC protocol as a Go
// client. The public API mirrors github.com/allbin/claudecli-go: consumers
// can swap implementations by changing the import path.
//
// Transport: stdio (newline-delimited JSON). Each line on stdin/stdout is a
// single JSON-RPC 2.0 message (request, response, or notification). The
// `"jsonrpc":"2.0"` header is intentionally omitted on the wire by the
// codex app-server, matching MCP-style framing.
//
// Lifecycle: every connection begins with a single `initialize` request
// from the client and an `initialized` notification before any other call
// is accepted. Client.New starts the subprocess and performs the handshake
// before returning.
//
// Use Client.Run(ctx, prompt) to start a thread, dispatch a turn, and
// stream typed events until `turn/completed`. For finer-grained control,
// use Client.NewThread followed by Thread.Run on a long-lived thread.
//
// Inventory: ListModels reads the codex CLI's on-disk model cache
// ($CODEX_HOME/models_cache.json) and returns a typed slice. UI callers
// driving a picker should filter for ModelInfo.Visibility == VisibilityList.
//
// This first pass intentionally omits approvals, MCP elicitation,
// fork/resume, dynamic tools, and the file/exec/account surfaces. Those
// land incrementally as consumer needs surface.
package codexcli
