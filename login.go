package codexcli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// Sign-in deadlines codex enforces itself. Neither is reported on the wire:
// they are constants in codex's source (codex-rs/login/src/device_code_auth.rs
// polls for 15 minutes; app-server's account processor gives the browser
// flow LOGIN_CHATGPT_TIMEOUT, 10 minutes). Login.ExpiresAt is derived from
// them.
const (
	deviceCodeLoginLifetime = 15 * time.Minute
	browserLoginLifetime    = 10 * time.Minute
)

// abandonedStartTimeout bounds how long a start whose caller gave up keeps
// waiting for codex's reply, so it can cancel the attempt that reply names.
const abandonedStartTimeout = 2 * time.Minute

// Login is one ChatGPT sign-in running inside the app-server process.
//
// The attempt lives only as long as that process: the server polls OpenAI
// (device code) or serves the OAuth callback (browser) from a task inside
// it, and writes auth.json to its CODEX_HOME when the person finishes. Keep
// the Conn open until Done closes. Conn.Close, or the process dying for any
// other reason, abandons the attempt; Err then reports the exit.
//
// Every attempt ends exactly once, with one of:
//
//   - nil: signed in; the credentials are stored.
//   - a *LoginError (errors.Is ErrLoginFailed) carrying codex's message:
//     refused, expired (also ErrLoginTimedOut), canceled (also
//     ErrLoginCanceled when this handle's Cancel stopped it), or replaced by
//     a newer Start* on the same process, which codex reports with the same
//     message as a cancel.
//   - the process exit (errors.Is ErrProcessExited), or ErrClosed.
type Login struct {
	// ID is codex's loginId for this attempt.
	ID string
	// Type is LoginTypeChatGPTDeviceCode or LoginTypeChatGPT.
	Type schema.LoginType

	// UserCode and VerificationURL are set for a device-code sign-in: show
	// both, the person opens the URL on any device and enters the code.
	UserCode        string
	VerificationURL string

	// AuthURL is set for a browser sign-in.
	AuthURL string

	// ExpiresAt is when codex gives up on the attempt: 15 minutes after the
	// start for a device code, 10 for a browser sign-in, taken from codex's
	// source and the local clock when the start was sent, so it errs early.
	// The server does not report it. At the deadline Err becomes ErrLoginTimedOut.
	ExpiresAt time.Time

	conn *Conn
	done chan struct{}
	once sync.Once
	err  error

	// mu guards the cancel bookkeeping. codex reports a canceled attempt and
	// a replaced one with the same message, and may send it before or after
	// it answers account/login/cancel, so that outcome is held until every
	// Cancel in flight has its answer.
	mu       sync.Mutex
	cancels  int
	canceled bool // codex answered a Cancel with "canceled"
	held     *schema.AccountLoginCompletedNotification
}

// Done is closed when the attempt has ended; Err then reports how.
func (l *Login) Done() <-chan struct{} { return l.done }

// Err reports how the attempt ended, or nil while it is still running. See
// Login for the outcomes; nil after Done means signed in.
func (l *Login) Err() error {
	select {
	case <-l.done:
		return l.err
	default:
		return nil
	}
}

