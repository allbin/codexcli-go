package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// accountHandlers maps a request method to the scripted server reply.
type accountHandlers map[string]func(id, params json.RawMessage)

// accountServer runs the scripted server side of an account RPC test: it
// completes the initialize handshake, then dispatches client requests to
// the handlers keyed by method until the connection closes. A method with
// no handler is read and left unanswered, which is how the timeout case is
// scripted.
func accountServer(t *testing.T, fix *BidiFixtureExecutor, handlers accountHandlers) {
	t.Helper()
	id, _ := expectRequest(t, fix, "initialize")
	_ = fix.SendResponse(id, basicInitResponse())
	expectNotification(t, fix, "initialized")

	for {
		f, err := fix.ReadFrame()
		if err != nil {
			return
		}
		if !f.IsRequest() {
			continue
		}
		if h, ok := handlers[f.Method]; ok {
			h(f.ID, f.Params)
		}
	}
}

// connectForAccount wires a fixture-backed connection whose server side is
// scripted by the handlers build returns. build receives the fixture so a
// handler can write its reply; the map is fully populated before the server
// goroutine starts, so nothing races on it.
func connectForAccount(t *testing.T, build func(fix *BidiFixtureExecutor) accountHandlers) *Conn {
	t.Helper()
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())
	handlers := build(fix)
	go accountServer(t, fix, handlers)

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// liveRateLimitsResult is the `account/rateLimits/read` payload captured
// from codex 0.148 against a live ChatGPT account, with the used-percent
// values raised off zero and the credit list emptied. It exercises the
// multi-bucket view and the raw reset-credit passthrough alongside the
// single-bucket snapshot.
const liveRateLimitsResult = `{
  "rateLimits": {
    "limitId": "codex",
    "limitName": null,
    "primary": {"usedPercent": 12, "windowDurationMins": 300, "resetsAt": 1787838968},
    "secondary": {"usedPercent": 43, "windowDurationMins": 10080, "resetsAt": 1788425768},
    "credits": {"hasCredits": false, "unlimited": false, "balance": "0"},
    "individualLimit": null,
    "spendControlReached": false,
    "planType": "plus",
    "rateLimitReachedType": null
  },
  "rateLimitsByLimitId": {
    "codex": {
      "limitId": "codex",
      "primary": {"usedPercent": 12, "windowDurationMins": 300, "resetsAt": 1787838968}
    }
  },
  "rateLimitResetCredits": {"availableCount": 1, "credits": []}
}`

// TestConnAccountRateLimits_Snapshot drives a normal response through the
// fake app-server and checks both the wire frame and the decoded snapshot.
func TestConnAccountRateLimits_Snapshot(t *testing.T) {
	gotParams := make(chan json.RawMessage, 1)
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountRateLimitsRead: func(id, params json.RawMessage) {
				gotParams <- params
				_ = fix.SendRawResponse(id, json.RawMessage(liveRateLimitsResult))
			},
		}
	})

	snap, err := conn.AccountRateLimits(context.Background())
	if err != nil {
		t.Fatalf("AccountRateLimits: %v", err)
	}

	select {
	case p := <-gotParams:
		// codex declares the params `undefined`; sending a params key at
		// all is what a stricter server would reject.
		if len(p) != 0 {
			t.Errorf("params = %s, want omitted", string(p))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no account/rateLimits/read frame")
	}

	if snap.Primary == nil || snap.Primary.UsedPercent != 12 {
		t.Fatalf("primary = %+v, want usedPercent 12", snap.Primary)
	}
	if snap.Primary.WindowDurationMin == nil || *snap.Primary.WindowDurationMin != 300 {
		t.Errorf("primary window = %v, want 300", snap.Primary.WindowDurationMin)
	}
	if snap.Primary.ResetsAt == nil || *snap.Primary.ResetsAt != 1787838968 {
		t.Errorf("primary resetsAt = %v, want 1787838968", snap.Primary.ResetsAt)
	}
	if snap.Secondary == nil || snap.Secondary.UsedPercent != 43 {
		t.Errorf("secondary = %+v, want usedPercent 43", snap.Secondary)
	}
	if snap.PlanType == nil || *snap.PlanType != "plus" {
		t.Errorf("planType = %v, want plus", snap.PlanType)
	}
	if snap.LimitID == nil || *snap.LimitID != "codex" {
		t.Errorf("limitId = %v, want codex", snap.LimitID)
	}
	if snap.Credits == nil || snap.Credits.HasCredits {
		t.Errorf("credits = %+v, want hasCredits false", snap.Credits)
	}
	if snap.SpendControlReached == nil || *snap.SpendControlReached {
		t.Errorf("spendControlReached = %v, want false", snap.SpendControlReached)
	}
	if snap.RateLimitReachedType != nil {
		t.Errorf("rateLimitReachedType = %v, want nil", *snap.RateLimitReachedType)
	}
}

