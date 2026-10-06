//go:build integration && !windows

package codexcli

// Live checks that cancelling Update partway through leaves a working codex,
// against throwaway installs only:
//
//	CODEXCLI_LIVE_NPM_SCRATCH=1 CODEXCLI_LIVE_NPM_SCRATCH_DIR=/some/dir \
//	  go test -tags integration -run TestLive_NPMUpdateCancel -count=1 -v .
//	CODEXCLI_LIVE_STANDALONE_SCRATCH=1 \
//	  go test -tags integration -run TestLive_StandaloneUpdateCancel -count=1 -v .
//
// CODEXCLI_LIVE_CANCEL_AT lists the delays at which to cancel, counted from
// the moment the updater starts (npm) or starts downloading (standalone);
// CODEXCLI_LIVE_CANCEL_RUNS is how many runs each. Each run starts from a
// fresh install of the release before latest.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLive_NPMUpdateCancel(t *testing.T) {
	if os.Getenv("CODEXCLI_LIVE_NPM_SCRATCH") != "1" {
		t.Skip("set CODEXCLI_LIVE_NPM_SCRATCH=1 to install @openai/codex into temp prefixes")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("no npm on PATH")
	}
	older, latest := liveOlderLatest(t, npm)

	// The prefixes this npm writes when not pointed at a throwaway one: they
	// must not move.
	node, _ := exec.LookPath("node")
	realNode, _ := filepath.EvalSymlinks(node)
	cfgPrefix, _ := exec.Command(npm, "prefix", "-g").Output()
	others := []string{strings.TrimSpace(string(cfgPrefix)), filepath.Dir(filepath.Dir(realNode))}
	before := snapshotCodexPackages(t, others)
	origPath := os.Getenv("PATH")

	liveCancelRuns(t, []time.Duration{time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second}, func(t *testing.T, delay time.Duration) {
		root := liveNPMScratchRoot(t)
		prefix := filepath.Join(root, "prefix")
		seed := exec.Command(npm, "install", "-g", "--no-fund", "--no-audit", CLIPackageName+"@"+older)
		seed.Env = append(os.Environ(), "npm_config_prefix="+prefix)
		if out, err := seed.CombinedOutput(); err != nil {
			t.Fatalf("seed: %v\n%s", err, out)
		}
		path := filepath.Join(prefix, "bin") + string(os.PathListSeparator) + origPath
		t.Setenv("PATH", path)
		client := New(WithEnv(map[string]string{"npm_config_prefix": prefix, "PATH": path}))

		run := cancelUpdateAt(t, client, "Installing ", delay, "npm_config_prefix="+prefix)
		stray, _ := filepath.Glob(filepath.Join(prefix, "bin", ".codex-*"))
		strayPkg, _ := filepath.Glob(filepath.Join(prefix, "lib", "node_modules", "@openai", ".codex-*"))
		t.Logf("left behind: bin %v, pkg %v", stray, strayPkg)
		run.check(t, filepath.Join(prefix, "bin", "codex"), older, latest)
	})
	if after := snapshotCodexPackages(t, others); after != before {
		t.Errorf("another prefix changed:\nbefore %s\nafter  %s", before, after)
	}
}

