//go:build integration

package codexcli

// Live checks that separate CODEX_HOMEs keep separate ChatGPT logins.
// TestLive_SeparateHomeLogins needs homes a person signed into first,
// one per account, never a copy of ~/.codex/auth.json (codex rotates the
// refresh token, so a copy that refreshes signs the original out):
//
//	CODEX_HOME=/path/a codex login --device-auth
//	CODEX_HOME=/path/b codex login --device-auth
//	CODEXCLI_LOGIN_HOMES=/path/a,/path/b go test -tags integration -run TestLive_SeparateHomeLogins -count=1 -v .
//
// Each home's config.toml picks the credential store under test with
// cli_auth_credentials_store. TestLive_CredentialStoreAvailable needs no
// account: it stores a fake API key to see which stores this machine
// supports.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/allbin/codexcli-go/schema"
)

// TestLive_SeparateHomeLogins forces a token refresh in each home in
// turn, twice round, and requires every home to stay signed in to its own
// account throughout. Run it more than once: each run refreshes with the
// tokens the previous run left behind.
func TestLive_SeparateHomeLogins(t *testing.T) {
	guardRealAuth(t)
	homes := strings.Split(os.Getenv("CODEXCLI_LOGIN_HOMES"), ",")
	if len(homes) < 2 || homes[0] == "" {
		t.Skip("CODEXCLI_LOGIN_HOMES not set to two or more signed-in homes")
	}
	accounts := map[string]string{}
	for round := 1; round <= 2; round++ {
		for _, home := range homes {
			acct := refreshHome(t, home, round)
			if prev, ok := accounts[home]; ok && prev != acct {
				t.Errorf("%s: account changed from %s to %s", home, prev, acct)
			}
			accounts[home] = acct
		}
	}
	for _, home := range homes {
		t.Logf("RESULT %s: %s, signed in after every refresh", filepath.Base(home), accounts[home])
	}
}

// refreshHome asks codex to refresh the home's tokens, reports whether
// the stored refresh token rotated, and checks the access token against
// the backend with a rate-limit read. It returns the account's email and
// plan.
func refreshHome(t *testing.T, home string, round int) string {
	t.Helper()
	store := credentialStore(home)
	before := refreshTokenOf(t, home)
	var stderr stderrLog
	client := New(WithEnv(map[string]string{"CODEX_HOME": home}), WithStderrCallback(stderr.add))
	conn, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("%s: Connect: %v", home, err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var resp schema.AccountReadResponse
	err = conn.rpc.Request(ctx, schema.MethodAccountRead, schema.AccountReadParams{RefreshToken: true}, &resp)
	if err != nil || resp.Account == nil {
		stderr.log(t)
		t.Fatalf("%s round %d: account/read refresh: account=%v err=%v", home, round, resp.Account, err)
	}
	acct := deref(resp.Account.Email) + " " + deref(resp.Account.PlanType)
	_, rlErr := conn.AccountRateLimits(ctx)
	after := refreshTokenOf(t, home)
	t.Logf("%s round %d: store=%s account=%s refresh-token rotated=%v rate-limits err=%v",
		filepath.Base(home), round, store, acct, before != "" && after != "" && before != after, rlErr)
	if rlErr != nil {
		stderr.log(t)
		t.Errorf("%s: rate-limit read after refresh: %v", home, rlErr)
	}
	for _, l := range stderr.lines {
		if strings.Contains(strings.ToLower(l), "refresh") {
			t.Logf("stderr: %s", l)
		}
	}
	return acct
}

func credentialStore(home string) string {
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return "default"
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == "cli_auth_credentials_store" {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return "default"
}

// refreshTokenOf returns the refresh token in a test home's auth.json, or
// "" when the home keeps none on disk. Only ever called on homes a person
// signed into for this test, and the token is compared, never logged.
func refreshTokenOf(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return ""
	}
	var auth struct {
		Tokens struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &auth) != nil {
		return ""
	}
	return auth.Tokens.RefreshToken
}

// TestLive_CredentialStoreAvailable stores a fake API key in a fresh home
// under each cli_auth_credentials_store value and logs whether it worked
// and where the credential went: auth.json, or elsewhere (the keyring).
func TestLive_CredentialStoreAvailable(t *testing.T) {
	guardRealAuth(t)
	for _, store := range []string{"file", "keyring", "auto"} {
		t.Run(store, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, filepath.Join(home, "config.toml"), "cli_auth_credentials_store = \""+store+"\"\n")
			var stderr stderrLog
			conn := liveMcpConn(t, home, WithStderrCallback(stderr.add))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			key := "sk-codexcli-test-" + randomToken(t)
			loginErr := conn.LoginWithAPIKey(ctx, key)
			_, acctErr := conn.Account(ctx)
			onDisk, _ := os.ReadFile(filepath.Join(home, "auth.json"))
			t.Logf("RESULT store=%s: login err=%v; account err=%v; key in auth.json=%v",
				store, loginErr, acctErr, bytes.Contains(onDisk, []byte(key)))
			if loginErr != nil {
				stderr.log(t)
			}
			if store == "file" && (loginErr != nil || errors.Is(acctErr, ErrNotSignedIn)) {
				t.Errorf("file store failed: %v / %v", loginErr, acctErr)
			}
			if loginErr == nil {
				_ = conn.Logout(ctx)
			}
		})
	}
}
