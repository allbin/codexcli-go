//go:build integration

package codexcli

// Live checks of the npm-global update proof against the codex on PATH:
//
//	go test -tags integration -run TestLive_NPMUpdate -count=1 -v .
//
// The proof test is read-only (it runs `npm prefix -g` and a write probe). The
// real update rewrites the machine's install, so it also needs
// CODEXCLI_LIVE_NPM_UPDATE=1.
//
// TestLive_NPMUpdateThrowawayPrefix runs a real update without touching the
// machine's install: it seeds an older @openai/codex into a temp prefix with
// the npm on PATH, and needs CODEXCLI_LIVE_NPM_SCRATCH=1 (it downloads it).
// Point CODEXCLI_LIVE_NPM_SCRATCH_DIR at a directory with no UUID in its path
// if TMPDIR has one; see the test.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	t.Logf("npm=%s pathDir=%s prefix=%s targets=%v", plan.npm, plan.pathDir, plan.prefix, plan.targets)
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

// TestLive_NPMUpdateThrowawayPrefix is the "system npm, user prefix" setup
// end to end: the npm on PATH, a prefix with no npm of its own chosen by
// npm_config_prefix, an older release seeded there, then Update. It checks the
// version moved, that the prefix npm would otherwise use and the node's own
// prefix are untouched, and that a client whose npm config points elsewhere
// is refused.
func TestLive_NPMUpdateThrowawayPrefix(t *testing.T) {
	if os.Getenv("CODEXCLI_LIVE_NPM_SCRATCH") != "1" {
		t.Skip("set CODEXCLI_LIVE_NPM_SCRATCH=1 to install @openai/codex into a temp prefix")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("no npm on PATH")
	}
	ctx := context.Background()
	npmOut := func(env []string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, npm, args...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("npm %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}

	latest := npmOut(nil, "view", CLIPackageName+"@latest", "version")
	var versions []string
	if err := json.Unmarshal([]byte(npmOut(nil, "view", CLIPackageName, "versions", "--json")), &versions); err != nil {
		t.Fatal(err)
	}
	var older string
	for _, v := range versions {
		if v == latest {
			break
		}
		if !strings.Contains(v, "-") {
			older = v
		}
	}
	if older == "" {
		t.Fatalf("no stable release before %s", latest)
	}

	// Every other prefix this npm could write: the one its own config names
	// (here that is the user's real install) and the node's built-in one.
	node, _ := exec.LookPath("node")
	realNode, _ := filepath.EvalSymlinks(node)
	others := []string{npmOut(nil, "prefix", "-g"), filepath.Dir(filepath.Dir(realNode))}
	before := snapshotCodexPackages(t, others)

	// npm prints a UUID-shaped path segment as "***", so a prefix under a
	// temp dir named after a session id is refused, correctly, as unproven.
	// CODEXCLI_LIVE_NPM_SCRATCH_DIR moves the scratch prefix somewhere else.
	root := t.TempDir()
	if base := os.Getenv("CODEXCLI_LIVE_NPM_SCRATCH_DIR"); base != "" {
		if root, err = os.MkdirTemp(base, "npm-scratch-"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	prefix := filepath.Join(root, "prefix")
	t.Logf("npm=%s seeding %s@%s into %s; latest is %s", npm, CLIPackageName, older, prefix, latest)
	npmOut([]string{"npm_config_prefix=" + prefix}, "install", "-g", "--no-fund", "--no-audit", CLIPackageName+"@"+older)

	path := filepath.Join(prefix, "bin") + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)
	client := New(WithEnv(map[string]string{"npm_config_prefix": prefix, "PATH": path}))

	info, err := client.DetectInstall(ctx)
	if err != nil {
		t.Fatalf("DetectInstall: %v", err)
	}
	if info.Path != filepath.Join(prefix, "bin", "codex") || info.Version != older || !info.SelfManaged {
		var manual *ManualUpdateError
		if _, err := client.Update(ctx); errors.As(err, &manual) {
			t.Logf("Update refuses: %s", manual.Reason)
		}
		t.Fatalf("detected %+v, want %s at %s, self-managed", info, older, prefix)
	}

	// A client whose npm config names another prefix is refused, whether that
	// prefix is empty or holds a different copy.
	other := filepath.Join(root, "other")
	for _, seedOther := range []bool{false, true} {
		if seedOther {
			pkg := filepath.Join(other, "lib", "node_modules", "@openai", "codex")
			if err := os.MkdirAll(pkg, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@openai/codex"}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		wrong := New(WithEnv(map[string]string{"npm_config_prefix": other, "PATH": path}))
		if info, err := wrong.DetectInstall(ctx); err != nil || info.SelfManaged {
			t.Errorf("other prefix (seeded %v): SelfManaged = %v, err %v; want false", seedOther, info != nil && info.SelfManaged, err)
		}
		_, err := wrong.Update(ctx)
		var manual *ManualUpdateError
		if !errors.As(err, &manual) {
			t.Fatalf("other prefix (seeded %v): err = %v, want *ManualUpdateError", seedOther, err)
		}
		t.Logf("other prefix (seeded %v) refused: %s", seedOther, manual.Reason)
	}

	result, err := client.Update(ctx, WithUpdateProgress(func(line string) { t.Log(line) }))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	t.Logf("%s → %s changed=%v updater=%s in %s", result.VersionBefore, result.VersionAfter, result.Changed, result.Updater, result.Duration)
	if !result.Changed || result.VersionBefore != older || result.VersionAfter != latest || result.Updater != npm {
		t.Errorf("result = %+v, want %s → %s by %s", result, older, latest, npm)
	}
	if got := packageVersion(t, filepath.Join(prefix, "lib", "node_modules", "@openai", "codex")); got != latest {
		t.Errorf("temp prefix package.json = %s, want %s", got, latest)
	}
	if after := snapshotCodexPackages(t, others); after != before {
		t.Errorf("another prefix changed:\nbefore %s\nafter  %s", before, after)
	}
	copies, err := filepath.Glob(filepath.Join(root, "*", "lib", "node_modules", "@openai", "codex"))
	if err != nil || len(copies) != 2 { // prefix, and the fake in other
		t.Errorf("copies under %s = %v, want the prefix's and the seeded fake only", root, copies)
	}
}

// snapshotCodexPackages records, per prefix, the CLI package's version and
// package.json mtime, so a second write anywhere shows as a difference.
func snapshotCodexPackages(t *testing.T, prefixes []string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range prefixes {
		pkg := filepath.Join(p, "lib", "node_modules", "@openai", "codex")
		fi, err := os.Stat(filepath.Join(pkg, "package.json"))
		if err != nil {
			b.WriteString(p + "=absent ")
			continue
		}
		b.WriteString(p + "=" + packageVersion(t, pkg) + "@" + fi.ModTime().String() + " ")
	}
	return b.String()
}

func packageVersion(t *testing.T, pkg string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pkg, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Version string }
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v.Version
}