// TestLive_StandaloneUpdateCancel runs `codex update` in a sandbox — HOME,
// CODEX_HOME and CODEX_INSTALL_DIR in a temp tree, and a PATH holding no other
// codex, so the installer neither sees a conflicting install nor edits a real
// shell profile — seeded by codex's own installer with the release before
// latest.
func TestLive_StandaloneUpdateCancel(t *testing.T) {
	if os.Getenv("CODEXCLI_LIVE_STANDALONE_SCRATCH") != "1" {
		t.Skip("set CODEXCLI_LIVE_STANDALONE_SCRATCH=1 to install standalone codex into a temp tree")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("no npm on PATH to resolve release versions")
	}
	older, latest := liveOlderLatest(t, npm)

	base := t.TempDir()
	work, template := filepath.Join(base, "work"), filepath.Join(base, "template")
	home := filepath.Join(work, "home")
	bin := filepath.Join(work, "bin")
	codexHome := filepath.Join(home, ".codex")
	path := bin + ":/usr/bin:/bin"
	sandbox := []string{"HOME=" + home, "CODEX_HOME=" + codexHome, "CODEX_INSTALL_DIR=" + bin, "PATH=" + path, "CODEX_NON_INTERACTIVE=1", "SHELL=/bin/sh"}

	// Seeded once at work's own path, so the installer's absolute symlinks
	// stay valid when the copy is put back there for each run.
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := exec.Command("sh", "-c", "curl -fsSL https://chatgpt.com/codex/install.sh | sh")
	seed.Env = append(sandbox, "CODEX_RELEASE="+older)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed %s: %v\n%s", older, err, out)
	}
	if out, err := exec.Command("cp", "-a", work, template).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Logf("seeded %s at %s; latest is %s", older, work, latest)

	liveCancelRuns(t, []time.Duration{500 * time.Millisecond, 2 * time.Second, 4 * time.Second}, func(t *testing.T, delay time.Duration) {
		if err := os.RemoveAll(work); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-a", template, work).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		t.Setenv("PATH", path)
		client := New(WithCodexHome(codexHome), WithEnv(map[string]string{
			"HOME": home, "CODEX_INSTALL_DIR": bin, "PATH": path, "SHELL": "/bin/sh",
		}))
		if info, err := client.DetectInstall(context.Background()); err != nil || info.Method != InstallNative || info.Version != older {
			t.Fatalf("detected %+v, %v; want standalone %s", info, err, older)
		}

		run := cancelUpdateAt(t, client, "Downloading Codex CLI", delay, "CODEX_HOME="+codexHome)
		staging, _ := filepath.Glob(filepath.Join(codexHome, "packages", "standalone", "releases", ".staging.*"))
		releases, _ := filepath.Glob(filepath.Join(codexHome, "packages", "standalone", "releases", "*"))
		current, _ := os.Readlink(filepath.Join(codexHome, "packages", "standalone", "current"))
		t.Logf("current -> %s; releases %v; staging %v", current, releases, staging)
		run.check(t, filepath.Join(bin, "codex"), older, latest)
	})
}