// Wait blocks until the attempt ends and returns Err. When ctx ends first it
// returns ctx.Err() and the attempt keeps running: ctx bounds the wait, not
// the sign-in. Call Cancel to stop it.
func (l *Login) Wait(ctx context.Context) error {
	select {
	case <-l.done:
		return l.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancel asks codex to stop the attempt. On success Err becomes a
// *LoginError matching ErrLoginCanceled, unless the sign-in finished first:
// the outcome is whatever Done reports. Returns ErrLoginNotFound when codex
// has no pending attempt with this ID: it already ended or was replaced.
// When the cancel request itself fails, the outcome is reported as a plain
// failure, since nothing confirms this Cancel stopped it.
func (l *Login) Cancel(ctx context.Context) error {
	if err := l.conn.checkExited(); err != nil {
		return err
	}
	l.mu.Lock()
	l.cancels++
	l.mu.Unlock()

	params := schema.CancelLoginAccountParams{LoginID: l.ID}
	var resp schema.CancelLoginAccountResponse
	err := l.conn.rpc.Request(ctx, schema.MethodAccountLoginCancel, params, &resp)

	l.mu.Lock()
	l.cancels--
	if err == nil && resp.Status == schema.CancelLoginCanceled {
		l.canceled = true
	}
	var held *schema.AccountLoginCompletedNotification
	if l.cancels == 0 {
		held, l.held = l.held, nil
	}
	canceled := l.canceled
	l.mu.Unlock()
	if held != nil {
		l.finish(loginOutcome(l.ID, *held, canceled))
	}

	if err != nil {
		return l.conn.promoteRPCError(schema.MethodAccountLoginCancel, err)
	}
	if resp.Status != schema.CancelLoginCanceled {
		return fmt.Errorf("%s %s: %w", schema.MethodAccountLoginCancel, l.ID, ErrLoginNotFound)
	}
	return nil
}

// deliver ends the attempt with codex's completion, or holds a "not
// completed" one while a Cancel is waiting to learn whether it was the
// cause.
func (l *Login) deliver(n schema.AccountLoginCompletedNotification) {
	l.mu.Lock()
	if l.cancels > 0 && !n.Success && loginMessage(n) == loginNotCompletedMessage {
		l.held = &n
		l.mu.Unlock()
		return
	}
	canceled := l.canceled
	l.mu.Unlock()
	l.finish(loginOutcome(l.ID, n, canceled))
}

// abandon ends the attempt because the process is gone, unless a held
// completion already says how it ended.
func (l *Login) abandon(err error) {
	l.mu.Lock()
	held, canceled := l.held, l.canceled
	l.held = nil
	l.mu.Unlock()
	if held != nil {
		l.finish(loginOutcome(l.ID, *held, canceled))
		return
	}
	l.finish(fmt.Errorf("login %s: %w", l.ID, err))
}

func (l *Login) finish(err error) {
	l.once.Do(func() {
		l.err = err
		close(l.done)
	})
}

// LoginError is a sign-in codex reported as failed on
// `account/login/completed`. Message is codex's text verbatim, fit to show
// the person.
type LoginError struct {
	LoginID string
	Message string
	// Canceled is true when the attempt ended after this handle's Cancel.
	Canceled bool
}

func (e *LoginError) Error() string {
	return fmt.Sprintf("codexcli: login %s failed: %s", e.LoginID, e.Message)
}

// Is matches ErrLoginFailed always, ErrLoginCanceled when Canceled, and
// ErrLoginTimedOut when codex's message is one of its timeouts.
func (e *LoginError) Is(target error) bool {
	switch target {
	case ErrLoginFailed:
		return true
	case ErrLoginCanceled:
		return e.Canceled
	case ErrLoginTimedOut:
		return isLoginTimeoutMessage(e.Message)
	}
	return false
}

// isLoginTimeoutMessage matches codex's two deadline messages:
// "device auth timed out after 15 minutes" (codex-rs/login device_code_auth.rs)
// and "Login timed out" (the app-server's browser-flow timeout).
func isLoginTimeoutMessage(msg string) bool {
	return strings.HasPrefix(msg, "device auth timed out") || msg == "Login timed out"
}

// loginRegistry pairs `account/login/completed` notifications with the
// Login handles waiting on them. A completion can be read before its handle
// registers: the read loop hands the start reply to the caller and keeps
// reading, so a fast outcome may be dispatched first. Completions read while
// any start is awaiting its reply are kept in early until the last of those
// starts registers; outside that window nobody could claim one, so it is
// dropped.
type loginRegistry struct {
	mu      sync.Mutex
	waiting map[string]*Login
	starts  int
	early   map[string]schema.AccountLoginCompletedNotification
}

func (r *loginRegistry) beginStart() {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
}

// endStart closes a beginStart. l is the started attempt, or nil when the
// start failed.
func (r *loginRegistry) endStart(l *Login) {
	r.mu.Lock()
	r.starts--
	var n schema.AccountLoginCompletedNotification
	var early bool
	if l != nil {
		if n, early = r.early[l.ID]; early {
			delete(r.early, l.ID)
		} else {
			if r.waiting == nil {
				r.waiting = map[string]*Login{}
			}
			r.waiting[l.ID] = l
		}
	}
	if r.starts == 0 {
		r.early = nil
	}
	r.mu.Unlock()
	if early {
		l.deliver(n)
	}
}

func (r *loginRegistry) unregister(id string) {
	r.mu.Lock()
	delete(r.waiting, id)
	r.mu.Unlock()
}

func (r *loginRegistry) complete(n schema.AccountLoginCompletedNotification) {
	if n.LoginID == nil {
		return
	}
	id := *n.LoginID
	r.mu.Lock()
	if l, ok := r.waiting[id]; ok {
		delete(r.waiting, id)
		r.mu.Unlock()
		l.deliver(n)
		return
	}
	if r.starts > 0 {
		if r.early == nil {
			r.early = map[string]schema.AccountLoginCompletedNotification{}
		}
		r.early[id] = n
	}
	r.mu.Unlock()
}

func loginMessage(n schema.AccountLoginCompletedNotification) string {
	if n.Error != nil && *n.Error != "" {
		return *n.Error
	}
	return "sign-in failed"
}

func loginOutcome(id string, n schema.AccountLoginCompletedNotification, canceled bool) error {
	if n.Success {
		return nil
	}
	msg := loginMessage(n)
	return &LoginError{LoginID: id, Message: msg, Canceled: canceled && msg == loginNotCompletedMessage}
}

// loginNotCompletedMessage is what codex reports for a sign-in it stopped
// before the end: canceled, replaced by a newer start, or dropped by a
// logout or API-key login. Only a Cancel on the handle tells them apart.
const loginNotCompletedMessage = "Login was not completed"

// StartDeviceCodeLogin begins a ChatGPT device-code sign-in via
// `account/login/start` {type: chatgptDeviceCode}. It returns once codex has
// a code from OpenAI; the person then opens VerificationURL on any device,
// signs in, and enters UserCode. Watch Done (or call Wait) for the outcome.
//
// This is the flow for a person who is not at the machine being signed in:
// nothing is pasted back, unlike the browser flow.
//
// A start replaces any sign-in still pending on the same process; the old
// handle ends with a *LoginError. The start itself fails with an RPC error
// carrying codex's message when OpenAI will not issue a code (e.g. -32600
// "device code login is not enabled for this Codex server. ...") or when
// config forces API-key login ("ChatGPT login is disabled. ...").
//
// If ctx ends before codex answers, StartDeviceCodeLogin returns ctx.Err()
// at once and cancels the attempt codex started, if any, when its reply
// arrives, so no attempt runs without a handle.
func (c *Conn) StartDeviceCodeLogin(ctx context.Context) (*Login, error) {
	return c.startLogin(ctx, schema.LoginAccountParams{Type: schema.LoginTypeChatGPTDeviceCode}, deviceCodeLoginLifetime)
}

// StartBrowserLogin begins the ChatGPT browser OAuth sign-in via
// `account/login/start` {type: chatgpt} and returns AuthURL.
//
// OpenAI redirects the finished sign-in to a callback server codex runs on
// localhost of the machine running the app-server, so it completes only in
// a browser on that machine. For a remote person use StartDeviceCodeLogin.
// Lifecycle and outcomes are as for StartDeviceCodeLogin.
func (c *Conn) StartBrowserLogin(ctx context.Context) (*Login, error) {
	return c.startLogin(ctx, schema.LoginAccountParams{Type: schema.LoginTypeChatGPT}, browserLoginLifetime)
}

func (c *Conn) startLogin(ctx context.Context, params schema.LoginAccountParams, lifetime time.Duration) (*Login, error) {
	if err := c.checkExited(); err != nil {
		return nil, err
	}
	sent := time.Now()
	c.logins.beginStart()
	type reply struct {
		resp schema.LoginAccountResponse
		err  error
	}
	replies := make(chan reply, 1)
	go func() {
		// Not bound to ctx: a start the caller gives up on still reads its
		// reply, so the attempt codex created can be canceled.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonedStartTimeout)
		defer cancel()
		var r reply
		r.err = c.rpc.Request(rctx, schema.MethodAccountLoginStart, params, &r.resp)
		replies <- r
	}()
	select {
	case r := <-replies:
		return c.registerLogin(r.resp, r.err, sent, lifetime)
	case <-ctx.Done():
		go func() {
			r := <-replies
			if l, err := c.registerLogin(r.resp, r.err, sent, lifetime); err == nil {
				cctx, cancel := context.WithTimeout(context.Background(), abandonedStartTimeout)
				defer cancel()
				_ = l.Cancel(cctx)
			}
		}()
		return nil, ctx.Err()
	}
}

// registerLogin turns a start reply into a tracked Login, closing the
// startLogin's beginStart either way.
func (c *Conn) registerLogin(resp schema.LoginAccountResponse, err error, sent time.Time, lifetime time.Duration) (*Login, error) {
	if err == nil && resp.LoginID == "" {
		err = fmt.Errorf("reply of type %q has no loginId", resp.Type)
	}
	if err != nil {
		c.logins.endStart(nil)
		if isMethodNotSupportedError(err) {
			return nil, fmt.Errorf("%s: %w", schema.MethodAccountLoginStart, ErrMethodNotSupported)
		}
		return nil, c.promoteRPCError(schema.MethodAccountLoginStart, err)
	}
	l := &Login{
		ID:              resp.LoginID,
		Type:            resp.Type,
		UserCode:        resp.UserCode,
		VerificationURL: resp.VerificationURL,
		AuthURL:         resp.AuthURL,
		ExpiresAt:       sent.Add(lifetime),
		conn:            c,
		done:            make(chan struct{}),
	}
	c.logins.endStart(l)
	go func() {
		select {
		case <-l.done:
		case <-c.waitDone:
			// The read loop has exited, so a completion it read was
			// already delivered and abandon only reports the exit.
			c.logins.unregister(l.ID)
			err := error(ErrClosed)
			if ex := c.exitErr.Load(); ex != nil {
				err = ex
			}
			l.abandon(err)
		}
	}()
	return l, nil
}

// LoginWithAPIKey stores an OpenAI API key as codex's credentials via
// `account/login/start` {type: apiKey}. Unlike the ChatGPT flows it is
// synchronous: the key is saved when it returns nil. It does not check the
// key against OpenAI. It cancels any pending ChatGPT sign-in.
func (c *Conn) LoginWithAPIKey(ctx context.Context, apiKey string) error {
	if err := c.checkExited(); err != nil {
		return err
	}
	params := schema.LoginAccountParams{Type: schema.LoginTypeAPIKey, APIKey: apiKey}
	var resp schema.LoginAccountResponse
	if err := c.rpc.Request(ctx, schema.MethodAccountLoginStart, params, &resp); err != nil {
		if isMethodNotSupportedError(err) {
			return fmt.Errorf("%s: %w", schema.MethodAccountLoginStart, ErrMethodNotSupported)
		}
		return c.promoteRPCError(schema.MethodAccountLoginStart, err)
	}
	return nil
}

// Logout removes codex's stored credentials via `account/logout` (revoking
// ChatGPT tokens with OpenAI) and cancels any pending sign-in, whose handle
// ends with a *LoginError.
func (c *Conn) Logout(ctx context.Context) error {
	if err := c.checkExited(); err != nil {
		return err
	}
	var resp schema.LogoutAccountResponse
	if err := c.rpc.Request(ctx, schema.MethodAccountLogout, nil, &resp); err != nil {
		if isMethodNotSupportedError(err) {
			return fmt.Errorf("%s: %w", schema.MethodAccountLogout, ErrMethodNotSupported)
		}
		return c.promoteRPCError(schema.MethodAccountLogout, err)
	}
	return nil
}