// TestAccountRateLimitsReadResponse_DecodesFullReply pins the parts of the
// reply Conn.AccountRateLimits does not surface: the multi-bucket view and
// the raw reset-credit block. Asserted at the schema layer so the fields
// stay wired while only the single bucket is exposed on Conn.
func TestAccountRateLimitsReadResponse_DecodesFullReply(t *testing.T) {
	var resp schema.AccountRateLimitsReadResponse
	if err := json.Unmarshal([]byte(liveRateLimitsResult), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	bucket, ok := resp.RateLimitsByLimitID["codex"]
	if !ok {
		t.Fatalf("rateLimitsByLimitId missing codex bucket: %+v", resp.RateLimitsByLimitID)
	}
	if bucket.Primary == nil || bucket.Primary.UsedPercent != 12 {
		t.Errorf("codex bucket primary = %+v, want usedPercent 12", bucket.Primary)
	}
	if len(resp.RateLimitResetCredits) == 0 {
		t.Error("rateLimitResetCredits decoded empty, want raw passthrough")
	}
}

// TestConnAccount_ChatGPT drives the `account/read` reply codex 0.148
// returns for a signed-in ChatGPT account.
func TestConnAccount_ChatGPT(t *testing.T) {
	gotParams := make(chan json.RawMessage, 1)
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountRead: func(id, params json.RawMessage) {
				gotParams <- params
				_ = fix.SendResponse(id, map[string]any{
					"account": map[string]any{
						"type": "chatgpt", "email": "dev@example.com", "planType": "plus",
					},
					"requiresOpenaiAuth": true,
				})
			},
		}
	})

	acct, err := conn.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}

	select {
	case p := <-gotParams:
		// refreshToken is omitempty and this call never refreshes, so the
		// params object must be empty rather than carrying a false flag.
		if s := string(p); s != "{}" {
			t.Errorf("params = %s, want {}", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no account/read frame")
	}

	if acct.Type != schema.AccountTypeChatGPT {
		t.Errorf("type = %q, want %q", acct.Type, schema.AccountTypeChatGPT)
	}
	if acct.Email == nil || *acct.Email != "dev@example.com" {
		t.Errorf("email = %v, want dev@example.com", acct.Email)
	}
	if acct.PlanType == nil || *acct.PlanType != "plus" {
		t.Errorf("planType = %v, want plus", acct.PlanType)
	}
	if acct.UsesCodexManagedCredentials != nil {
		t.Errorf("usesCodexManagedCredentials = %v, want nil", *acct.UsesCodexManagedCredentials)
	}
}

// TestConnAccount_APIKeyVariant confirms the union's slimmest variant
// decodes without inventing empty strings for the absent fields.
func TestConnAccount_APIKeyVariant(t *testing.T) {
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountRead: func(id, _ json.RawMessage) {
				_ = fix.SendResponse(id, map[string]any{
					"account":            map[string]any{"type": "apiKey"},
					"requiresOpenaiAuth": true,
				})
			},
		}
	})

	acct, err := conn.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if acct.Type != schema.AccountTypeAPIKey {
		t.Errorf("type = %q, want %q", acct.Type, schema.AccountTypeAPIKey)
	}
	if acct.Email != nil || acct.PlanType != nil {
		t.Errorf("email/planType = %v/%v, want nil/nil", acct.Email, acct.PlanType)
	}
}

// TestConnAccount_NotSignedIn asserts a null account is reported as
// ErrNotSignedIn rather than a nil-nil return the caller must remember to
// check.
func TestConnAccount_NotSignedIn(t *testing.T) {
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountRead: func(id, _ json.RawMessage) {
				_ = fix.SendResponse(id, map[string]any{
					"account": nil, "requiresOpenaiAuth": true,
				})
			},
		}
	})

	acct, err := conn.Account(context.Background())
	if acct != nil {
		t.Errorf("account = %+v, want nil", acct)
	}
	if !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("err = %v, want errors.Is(ErrNotSignedIn)", err)
	}
}

