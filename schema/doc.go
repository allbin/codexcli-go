// Package schema holds Go types that mirror the codex app-server JSON
// Schema bundle (`codex app-server generate-json-schema`). It is the
// statically typed surface that codexcli marshals into request params
// and unmarshals from notification payloads.
//
// The codex app-server emits one JSON Schema file per RPC method (~200
// files). Most are stable; the bundle ships v1 (initialize) and v2
// (everything else).
//
// # Code generation status
//
// We evaluated go-jsonschema (atombender) and oapi-codegen against the
// v2_raw schema bundle. Both produce non-idiomatic output: go-jsonschema
// generates ~700 lines per notification type with 40+ custom
// UnmarshalJSON methods containing inline validation boilerplate; the
// oneOf discriminated unions used by ThreadItem and SandboxPolicy produce
// nested type aliases rather than idiomatic Go unions. oapi-codegen
// targets OpenAPI, not raw JSON Schema Draft 7, so it requires a wrapper
// that loses fidelity on the codex-specific patterns.
//
// Decision: hand-written types stay. They're kept in sync with the raw
// schema via cmd/genschema (which refreshes v2_raw/) and manual diff
// review when upgrading codex. Typed fields cover all protocol surfaces
// consumers need; json.RawMessage provides forward compat for the rest.
package schema