// liveCancelRuns runs fn for each delay, CODEXCLI_LIVE_CANCEL_RUNS times
// (default 1), with CODEXCLI_LIVE_CANCEL_AT replacing defaults when set.
func liveCancelRuns(t *testing.T, defaults []time.Duration, fn func(t *testing.T, delay time.Duration)) {
	t.Helper()
	delays := defaults
	if s := os.Getenv("CODEXCLI_LIVE_CANCEL_AT"); s != "" {
		delays = nil
		for _, f := range strings.Split(s, ",") {
			d, err := time.ParseDuration(strings.TrimSpace(f))
			if err != nil {
				t.Fatal(err)
			}
			delays = append(delays, d)
		}
	}
	runs := 1
	if s := os.Getenv("CODEXCLI_LIVE_CANCEL_RUNS"); s != "" {
		var err error
		if runs, err = strconv.Atoi(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, delay := range delays {
		for i := 0; i < runs; i++ {
			t.Run(delay.String()+"#"+strconv.Itoa(i), func(t *testing.T) { fn(t, delay) })
		}
	}
}

// cancelledUpdate is what one cancelled Update did, timed from the cancel.
type cancelledUpdate struct {
	result *UpdateResult
	err    error
	// ran is how long the updater's processes lived; gone and returned are
	// when they had all exited and when Update returned, after the cancel.
	// Zero when the cancel never fired: the updater finished first.
	ran, gone, returned time.Duration
	cancelled           bool
}

// cancelUpdateAt runs client.Update and cancels it delay after the first
// output line containing trigger. The updater's processes are found by
// procEnv, a KEY=value only they carry in their environment (not argv: npm
// overwrites its own with process.title).
func cancelUpdateAt(t *testing.T, client *Client, trigger string, delay time.Duration, procEnv string) cancelledUpdate {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu                               sync.Mutex
		triggeredAt, cancelledAt, goneAt time.Time
		lines                            []string
	)
	watchDone, returnedCh := make(chan struct{}), make(chan struct{})
	var once sync.Once
	onLine := func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
		if !strings.Contains(line, trigger) {
			return
		}
		once.Do(func() {
			mu.Lock()
			triggeredAt = time.Now()
			mu.Unlock()
			time.AfterFunc(delay, func() {
				mu.Lock()
				cancelledAt = time.Now()
				mu.Unlock()
				cancel()
			})
			go func() {
				defer close(watchDone)
				// npm starts after its trigger line, the installer before;
				// either way gone is the first check after both that finds
				// nothing.
				seen := false
				for {
					alive := processWithEnv(procEnv)
					seen = seen || alive
					returned := false
					select {
					case <-returnedCh:
						returned = true
					default:
					}
					if !alive && (seen || returned) {
						mu.Lock()
						goneAt = time.Now()
						mu.Unlock()
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
		})
	}

	start := time.Now()
	result, err := client.Update(ctx, WithUpdateProgress(onLine))
	returned := time.Now()
	close(returnedCh)
	select {
	case <-watchDone:
	case <-time.After(30 * time.Second):
		t.Error("updater still running 30s after Update returned")
	}

	mu.Lock()
	defer mu.Unlock()
	run := cancelledUpdate{result: result, err: err, ran: goneAt.Sub(triggeredAt)}
	if !cancelledAt.IsZero() && cancelledAt.Before(returned) {
		run.cancelled = true
		run.gone = goneAt.Sub(cancelledAt)
		run.returned = returned.Sub(cancelledAt)
	}
	var after string
	var changed bool
	if result != nil {
		after, changed = result.VersionAfter, result.Changed
	}
	t.Logf("cancel at +%s (fired %v): updater ran %s, gone %s and Update returned %s after the cancel (%s total); after=%q changed=%v err=%v",
		delay, run.cancelled, run.ran.Round(10*time.Millisecond), run.gone.Round(10*time.Millisecond),
		run.returned.Round(10*time.Millisecond), returned.Sub(start).Round(10*time.Millisecond), after, changed, err)
	if n := len(lines); n > 0 {
		t.Logf("last output: %s", strings.Join(lines[max(0, n-4):], " | "))
	}
	return run
}

// check asserts the outcome a consumer depends on: a codex still runs at
// codexPath, at one of the two versions, and the result never claims a
// version that is not the one installed.
func (r cancelledUpdate) check(t *testing.T, codexPath, older, latest string) {
	t.Helper()
	out, err := exec.Command(codexPath, "--version").Output()
	got := strings.TrimPrefix(strings.TrimSpace(string(out)), "codex-cli ")
	t.Logf("%s --version: %q, err %v", codexPath, got, err)
	if err != nil {
		t.Errorf("no working codex left at %s: %v", codexPath, err)
		return
	}
	if got != older && got != latest {
		t.Errorf("codex reports %q, want %s or %s", got, older, latest)
	}
	if r.err == nil && (r.result == nil || r.result.VersionAfter != got || got != latest) {
		t.Errorf("Update reported success with result %+v, but %s is installed", r.result, got)
	}
	if r.result != nil && r.result.VersionAfter != "" && r.result.VersionAfter != got {
		t.Errorf("result says %q, but %s is installed", r.result.VersionAfter, got)
	}
	if r.cancelled && r.err != nil && !errors.Is(r.err, context.Canceled) {
		t.Errorf("err = %v, want a cancelled update to match context.Canceled", r.err)
	}
}

// liveOlderLatest returns the latest release and the stable one before it.
func liveOlderLatest(t *testing.T, npm string) (older, latest string) {
	t.Helper()
	out := func(args ...string) []byte {
		b, err := exec.Command(npm, args...).Output()
		if err != nil {
			t.Fatalf("npm %v: %v", args, err)
		}
		return b
	}
	latest = strings.TrimSpace(string(out("view", CLIPackageName+"@latest", "version")))
	var versions []string
	if err := json.Unmarshal(out("view", CLIPackageName, "versions", "--json"), &versions); err != nil {
		t.Fatal(err)
	}
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
	return older, latest
}

// liveNPMScratchRoot is a fresh directory for one throwaway prefix, under
// CODEXCLI_LIVE_NPM_SCRATCH_DIR when set: npm prints a UUID-shaped path
// segment as "***", so a prefix under a session-named TMPDIR is unprovable.
func liveNPMScratchRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv("CODEXCLI_LIVE_NPM_SCRATCH_DIR")
	if base == "" {
		return t.TempDir()
	}
	root, err := os.MkdirTemp(base, "npm-scratch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

// processWithEnv reports whether a live (not zombie) process carries kv in
// its environment.
func processWithEnv(kv string) bool {
	procs, _ := filepath.Glob("/proc/[0-9]*/environ")
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, e := range strings.Split(string(b), "\x00") {
			if e != kv {
				continue
			}
			if st, err := os.ReadFile(filepath.Join(filepath.Dir(p), "stat")); err == nil && !strings.Contains(string(st), ") Z ") {
				return true
			}
		}
	}
	return false
}
