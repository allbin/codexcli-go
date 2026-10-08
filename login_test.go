package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// Wire values captured from codex 0.160.1 (`account/login/start`
// {type: chatgptDeviceCode} against a fresh CODEX_HOME).
const (
	testLoginID         = "287dc8a1-ee87-4893-9ba5-4bb580c06049"
	testUserCode        = "L0M7-L9L9J"
	testVerificationURL = "https://auth.openai.com/codex/device"
)

func deviceStartReply(fix *BidiFixtureExecutor, id json.RawMessage) {
	_ = fix.SendResponse(id, map[string]any{
		"type": "chatgptDeviceCode", "loginId": testLoginID,
		"verificationUrl": testVerificationURL, "userCode": testUserCode,
	})
}

func loginCompleted(fix *BidiFixtureExecutor, loginID string, success bool, msg string) {
	p := map[string]any{"loginId": loginID, "success": success, "error": nil}
	if msg != "" {
		p["error"] = msg
	}
	_ = fix.SendNotification(schema.MethodAccountLoginCompleted, p)
}

// connectWithDeviceLogin connects and starts a device-code login. Each
// further handler is added to the scripted server; the start handler
// replies with the captured device-code reply, then runs after (if set).
func connectWithDeviceLogin(t *testing.T, after func(fix *BidiFixtureExecutor), extra func(fix *BidiFixtureExecutor) accountHandlers) (*Conn, *Login, json.RawMessage) {
	t.Helper()
	gotParams := make(chan json.RawMessage, 1)
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		h := accountHandlers{}
		if extra != nil {
			h = extra(fix)
		}
		h[schema.MethodAccountLoginStart] = func(id, params json.RawMessage) {
			gotParams <- params
			deviceStartReply(fix, id)
			if after != nil {
				after(fix)
			}
		}
		return h
	})
	login, err := conn.StartDeviceCodeLogin(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceCodeLogin: %v", err)
	}
	return conn, login, <-gotParams
}

func waitLogin(t *testing.T, l *Login) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := l.Wait(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("login did not end")
	}
	return err
}

// TestStartDeviceCodeLogin_Success: the start frame carries only the type,
// the reply's code and URL reach the handle, and a successful
// account/login/completed ends it with nil.
func TestStartDeviceCodeLogin_Success(t *testing.T) {
	var fixRef *BidiFixtureExecutor
	_, login, params := connectWithDeviceLogin(t, nil, func(fix *BidiFixtureExecutor) accountHandlers {
		fixRef = fix
		return accountHandlers{}
	})
	if s := string(params); s != `{"type":"chatgptDeviceCode"}` {
		t.Errorf("params = %s, want only the type", s)
	}
	if login.ID != testLoginID || login.UserCode != testUserCode || login.VerificationURL != testVerificationURL {
		t.Errorf("login = %+v", login)
	}
	if login.Type != schema.LoginTypeChatGPTDeviceCode {
		t.Errorf("type = %q", login.Type)
	}
	if left := time.Until(login.ExpiresAt); left < 14*time.Minute || left > 15*time.Minute {
		t.Errorf("ExpiresAt in %v, want ~15m", left)
	}
	if login.Err() != nil {
		t.Errorf("Err before completion = %v, want nil", login.Err())
	}
	select {
	case <-login.Done():
		t.Fatal("Done closed before completion")
	default:
	}

	// A completion for another sign-in must not end this one.
	loginCompleted(fixRef, "someone-else", false, "Login was not completed")
	time.Sleep(50 * time.Millisecond)
	select {
	case <-login.Done():
		t.Fatal("another login's completion ended this one")
	default:
	}

	loginCompleted(fixRef, testLoginID, true, "")
	if err := waitLogin(t, login); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if login.Err() != nil {
		t.Errorf("Err after success = %v", login.Err())
	}
}

