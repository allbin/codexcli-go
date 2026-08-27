package schema

import "encoding/json"

// Account RPC method names. Keep in sync with the bundle `go generate
// ./schema` writes to schema/v2_raw/: ClientRequest.json.
const (
	// MethodAccountRead is the `account/read` request: report the account
	// the app-server is currently signed in as.
	MethodAccountRead = "account/read"
	// MethodAccountRateLimitsRead is the `account/rateLimits/read` request:
	// pull the current rate-limit snapshot on demand. Unlike the
	// `account/rateLimits/updated` notification it does not need a thread.
	MethodAccountRateLimitsRead = "account/rateLimits/read"
)

// AccountType discriminates the `account/read` reply. Unknown values are
// passed through verbatim for forward compatibility.
type AccountType string

const (
	// AccountTypeAPIKey means codex authenticates with an OpenAI API key.
	// No plan or email is reported for this mode.
	AccountTypeAPIKey AccountType = "apiKey"
	// AccountTypeChatGPT means codex authenticates against a ChatGPT
	// subscription; Email and PlanType are populated.
	AccountTypeChatGPT AccountType = "chatgpt"
	// AccountTypeAmazonBedrock means codex authenticates against Amazon
	// Bedrock; UsesCodexManagedCredentials is populated.
	AccountTypeAmazonBedrock AccountType = "amazonBedrock"
)

// Account is the signed-in account as reported by `account/read`. The wire
// shape is a union discriminated on Type, so which optional fields are
// present depends on Type: chatgpt carries Email and PlanType, amazonBedrock
// carries UsesCodexManagedCredentials, apiKey carries neither.
//
// PlanType is a string rather than an enum for the same reason it is one on
// RateLimitSnapshot: the server's plan vocabulary grows between releases
// (free, go, plus, pro, prolite, team, business, enterprise, edu, ... plus
// an explicit "unknown"), and a closed Go enum would reject new values.
type Account struct {
	Type AccountType `json:"type"`
	// Email is the ChatGPT account email. Nil for non-chatgpt accounts, and
	// nil for a chatgpt account whose email the server did not report.
	Email *string `json:"email,omitempty"`
	// PlanType is the ChatGPT plan slug (e.g. "plus", "pro", "team"). Nil
	// for non-chatgpt accounts.
	PlanType *string `json:"planType,omitempty"`
	// UsesCodexManagedCredentials reports whether codex manages the Bedrock
	// credentials itself. Nil for non-amazonBedrock accounts.
	UsesCodexManagedCredentials *bool `json:"usesCodexManagedCredentials,omitempty"`
}

// AccountReadParams is the `account/read` request payload.
type AccountReadParams struct {
	// RefreshToken asks for a proactive token refresh before the server
	// replies. It is honoured in managed auth mode and ignored in external
	// auth mode, where the client is expected to refresh tokens itself and
	// re-login via `account/login/start`.
	RefreshToken bool `json:"refreshToken,omitempty"`
}

// AccountReadResponse is the `account/read` reply.
type AccountReadResponse struct {
	// Account is nil when nobody is signed in.
	Account *Account `json:"account"`
	// RequiresOpenaiAuth reports whether the configured model provider needs
	// OpenAI credentials at all. It is false for provider setups (e.g. a
	// third-party OSS endpoint) where a missing account is not a problem.
	RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
}

// AccountRateLimitsReadResponse is the `account/rateLimits/read` reply.
//
// RateLimits is the same single-bucket shape the
// `account/rateLimits/updated` notification carries, so a consumer can hold
// one RateLimitSnapshot and refresh it from either source.
type AccountRateLimitsReadResponse struct {
	// RateLimits is the backward-compatible single-bucket view.
	RateLimits RateLimitSnapshot `json:"rateLimits"`
	// RateLimitsByLimitID is the multi-bucket view keyed by metered limit id
	// (e.g. "codex"). Nil when the server reports only the single bucket.
	RateLimitsByLimitID map[string]RateLimitSnapshot `json:"rateLimitsByLimitId,omitempty"`
	// RateLimitResetCredits carries the one-off rate-limit reset credits the
	// account has been granted. Left raw for forward compatibility — the
	// credit shape is still moving between codex releases and no consumer
	// needs it typed yet.
	RateLimitResetCredits json.RawMessage `json:"rateLimitResetCredits,omitempty"`
}
