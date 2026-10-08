//go:build integration

package codexcli

// Live checks of the ChatGPT sign-in RPCs against the codex on PATH. Every
// test runs in a throwaway CODEX_HOME, so the real ~/.codex/auth.json is
// never read or replaced; each asserts that it is byte-identical afterwards.
// They need network access to auth.openai.com but no account:
//
//	go test -tags integration -run TestLive_DeviceCodeLogin -count=1 -v .
//
// TestLive_DeviceCodeLoginInteractive needs a person: it prints a code and
// URL, and waits up to codex's 15-minute deadline for someone to sign in.
// It runs only with CODEXCLI_LIVE_LOGIN=1. CODEXCLI_LOGIN_HOME keeps the
// CODEX_HOME it signs into (default: a temp dir removed afterwards).

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// guardRealAuth fails the test if the real auth.json changes.
func guardRealAuth(t *testing.T) {
	t.Helper()
	real, err := resolveCodexHome("")
	if err != nil {
		return
	}
	path := filepath.Join(real, "auth.json")
	before, beforeErr := os.ReadFile(path)
	t.Cleanup(func() {
		after, afterErr := os.ReadFile(path)
		if (beforeErr == nil) != (afterErr == nil) || !bytes.Equal(before, after) {
			t.Errorf("real %s changed during the test", path)
		}
	})
}

func requireNoAuth(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err == nil {
		t.Errorf("auth.json written to %s without a sign-in", home)
	}
}

func checkDeviceCode(t *testing.T, l *Login) {
	t.Helper()
	if l.ID == "" || l.UserCode == "" {
		t.Fatalf("login = %+v, want an id and a code", l)
	}
	if !strings.HasPrefix(l.VerificationURL, "https://") {
		t.Errorf("verificationUrl = %q, want https", l.VerificationURL)
	}
	t.Logf("loginId=%s userCode=%s verificationUrl=%s expiresAt=%s",
		l.ID, l.UserCode, l.VerificationURL, l.ExpiresAt.Format(time.RFC3339))
}

// TestLive_DeviceCodeLoginStartAndCancel: a signed-out codex hands out a
// code and URL, Cancel stops it with ErrLoginCanceled, and nothing is
// stored.
func TestLive_DeviceCodeLoginStartAndCancel(t *testing.T) {
	guardRealAuth(t)
	home := t.TempDir()
	conn := liveMcpConn(t, home)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if _, err := conn.Account(ctx); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("Account in a fresh home = %v, want ErrNotSignedIn", err)
	}
	login, err := conn.StartDeviceCodeLogin(ctx)
	if err != nil {
		t.Fatalf("StartDeviceCodeLogin: %v", err)
	}
	checkDeviceCode(t, login)
	if login.Err() != nil {
		t.Fatalf("Err right after start = %v", login.Err())
	}

	if err := login.Cancel(ctx); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	err = login.Wait(ctx)
	if !errors.Is(err, ErrLoginCanceled) {
		t.Fatalf("Wait after Cancel = %v, want ErrLoginCanceled", err)
	}
	t.Logf("canceled: %v", err)
	if err := login.Cancel(ctx); !errors.Is(err, ErrLoginNotFound) {
		t.Errorf("second Cancel = %v, want ErrLoginNotFound", err)
	}
	if _, err := conn.Account(ctx); !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("Account after cancel = %v, want ErrNotSignedIn", err)
	}
	requireNoAuth(t, home)
}

// TestLive_DeviceCodeLoginReplaced: a second start on the same process
// ends the first with codex's "not completed" message, which is not
// reported as this handle's cancel.
func TestLive_DeviceCodeLoginReplaced(t *testing.T) {
	guardRealAuth(t)
	home := t.TempDir()
	conn := liveMcpConn(t, home)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	first, err := conn.StartDeviceCodeLogin(ctx)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := conn.StartDeviceCodeLogin(ctx)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if second.UserCode == first.UserCode {
		t.Errorf("both starts got code %s", first.UserCode)
	}
	err = first.Wait(ctx)
	var lerr *LoginError
	if !errors.As(err, &lerr) || errors.Is(err, ErrLoginCanceled) {
		t.Fatalf("first = %v, want a LoginError that is not ErrLoginCanceled", err)
	}
	t.Logf("replaced: %q", lerr.Message)
	if second.Err() != nil {
		t.Errorf("second ended: %v", second.Err())
	}
	if err := second.Cancel(ctx); err != nil {
		t.Errorf("Cancel second: %v", err)
	}
	requireNoAuth(t, home)
}

// TestLive_DeviceCodeLoginConnClosed: closing the Conn ends the handle and
// leaves nothing stored.
func TestLive_DeviceCodeLoginConnClosed(t *testing.T) {
	guardRealAuth(t)
	home := t.TempDir()
	conn := liveMcpConn(t, home)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	login, err := conn.StartDeviceCodeLogin(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = conn.Close()
	err = login.Wait(ctx)
	if err == nil || errors.Is(err, ErrLoginFailed) {
		t.Fatalf("Wait after Close = %v, want an exit/closed error", err)
	}
	t.Logf("closed: %v", err)
	requireNoAuth(t, home)
}

// TestLive_DeviceCodeLoginInteractive: a person completes the sign-in;
// account/read then reports the ChatGPT account on the signing process, on
// a process that was already running in the same CODEX_HOME, and on a
// fresh one.
func TestLive_DeviceCodeLoginInteractive(t *testing.T) {
	if os.Getenv("CODEXCLI_LIVE_LOGIN") != "1" {
		t.Skip("needs a person; set CODEXCLI_LIVE_LOGIN=1")
	}
	guardRealAuth(t)
	home := os.Getenv("CODEXCLI_LOGIN_HOME")
	if home == "" {
		home = t.TempDir()
	}
	conn := liveMcpConn(t, home)
	bystander := liveMcpConn(t, home)
	ctx := context.Background()
	if _, err := bystander.Account(ctx); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("bystander Account before = %v, want ErrNotSignedIn", err)
	}

	login, err := conn.StartDeviceCodeLogin(ctx)
	if err != nil {
		t.Fatalf("StartDeviceCodeLogin: %v", err)
	}
	checkDeviceCode(t, login)
	t.Logf("SIGN IN NOW: open %s and enter %s", login.VerificationURL, login.UserCode)

	wctx, cancel := context.WithDeadline(ctx, login.ExpiresAt.Add(time.Minute))
	defer cancel()
	start := time.Now()
	err = login.Wait(wctx)
	t.Logf("login ended after %s: err=%v (%T)", time.Since(start).Round(time.Second), err, err)
	if err != nil {
		t.Fatalf("sign-in failed: %v", err)
	}

	for name, c := range map[string]*Conn{"signing": conn, "bystander": bystander, "fresh": liveMcpConn(t, home)} {
		acct, err := c.Account(ctx)
		if err != nil {
			t.Errorf("%s Account = %v", name, err)
			continue
		}
		plan := ""
		if acct.PlanType != nil {
			plan = *acct.PlanType
		}
		t.Logf("%s Account: type=%s plan=%s email set=%t", name, acct.Type, plan, acct.Email != nil)
		if acct.Type != "chatgpt" {
			t.Errorf("%s type = %q, want chatgpt", name, acct.Type)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Errorf("no auth.json in %s after sign-in: %v", home, err)
	}
}