// TestStartDeviceCodeLogin_Failed: a failed completion is a *LoginError with
// codex's message, matching ErrLoginFailed only.
func TestStartDeviceCodeLogin_Failed(t *testing.T) {
	const msg = "device auth failed with status 400 Bad Request"
	_, login, _ := connectWithDeviceLogin(t, func(fix *BidiFixtureExecutor) {
		loginCompleted(fix, testLoginID, false, msg)
	}, nil)
	err := waitLogin(t, login)
	var lerr *LoginError
	if !errors.As(err, &lerr) || lerr.Message != msg || lerr.LoginID != testLoginID {
		t.Fatalf("err = %#v, want LoginError %q", err, msg)
	}
	if !errors.Is(err, ErrLoginFailed) {
		t.Error("not ErrLoginFailed")
	}
	if errors.Is(err, ErrLoginCanceled) || errors.Is(err, ErrLoginTimedOut) {
		t.Errorf("err %v wrongly canceled/timed out", err)
	}
}

// TestStartDeviceCodeLogin_TimedOut: codex's deadline messages map to
// ErrLoginTimedOut.
func TestStartDeviceCodeLogin_TimedOut(t *testing.T) {
	for _, msg := range []string{"device auth timed out after 15 minutes", "Login timed out"} {
		t.Run(msg, func(t *testing.T) {
			_, login, _ := connectWithDeviceLogin(t, func(fix *BidiFixtureExecutor) {
				loginCompleted(fix, testLoginID, false, msg)
			}, nil)
			err := waitLogin(t, login)
			if !errors.Is(err, ErrLoginTimedOut) || !errors.Is(err, ErrLoginFailed) {
				t.Fatalf("err = %v, want ErrLoginTimedOut and ErrLoginFailed", err)
			}
		})
	}
}

// TestLoginCancel: Cancel sends the loginId; codex answers canceled and then
// reports the attempt as not completed, which the handle reports as
// ErrLoginCanceled.
func TestLoginCancel(t *testing.T) {
	gotCancel := make(chan json.RawMessage, 1)
	_, login, _ := connectWithDeviceLogin(t, nil, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountLoginCancel: func(id, params json.RawMessage) {
				gotCancel <- params
				_ = fix.SendResponse(id, map[string]any{"status": "canceled"})
				loginCompleted(fix, testLoginID, false, "Login was not completed")
			},
		}
	})
	if err := login.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if s := string(<-gotCancel); s != `{"loginId":"`+testLoginID+`"}` {
		t.Errorf("cancel params = %s", s)
	}
	err := waitLogin(t, login)
	if !errors.Is(err, ErrLoginCanceled) || !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("err = %v, want ErrLoginCanceled", err)
	}
}

// TestLoginCancel_NotFound: an attempt codex no longer has is
// ErrLoginNotFound, and a "not completed" outcome nobody here canceled
// (a newer start replaced it) is not ErrLoginCanceled.
func TestLoginCancel_NotFound(t *testing.T) {
	_, login, _ := connectWithDeviceLogin(t, func(fix *BidiFixtureExecutor) {
		loginCompleted(fix, testLoginID, false, "Login was not completed")
	}, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountLoginCancel: func(id, _ json.RawMessage) {
				_ = fix.SendResponse(id, map[string]any{"status": "notFound"})
			},
		}
	})
	err := waitLogin(t, login)
	if !errors.Is(err, ErrLoginFailed) || errors.Is(err, ErrLoginCanceled) {
		t.Fatalf("err = %v, want ErrLoginFailed, not ErrLoginCanceled", err)
	}
	if err := login.Cancel(context.Background()); !errors.Is(err, ErrLoginNotFound) {
		t.Errorf("Cancel = %v, want ErrLoginNotFound", err)
	}
	// The outcome is fixed once Done closed; the late Cancel does not
	// rewrite it.
	if errors.Is(login.Err(), ErrLoginCanceled) {
		t.Error("late Cancel rewrote the outcome")
	}
}

