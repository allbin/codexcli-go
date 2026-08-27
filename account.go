package codexcli

import (
	"context"
	"fmt"

	"github.com/allbin/codexcli-go/schema"
)

// AccountRateLimits reads the current rate-limit snapshot from the
// app-server via the `account/rateLimits/read` RPC.
//
// Unlike RateLimitsUpdatedEvent — which the server only pushes while a turn
// is running — this is a pull, so a usage indicator can render on a
// connection that has never started a thread. The returned snapshot is the
// same schema.RateLimitSnapshot the notification carries, so a consumer can
// seed from this call and keep refreshing from the event stream.
//
// It costs a round trip to OpenAI's backend on the server side, so treat it
// as a poll (on connect, then on an interval) rather than something to call
// per render.
//
// Returns ErrMethodNotSupported when the app-server does not implement the
// method, so callers can degrade to "usage unavailable" instead of
// reporting a failure.
func (c *Conn) AccountRateLimits(ctx context.Context) (*schema.RateLimitSnapshot, error) {
	if err := c.checkExited(); err != nil {
		return nil, err
	}
	// The method takes no params; codex declares them `undefined`.
	var resp schema.AccountRateLimitsReadResponse
	if err := c.rpc.Request(ctx, schema.MethodAccountRateLimitsRead, nil, &resp); err != nil {
		if isMethodNotSupportedError(err) {
			return nil, fmt.Errorf("%s: %w", schema.MethodAccountRateLimitsRead, ErrMethodNotSupported)
		}
		return nil, c.promoteRPCError(schema.MethodAccountRateLimitsRead, err)
	}
	snapshot := resp.RateLimits
	return &snapshot, nil
}

// Account reads the signed-in account via the `account/read` RPC.
//
// It does not ask the server to refresh credentials — the call is a plain
// read, safe to make at any point in a connection's life.
//
// Returns ErrNotSignedIn when the server reports no account, and
// ErrMethodNotSupported when the app-server does not implement the method.
// Both are "there is nothing to show" rather than "the call failed"; every
// other error is a real failure.
func (c *Conn) Account(ctx context.Context) (*schema.Account, error) {
	if err := c.checkExited(); err != nil {
		return nil, err
	}
	var resp schema.AccountReadResponse
	params := schema.AccountReadParams{}
	if err := c.rpc.Request(ctx, schema.MethodAccountRead, params, &resp); err != nil {
		if isMethodNotSupportedError(err) {
			return nil, fmt.Errorf("%s: %w", schema.MethodAccountRead, ErrMethodNotSupported)
		}
		return nil, c.promoteRPCError(schema.MethodAccountRead, err)
	}
	if resp.Account == nil {
		return nil, fmt.Errorf("%s: %w", schema.MethodAccountRead, ErrNotSignedIn)
	}
	return resp.Account, nil
}
