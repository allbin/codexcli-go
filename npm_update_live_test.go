//go:build integration

package codexcli

// Live checks of the npm-global update proof against the codex on PATH:
//
//	go test -tags integration -run TestLive_NPMUpdate -count=1 -v .
//
// The proof test is read-only (it runs `npm prefix -g` and a write probe). The
// real update rewrites the machine's install, so it also needs
// CODEXCLI_LIVE_NPM_UPDATE=1.

import (
	"context"
	"errors"
	"os"
	"testing"
)

func liveNPMInstall(t *testing.T) *InstallInfo {
	t.Helper()
	info, err := New().DetectInstall(context.Background())
	if errors.Is(err, ErrCLINotFound) {
		t.Skip("no codex on PATH")
	}
	if err != nil {
		t.Fatalf("DetectInstall: %v", err)
	}
	if info.Method != InstallNPMGlobal || info.PackageManager != "npm" {
		t.Skipf("codex on PATH is %s/%s, not npm-global", info.Method, info.PackageManager)
	}
	return info
}

// TestLive_NPMUpdateProof: detection and the update's own proof agree, and on
// a single-prefix npm install (the fnm reference machine) both accept.
func TestLive_NPMUpdateProof(t *testing.T) {
	info := liveNPMInstall(t)
	env := osInstallEnv("")
	plan, err := proveNPMUpdate(context.Background(), info, env)
	t.Logf("path=%s real=%s version=%s selfManaged=%v", info.Path, info.RealPath, info.Version, info.SelfManaged)
	if err != nil {
		t.Logf("proof refused: %v", err)
		if info.SelfManaged {
			t.Error("SelfManaged = true although the proof refused")
		}
		return
	}
	t.Logf("npm=%s prefix=%s targets=%v", plan.npm, plan.prefix, plan.targets)
	writable := true
	for _, dir := range plan.targets {
		if err := checkWritable(dir); err != nil {
			t.Logf("%s not writable: %v", dir, err)
			writable = false
		}
	}
	if info.SelfManaged != writable {
		t.Errorf("SelfManaged = %v, want %v (proof held, writable %v)", info.SelfManaged, writable, writable)
	}
}

// TestLive_NPMUpdateRuns performs the real update. Opt-in only.
func TestLive_NPMUpdateRuns(t *testing.T) {
	if os.Getenv("CODEXCLI_LIVE_NPM_UPDATE") != "1" {
		t.Skip("set CODEXCLI_LIVE_NPM_UPDATE=1 to update the codex install on this machine")
	}
	if info := liveNPMInstall(t); !info.SelfManaged {
		t.Skip("install is not self-managed here")
	}
	result, err := New().Update(context.Background(), WithUpdateProgress(func(line string) { t.Log(line) }))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	t.Logf("%s → %s changed=%v updater=%s in %s", result.VersionBefore, result.VersionAfter, result.Changed, result.Updater, result.Duration)
}