// TestLoginCancel_NotFoundBeforeOutcome: codex sends a replaced attempt's
// outcome asynchronously, so a Cancel can get notFound before it arrives.
// That outcome is the replacement's doing, not this Cancel's.
func TestLoginCancel_NotFoundBeforeOutcome(t *testing.T) {
	_, login, _ := connectWithDeviceLogin(t, nil, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountLoginCancel: func(id, _ json.RawMessage) {
				_ = fix.SendResponse(id, map[string]any{"status": "notFound"})
				go func() {
					time.Sleep(50 * time.Millisecond)
					loginCompleted(fix, testLoginID, false, "Login was not completed")
				}()
			},
		}
	})
	if err := login.Cancel(context.Background()); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("Cancel = %v, want ErrLoginNotFound", err)
	}
	err := waitLogin(t, login)
	if !errors.Is(err, ErrLoginFailed) || errors.Is(err, ErrLoginCanceled) {
		t.Fatalf("err = %v, want ErrLoginFailed, not ErrLoginCanceled", err)
	}
}

// TestLoginWait_ContextEnds: ctx bounds the wait, not the sign-in.
func TestLoginWait_ContextEnds(t *testing.T) {
	var fixRef *BidiFixtureExecutor
	_, login, _ := connectWithDeviceLogin(t, nil, func(fix *BidiFixtureExecutor) accountHandlers {
		fixRef = fix
		return accountHandlers{}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := login.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want DeadlineExceeded", err)
	}
	loginCompleted(fixRef, testLoginID, true, "")
	if err := waitLogin(t, login); err != nil {
		t.Fatalf("Wait after ctx expiry = %v, want the later success", err)
	}
}

// TestLogin_ProcessExits: the attempt lives in the app-server process, so
// its death ends the handle with the exit error.
func TestLogin_ProcessExits(t *testing.T) {
	_, login, _ := connectWithDeviceLogin(t, func(fix *BidiFixtureExecutor) {
		go func() {
			time.Sleep(50 * time.Millisecond)
			fix.FailFromServer(errors.New("simulated SIGKILL"))
		}()
	}, nil)
	err := waitLogin(t, login)
	if !errors.Is(err, ErrProcessExited) {
		t.Fatalf("err = %v, want ErrProcessExited", err)
	}
}

// TestLogin_ConnClosed: closing the Conn abandons the attempt.
func TestLogin_ConnClosed(t *testing.T) {
	conn, login, _ := connectWithDeviceLogin(t, nil, nil)
	_ = conn.Close()
	if err := waitLogin(t, login); err == nil || errors.Is(err, ErrLoginFailed) {
		t.Fatalf("err = %v, want a connection/exit error", err)
	}
}

// TestLoginRegistry_CompletionBeforeRegister: the read loop can dispatch a
// completion before the start caller registers its handle; it must still
// reach it, however many other completions arrive meanwhile. Completions
// read while no start is in flight are dropped.
func TestLoginRegistry_CompletionBeforeRegister(t *testing.T) {
	var r loginRegistry
	id := testLoginID
	msg := "boom"
	r.beginStart()
	r.complete(schema.AccountLoginCompletedNotification{LoginID: &id, Error: &msg})
	for i := 0; i < 100; i++ {
		other := string(rune('a' + i))
		r.complete(schema.AccountLoginCompletedNotification{LoginID: &other, Success: true})
	}
	l := &Login{ID: id, done: make(chan struct{})}
	r.endStart(l)
	var lerr *LoginError
	if !errors.As(l.Err(), &lerr) || lerr.Message != "boom" {
		t.Fatalf("err = %v, want the early completion", l.Err())
	}
	if r.early != nil || len(r.waiting) != 0 {
		t.Errorf("early = %d, waiting = %d after the last start, want both empty", len(r.early), len(r.waiting))
	}

	stray := "stray"
	r.complete(schema.AccountLoginCompletedNotification{LoginID: &stray, Success: true})
	if len(r.early) != 0 {
		t.Errorf("kept a completion with no start in flight")
	}
}

// TestLoginCancel_OutcomeBeforeAnswer: codex may send "not completed"
// before it answers the cancel. The outcome is classified by the answer:
// canceled is this Cancel's doing, notFound is not.
func TestLoginCancel_OutcomeBeforeAnswer(t *testing.T) {
	for _, status := range []string{"canceled", "notFound"} {
		t.Run(status, func(t *testing.T) {
			_, login, _ := connectWithDeviceLogin(t, nil, func(fix *BidiFixtureExecutor) accountHandlers {
				return accountHandlers{
					schema.MethodAccountLoginCancel: func(id, _ json.RawMessage) {
						loginCompleted(fix, testLoginID, false, "Login was not completed")
						time.Sleep(50 * time.Millisecond)
						_ = fix.SendResponse(id, map[string]any{"status": status})
					},
				}
			})
			cerr := login.Cancel(context.Background())
			err := waitLogin(t, login)
			if !errors.Is(err, ErrLoginFailed) {
				t.Fatalf("err = %v, want ErrLoginFailed", err)
			}
			if status == "canceled" {
				if cerr != nil || !errors.Is(err, ErrLoginCanceled) {
					t.Errorf("Cancel = %v, err = %v; want nil and ErrLoginCanceled", cerr, err)
				}
			} else if !errors.Is(cerr, ErrLoginNotFound) || errors.Is(err, ErrLoginCanceled) {
				t.Errorf("Cancel = %v, err = %v; want ErrLoginNotFound and not ErrLoginCanceled", cerr, err)
			}
		})
	}
}

// TestStartDeviceCodeLogin_AbandonedStart: a start whose ctx ends before
// codex answers returns at once, and the attempt codex then reports is
// canceled rather than left running with no handle.
func TestStartDeviceCodeLogin_AbandonedStart(t *testing.T) {
	gotCancel := make(chan json.RawMessage, 1)
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountLoginStart: func(id, _ json.RawMessage) {
				go func() {
					time.Sleep(200 * time.Millisecond)
					deviceStartReply(fix, id)
				}()
			},
			schema.MethodAccountLoginCancel: func(id, params json.RawMessage) {
				gotCancel <- params
				_ = fix.SendResponse(id, map[string]any{"status": "canceled"})
			},
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if l, err := conn.StartDeviceCodeLogin(ctx); l != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start = %v, %v; want nil, DeadlineExceeded", l, err)
	}
	select {
	case p := <-gotCancel:
		if s := string(p); s != `{"loginId":"`+testLoginID+`"}` {
			t.Errorf("cancel params = %s", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abandoned attempt was not canceled")
	}
}

// TestStartDeviceCodeLogin_Refused: codex's refusal to issue a code reaches
// the caller with its message and is not mistaken for a missing method.
func TestStartDeviceCodeLogin_Refused(t *testing.T) {
	const msg = "device code login is not enabled for this Codex server. Use the browser login or verify the server URL."
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountLoginStart: func(id, _ json.RawMessage) {
				_ = fix.SendErrorResponse(id, rpcCodeInvalidRequest, msg)
			},
		}
	})
	login, err := conn.StartDeviceCodeLogin(context.Background())
	if login != nil || err == nil {
		t.Fatalf("login, err = %v, %v; want nil and an error", login, err)
	}
	if errors.Is(err, ErrMethodNotSupported) {
		t.Errorf("err = %v classified as ErrMethodNotSupported", err)
	}
	var rerr *rpcError
	if !errors.As(err, &rerr) || rerr.Message != msg {
		t.Errorf("err = %v, want codex's message", err)
	}
}