// TestAccountReads_UnknownMethod covers a server that does not implement
// the methods. codex rejects an unrecognised method while deserializing the
// ClientRequest union, so it answers -32600 "unknown variant" rather than
// the spec's -32601 — both must map to ErrMethodNotSupported so a caller
// can degrade to "unavailable".
func TestAccountReads_UnknownMethod(t *testing.T) {
	// Message text captured from codex 0.148, truncated at the method list.
	const codexUnknownVariant = "Invalid request: unknown variant `account/rateLimits/read`, expected one of `initialize`, `thread/start`"

	tests := []struct {
		name    string
		code    int
		message string
	}{
		{"codex unknown variant", rpcCodeInvalidRequest, codexUnknownVariant},
		{"jsonrpc method not found", rpcCodeMethodNotFound, "Method not found"},
		{"experimental gate", rpcCodeInvalidRequest, "account/read requires experimentalApi capability"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
				reject := func(id, _ json.RawMessage) {
					_ = fix.SendErrorResponse(id, tc.code, tc.message)
				}
				return accountHandlers{
					schema.MethodAccountRateLimitsRead: reject,
					schema.MethodAccountRead:           reject,
				}
			})

			snap, err := conn.AccountRateLimits(context.Background())
			if snap != nil {
				t.Errorf("snapshot = %+v, want nil", snap)
			}
			if !errors.Is(err, ErrMethodNotSupported) {
				t.Errorf("AccountRateLimits err = %v, want errors.Is(ErrMethodNotSupported)", err)
			}
			if _, err := conn.Account(context.Background()); !errors.Is(err, ErrMethodNotSupported) {
				t.Errorf("Account err = %v, want errors.Is(ErrMethodNotSupported)", err)
			}
		})
	}
}

// TestAccountReads_OtherRPCError guards the classification: codex reuses
// -32600 for malformed params, and that must stay a real failure rather
// than a "degrade to unavailable" signal.
func TestAccountReads_OtherRPCError(t *testing.T) {
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountRead: func(id, _ json.RawMessage) {
				_ = fix.SendErrorResponse(id, rpcCodeInvalidRequest, "Invalid request: missing field `refreshToken`")
			},
		}
	})

	_, err := conn.Account(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrMethodNotSupported) {
		t.Errorf("err = %v, want it NOT classified as ErrMethodNotSupported", err)
	}
	if errors.Is(err, ErrNotSignedIn) {
		t.Errorf("err = %v, want it NOT classified as ErrNotSignedIn", err)
	}
}

// TestAccountReads_Timeout asserts an unanswered request surfaces the
// context error instead of hanging, for both reads.
func TestAccountReads_Timeout(t *testing.T) {
	// No handlers: the fake server reads the frames and never replies.
	conn := connectForAccount(t, func(*BidiFixtureExecutor) accountHandlers {
		return accountHandlers{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := conn.AccountRateLimits(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AccountRateLimits err = %v, want errors.Is(context.DeadlineExceeded)", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if _, err := conn.Account(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Account err = %v, want errors.Is(context.DeadlineExceeded)", err)
	}
}

// TestAccountReads_ProcessExited confirms both reads report the typed exit
// error once the subprocess is gone, matching Interrupt's behaviour.
func TestAccountReads_ProcessExited(t *testing.T) {
	fix := NewBidiFixtureExecutor()
	client := NewWithExecutor(fix, WithEphemeralThread())

	go func() {
		id, _ := expectRequest(t, fix, "initialize")
		_ = fix.SendResponse(id, basicInitResponse())
		expectNotification(t, fix, "initialized")
		fix.FailFromServer(errors.New("simulated SIGKILL"))
	}()

	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for conn.ExitError() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := conn.AccountRateLimits(context.Background()); !errors.Is(err, ErrProcessExited) {
		t.Errorf("AccountRateLimits err = %v, want errors.Is(ErrProcessExited)", err)
	}
	if _, err := conn.Account(context.Background()); !errors.Is(err, ErrProcessExited) {
		t.Errorf("Account err = %v, want errors.Is(ErrProcessExited)", err)
	}
}
