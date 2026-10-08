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
	// MethodAccountLoginStart is the `account/login/start` request: begin a
	// sign-in. For the ChatGPT flows it returns at once with a loginId; the
	// outcome arrives later as `account/login/completed`.
	MethodAccountLoginStart = "account/login/start"
	// MethodAccountLoginCancel is the `account/login/cancel` request: stop
	// the pending ChatGPT sign-in with the given loginId.
	MethodAccountLoginCancel = "account/login/cancel"
	// MethodAccountLogout is the `account/logout` request: remove the stored
	// credentials and cancel any pending sign-in.
	MethodAccountLogout = "account/logout"
	// MethodAccountLoginCompleted is the `account/login/completed`
	// notification: a sign-in started with `account/login/start` ended.
	MethodAccountLoginCompleted = "account/login/completed"
	// MethodAccountUpdated is the `account/updated` notification: the auth
	// mode or plan changed (after a sign-in or a logout).
	MethodAccountUpdated = "account/updated"
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

// LoginType discriminates `account/login/start` params and its reply.
// codex also defines chatgptAuthTokens ("OpenAI internal use only") and two
// experimental Amazon Bedrock variants; they are not bound.
type LoginType string

const (
	// LoginTypeAPIKey stores an OpenAI API key. The reply carries no
	// loginId: the key is saved before the server answers.
	LoginTypeAPIKey LoginType = "apiKey"
	// LoginTypeChatGPT is the browser OAuth flow. The reply carries AuthURL,
	// which redirects to a callback server codex runs on the app-server's
	// localhost, so it only completes in a browser on that machine.
	LoginTypeChatGPT LoginType = "chatgpt"
	// LoginTypeChatGPTDeviceCode is the device-code flow. The reply carries
	// UserCode and VerificationURL; the code is entered on any device.
	LoginTypeChatGPTDeviceCode LoginType = "chatgptDeviceCode"
)

// LoginAccountParams is the `account/login/start` request payload, a union
// discriminated on Type. APIKey is set for LoginTypeAPIKey only;
// CodexStreamlinedLogin for LoginTypeChatGPT only.
type LoginAccountParams struct {
	Type                  LoginType `json:"type"`
	APIKey                string    `json:"apiKey,omitempty"`
	CodexStreamlinedLogin bool      `json:"codexStreamlinedLogin,omitempty"`
}

// LoginAccountResponse is the `account/login/start` reply. Which fields are
// set depends on Type: chatgpt carries LoginID and AuthURL,
// chatgptDeviceCode carries LoginID, UserCode and VerificationURL, apiKey
// carries none.
type LoginAccountResponse struct {
	Type    LoginType `json:"type"`
	LoginID string    `json:"loginId,omitempty"`
	// AuthURL is the browser OAuth URL (chatgpt).
	AuthURL string `json:"authUrl,omitempty"`
	// UserCode is the one-time code the person enters (chatgptDeviceCode).
	UserCode string `json:"userCode,omitempty"`
	// VerificationURL is where the person enters UserCode
	// (chatgptDeviceCode).
	VerificationURL string `json:"verificationUrl,omitempty"`
}

// CancelLoginAccountParams is the `account/login/cancel` request payload.
type CancelLoginAccountParams struct {
	LoginID string `json:"loginId"`
}

// CancelLoginAccountStatus is the outcome of `account/login/cancel`.
type CancelLoginAccountStatus string

const (
	// CancelLoginCanceled means the sign-in was pending and is now stopped;
	// its `account/login/completed` (success false) follows.
	CancelLoginCanceled CancelLoginAccountStatus = "canceled"
	// CancelLoginNotFound means no pending sign-in has that id: it already
	// ended, was replaced by a newer one, or never existed.
	CancelLoginNotFound CancelLoginAccountStatus = "notFound"
)

// CancelLoginAccountResponse is the `account/login/cancel` reply.
type CancelLoginAccountResponse struct {
	Status CancelLoginAccountStatus `json:"status"`
}

// LogoutAccountResponse is the `account/logout` reply; it has no fields.
type LogoutAccountResponse struct{}

// AccountLoginCompletedNotification is the params payload of
// `account/login/completed`. LoginID is nil for a sign-in that had none
// (API key). Error is the server's message when Success is false.
type AccountLoginCompletedNotification struct {
	LoginID *string `json:"loginId,omitempty"`
	Success bool    `json:"success"`
	Error   *string `json:"error,omitempty"`
}

// AccountUpdatedNotification is the params payload of `account/updated`.
// Both fields are strings rather than enums: codex adds auth modes and
// plans between releases. AuthMode is nil after a logout.
type AccountUpdatedNotification struct {
	AuthMode *string `json:"authMode,omitempty"`
	PlanType *string `json:"planType,omitempty"`
}