// TestLogin_UnknownMethod: an app-server without the login methods reports
// ErrMethodNotSupported from every entry point.
func TestLogin_UnknownMethod(t *testing.T) {
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		reject := func(id, _ json.RawMessage) {
			_ = fix.SendErrorResponse(id, rpcCodeInvalidRequest, "Invalid request: unknown variant `account/login/start`, expected one of `initialize`")
		}
		return accountHandlers{
			schema.MethodAccountLoginStart: reject,
			schema.MethodAccountLogout:     reject,
		}
	})
	ctx := context.Background()
	if _, err := conn.StartDeviceCodeLogin(ctx); !errors.Is(err, ErrMethodNotSupported) {
		t.Errorf("StartDeviceCodeLogin = %v", err)
	}
	if _, err := conn.StartBrowserLogin(ctx); !errors.Is(err, ErrMethodNotSupported) {
		t.Errorf("StartBrowserLogin = %v", err)
	}
	if err := conn.LoginWithAPIKey(ctx, "sk-test"); !errors.Is(err, ErrMethodNotSupported) {
		t.Errorf("LoginWithAPIKey = %v", err)
	}
	if err := conn.Logout(ctx); !errors.Is(err, ErrMethodNotSupported) {
		t.Errorf("Logout = %v", err)
	}
}

