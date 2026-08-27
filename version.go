package codexcli

// SDKVersion is the codexcli-go release identifier sent in client metadata
// during the `initialize` handshake. Bump on every public-API change.
const SDKVersion = "0.3.0"

// DefaultClientName is sent as `initialize.params.clientInfo.name`. Override
// via WithClientInfo when embedding codexcli-go in a named product —
// codex app-server uses this to identify the client in Compliance Logs.
const DefaultClientName = "codexcli_go"