// TestStartBrowserLogin: the chatgpt variant returns the auth URL and a
// 10-minute deadline.
func TestStartBrowserLogin(t *testing.T) {
	gotParams := make(chan json.RawMessage, 1)
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountLoginStart: func(id, params json.RawMessage) {
				gotParams <- params
				_ = fix.SendResponse(id, map[string]any{
					"type": "chatgpt", "loginId": testLoginID, "authUrl": "https://auth.openai.com/oauth/authorize?x=1",
				})
			},
		}
	})
	login, err := conn.StartBrowserLogin(context.Background())
	if err != nil {
		t.Fatalf("StartBrowserLogin: %v", err)
	}
	if s := string(<-gotParams); s != `{"type":"chatgpt"}` {
		t.Errorf("params = %s", s)
	}
	if login.AuthURL == "" || login.UserCode != "" || login.Type != schema.LoginTypeChatGPT {
		t.Errorf("login = %+v", login)
	}
	if left := time.Until(login.ExpiresAt); left < 9*time.Minute || left > 10*time.Minute {
		t.Errorf("ExpiresAt in %v, want ~10m", left)
	}
}

// TestLoginWithAPIKeyAndLogout pins both frames: the key travels in the
// apiKey variant, and logout sends no params (codex declares them null).
func TestLoginWithAPIKeyAndLogout(t *testing.T) {
	gotLogin := make(chan json.RawMessage, 1)
	gotLogout := make(chan json.RawMessage, 1)
	conn := connectForAccount(t, func(fix *BidiFixtureExecutor) accountHandlers {
		return accountHandlers{
			schema.MethodAccountLoginStart: func(id, params json.RawMessage) {
				gotLogin <- params
				_ = fix.SendResponse(id, map[string]any{"type": "apiKey"})
			},
			schema.MethodAccountLogout: func(id, params json.RawMessage) {
				gotLogout <- params
				_ = fix.SendResponse(id, map[string]any{})
			},
		}
	})
	if err := conn.LoginWithAPIKey(context.Background(), "sk-test"); err != nil {
		t.Fatalf("LoginWithAPIKey: %v", err)
	}
	if s := string(<-gotLogin); s != `{"type":"apiKey","apiKey":"sk-test"}` {
		t.Errorf("login params = %s", s)
	}
	if err := conn.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if p := <-gotLogout; len(p) != 0 {
		t.Errorf("logout params = %s, want omitted", string(p))
	}
}

// TestAccountNotifications_Events: both account notifications reach a
// thread subscriber as typed events rather than UnknownEvent.
func TestAccountNotifications_Events(t *testing.T) {
	var c Conn
	sub := c.subscribe("t1")
	defer c.unsubscribe("t1", sub)
	c.dispatchNotification(schema.MethodAccountLoginCompleted, json.RawMessage(`{"loginId":"L","success":false,"error":"nope"}`))
	c.dispatchNotification(schema.MethodAccountUpdated, json.RawMessage(`{"authMode":"chatgpt","planType":"plus"}`))

	want := []Event{
		&AccountLoginCompletedEvent{LoginID: "L", Success: false, Error: "nope"},
		&AccountUpdatedEvent{AuthMode: "chatgpt", PlanType: "plus"},
	}
	for i, w := range want {
		select {
		case got := <-sub.out:
			switch g := got.(type) {
			case *AccountLoginCompletedEvent:
				if *g != *w.(*AccountLoginCompletedEvent) {
					t.Errorf("event %d = %+v, want %+v", i, g, w)
				}
			case *AccountUpdatedEvent:
				if *g != *w.(*AccountUpdatedEvent) {
					t.Errorf("event %d = %+v, want %+v", i, g, w)
				}
			default:
				t.Errorf("event %d = %T, want %T", i, got, w)
			}
		case <-time.After(time.Second):
			t.Fatalf("event %d not delivered", i)
		}
	}
}
