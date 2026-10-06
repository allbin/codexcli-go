package codexcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// winCodexShim is the codex.cmd npm 11.6.0's cmd-shim wrote on the reference
// Windows machine, verbatim.
const winCodexShim = "@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nSETLOCAL\r\nCALL :find_dp0\r\n\r\n" +
	"IF EXIST \"%dp0%\\node.exe\" (\r\n  SET \"_prog=%dp0%\\node.exe\"\r\n) ELSE (\r\n  SET \"_prog=node\"\r\n  SET PATHEXT=%PATHEXT:;.JS;=;%\r\n)\r\n\r\n" +
	"endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & \"%_prog%\"  \"%dp0%\\node_modules\\@openai\\codex\\bin\\codex.js\" %*\r\n"

// winNPMFixture is the verified Windows global layout in a temp dir: shims
// and node_modules\@openai\codex directly under the prefix, and npm in a
// directory of its own, as it ships beside node.exe. Paths use the native
// separator, so the Windows proof runs over it on any OS.
type winNPMFixture struct {
	root    string
	prefix  string
	pkgRoot string
	shim    string
	npm     string

	mu          sync.Mutex
	prefixCalls []string // npm|binDir for each `prefix -g`
}

func newWinNPMFixture(t *testing.T) *winNPMFixture {
	t.Helper()
	root := t.TempDir()
	f := &winNPMFixture{root: root, prefix: filepath.Join(root, "npm")}
	f.pkgRoot = filepath.Join(f.prefix, "node_modules", "@openai", "codex")
	f.shim = filepath.Join(f.prefix, "codex.cmd")
	f.npm = filepath.Join(root, "nodejs", "npm.cmd")

	mustMkdir(t, filepath.Join(f.pkgRoot, "bin"))
	mustMkdir(t, filepath.Dir(f.npm))
	mustWrite(t, filepath.Join(f.pkgRoot, "package.json"), cliPackageJSON, 0o644)
	mustWrite(t, filepath.Join(f.pkgRoot, "bin", "codex.js"), "", 0o644)
	mustWrite(t, f.shim, winCodexShim, 0o644)
	mustWrite(t, filepath.Join(f.prefix, "codex"), "#!/bin/sh\n", 0o644)
	mustWrite(t, filepath.Join(f.prefix, "codex.ps1"), "#!/usr/bin/env pwsh\n", 0o644)
	writeNPMCmd(t, filepath.Dir(f.npm))
	return f
}

// winNodeNPMCmd is the npm.cmd node 24.20.0's installer put beside node.exe
// on the reference machine, verbatim.
const winNodeNPMCmd = ":: Created by npm, please don't edit manually.\r\n@ECHO OFF\r\n\r\nSETLOCAL\r\n\r\n" +
	"SET \"NODE_EXE=%~dp0\\node.exe\"\r\nIF NOT EXIST \"%NODE_EXE%\" (\r\n  SET \"NODE_EXE=node\"\r\n)\r\n\r\n" +
	"SET \"NPM_PREFIX_JS=%~dp0\\node_modules\\npm\\bin\\npm-prefix.js\"\r\n" +
	"SET \"NPM_CLI_JS=%~dp0\\node_modules\\npm\\bin\\npm-cli.js\"\r\n" +
	"FOR /F \"delims=\" %%F IN ('CALL \"%NODE_EXE%\" \"%NPM_PREFIX_JS%\"') DO (\r\n  SET \"NPM_PREFIX_NPM_CLI_JS=%%F\\node_modules\\npm\\bin\\npm-cli.js\"\r\n)\r\n" +
	"IF EXIST \"%NPM_PREFIX_NPM_CLI_JS%\" (\r\n  SET \"NPM_CLI_JS=%NPM_PREFIX_NPM_CLI_JS%\"\r\n)\r\n\r\n\"%NODE_EXE%\" \"%NPM_CLI_JS%\" %*\r\n"

// writeNPMCmd puts npm's own npm.cmd, and the npm package it runs, in dir.
func writeNPMCmd(t *testing.T, dir string) {
	t.Helper()
	mustMkdir(t, filepath.Join(dir, "node_modules", "npm", "bin"))
	mustWrite(t, filepath.Join(dir, "node_modules", "npm", "package.json"), `{"name":"npm","version":"11.6.0"}`, 0o644)
	mustWrite(t, filepath.Join(dir, "npm.cmd"), winNodeNPMCmd, 0o644)
}

// env is the real file system with the Windows proof selected, npm's
// directory alone on the child's PATH, and `npm prefix -g` answering
// reported.
func (f *winNPMFixture) env(reported string) installEnv {
	env := osInstallEnv("")
	env.goos = "windows"
	env.childPath = filepath.Dir(f.npm)
	env.childPathExt = ".COM;.EXE;.BAT;.CMD"
	env.npmPrefix = func(_ context.Context, npm, binDir string) (string, error) {
		f.mu.Lock()
		f.prefixCalls = append(f.prefixCalls, npm+"|"+binDir)
		f.mu.Unlock()
		return reported, nil
	}
	return env
}

func (f *winNPMFixture) info() *InstallInfo {
	return &InstallInfo{Path: f.shim, RealPath: f.shim, Method: InstallNPMGlobal, PackageManager: "npm"}
}

func (f *winNPMFixture) secondPrefix(t *testing.T) string {
	t.Helper()
	other := filepath.Join(f.root, "other-npm")
	pkg := filepath.Join(other, "node_modules", "@openai", "codex")
	mustMkdir(t, pkg)
	mustWrite(t, filepath.Join(pkg, "package.json"), cliPackageJSON, 0o644)
	return other
}

func TestNPMUpdateWindows_OwnPrefixIsProven(t *testing.T) {
	f := newWinNPMFixture(t)
	plan, err := proveNPMUpdate(context.Background(), f.info(), f.env(f.prefix))
	if err != nil {
		t.Fatalf("proveNPMUpdate: %v", err)
	}
	if plan.npm != f.npm || plan.pathDir != filepath.Dir(f.npm) {
		t.Errorf("npm = %q in %q, want %q in its own directory", plan.npm, plan.pathDir, f.npm)
	}
	if plan.prefix != f.prefix || plan.pinPrefix != f.prefix {
		t.Errorf("prefix = %q pinned to %q, want both %q", plan.prefix, plan.pinPrefix, f.prefix)
	}
	wantTargets := []string{filepath.Join(f.prefix, "node_modules"), f.prefix}
	if strings.Join(plan.targets, "|") != strings.Join(wantTargets, "|") {
		t.Errorf("targets = %v, want %v", plan.targets, wantTargets)
	}
	wantInUse := []string{f.pkgRoot, filepath.Join(f.prefix, "codex"), f.shim, filepath.Join(f.prefix, "codex.ps1")}
	if strings.Join(plan.inUse, "|") != strings.Join(wantInUse, "|") {
		t.Errorf("inUse = %v, want %v", plan.inUse, wantInUse)
	}
	if want := []string{f.npm + "|" + filepath.Dir(f.npm)}; strings.Join(f.prefixCalls, ",") != strings.Join(want, ",") {
		t.Errorf("prefix -g calls = %v, want %v", f.prefixCalls, want)
	}
}

func TestNPMUpdateWindows_SameDirectoryOtherSpellingIsProven(t *testing.T) {
	f := newWinNPMFixture(t)
	sep := string(filepath.Separator)
	spellings := []string{f.prefix + sep + "node_modules" + sep + ".."}
	if runtime.GOOS == "windows" {
		// The file system is case-insensitive; resolution restores case.
		spellings = append(spellings, strings.ToUpper(f.prefix))
	}
	for _, reported := range spellings {
		if _, err := proveNPMUpdate(context.Background(), f.info(), f.env(reported)); err != nil {
			t.Errorf("reported %q: %v", reported, err)
		}
	}
}

func TestNPMUpdateWindows_RefusesUnproven(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the reported prefix, and may rewrite the fixture.
		setup      func(t *testing.T, f *winNPMFixture) (reported string, info *InstallInfo)
		reason     string
		probesNPM  bool
		configured func(f *winNPMFixture, env *installEnv)
	}{
		{
			name: "npm writes a prefix holding a second copy",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				return f.secondPrefix(t), f.info()
			},
			reason:    "PATH runs",
			probesNPM: true,
		},
		{
			// npm 11 prints UUID-shaped path segments as *** (old npm tokens
			// were UUIDs), so a prefix under one reports a path that does not
			// exist. Observed with a prefix under a session temp directory.
			name: "npm redacted the prefix it reported",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				return filepath.Join(f.root, "***", "npm"), f.info()
			},
			reason:    "resolve",
			probesNPM: true,
		},
		{
			name: "npm reported nothing",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				return "", f.info()
			},
			reason:    "reported nothing",
			probesNPM: true,
		},
		{
			name: "shim runs some other package",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				mustWrite(t, f.shim, strings.ReplaceAll(winCodexShim, `@openai\codex`, `@other\codex`), 0o644)
				return f.prefix, f.info()
			},
			reason: "does not run",
		},
		{
			name: "shim runs the package by absolute path",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				mustWrite(t, f.shim, strings.ReplaceAll(winCodexShim, `%dp0%\node_modules`, `C:\elsewhere\node_modules`), 0o644)
				return f.prefix, f.info()
			},
			reason: "does not run",
		},
		{
			name: "powershell shim",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				info := f.info()
				info.RealPath = filepath.Join(f.prefix, "codex.ps1")
				return f.prefix, info
			},
			reason: "not an npm .cmd shim",
		},
		{
			name: "no package beside the shim",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				mustWrite(t, filepath.Join(f.pkgRoot, "package.json"), `{"name":"not-codex"}`, 0o644)
				return f.prefix, f.info()
			},
			reason: "package.json",
		},
		{
			name: "no npm found",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				return f.prefix, f.info()
			},
			configured: func(f *winNPMFixture, env *installEnv) {
				env.childPath = f.pkgRoot
			},
			reason: "find npm",
		},
		{
			// Volta's npm answers prefix -g from npm's config and installs
			// into its own image directory.
			name: "a dispatcher's npm.exe comes first on PATH",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				mustMkdir(t, filepath.Join(f.root, "volta", "bin"))
				mustWrite(t, filepath.Join(f.root, "volta", "bin", "npm.exe"), "MZ", 0o755)
				return f.prefix, f.info()
			},
			configured: func(f *winNPMFixture, env *installEnv) {
				env.childPath = filepath.Join(f.root, "volta", "bin") + string(os.PathListSeparator) + env.childPath
			},
			reason: "not npm's npm.cmd",
		},
		{
			name: "an npm.cmd that runs something else",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				mustWrite(t, f.npm, "@mise x -- npm %*\r\n", 0o644)
				return f.prefix, f.info()
			},
			reason: "npm-cli.js",
		},
		{
			name: "an npm.cmd with no npm package beside it",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				if err := os.RemoveAll(filepath.Join(filepath.Dir(f.npm), "node_modules")); err != nil {
					t.Fatal(err)
				}
				return f.prefix, f.info()
			},
			reason: "no npm package",
		},
		{
			name: "npm prefix -g failed",
			setup: func(t *testing.T, f *winNPMFixture) (string, *InstallInfo) {
				return f.prefix, f.info()
			},
			configured: func(f *winNPMFixture, env *installEnv) {
				env.npmPrefix = func(context.Context, string, string) (string, error) { return "", errors.New("exit status 1") }
			},
			reason: "prefix -g",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newWinNPMFixture(t)
			reported, info := tt.setup(t, f)
			env := f.env(reported)
			if tt.configured != nil {
				tt.configured(f, &env)
			}
			plan, err := proveNPMUpdate(context.Background(), info, env)
			if !errors.Is(err, errNPMUnproven) {
				t.Fatalf("plan = %+v, err = %v, want errNPMUnproven", plan, err)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("err = %q, want it to name %q", err, tt.reason)
			}
			if probed := len(f.prefixCalls) > 0; probed != tt.probesNPM {
				t.Errorf("ran npm prefix -g = %v, want %v", probed, tt.probesNPM)
			}
			if installSelfManaged(context.Background(), info, env) {
				t.Error("SelfManaged = true although the proof refused")
			}
		})
	}
}

func TestNPMUpdateWindows_OlderShimSpelling(t *testing.T) {
	f := newWinNPMFixture(t)
	mustWrite(t, f.shim, "@\"%~dp0\\node_modules\\@openai\\codex\\bin\\codex.js\" %*\r\n", 0o644)
	if _, err := proveNPMUpdate(context.Background(), f.info(), f.env(f.prefix)); err != nil {
		t.Fatalf("proveNPMUpdate: %v", err)
	}
}

func TestNPMUpdateWindows_DetectionSharesTheVerdict(t *testing.T) {
	f := newWinNPMFixture(t)
	env := f.env(f.prefix)
	env.lookPath = func(string) (string, error) { return f.shim, nil }
	env.runVersion = func(context.Context, string) (string, error) { return "0.159.0", nil }

	info, err := detectInstall(context.Background(), "codex", env)
	if err != nil {
		t.Fatal(err)
	}
	if info.Method != InstallNPMGlobal || info.PackageManager != "npm" || info.Source != InstallSourcePackageMetadata {
		t.Fatalf("detected %s/%s from %s, want npm-global/npm from package metadata", info.Method, info.PackageManager, info.Source)
	}
	if !installSelfManaged(context.Background(), info, env) {
		t.Error("SelfManaged = false for a proven, writable Windows prefix")
	}
	if mismatched := f.env(f.secondPrefix(t)); installSelfManaged(context.Background(), info, mismatched) {
		t.Error("SelfManaged = true although npm writes a different prefix")
	}
}

// winUpdateStub drives runUpdate over the Windows fixture with every process
// stubbed: the version sequence, `npm view`, the in-use probe and the run.
type winUpdateStub struct {
	env    updateEnv
	ran    *updaterRun
	probed [][]string
}

func newWinUpdateStub(t *testing.T, f *winNPMFixture, latest string, held string, versions ...string) *winUpdateStub {
	t.Helper()
	s := &winUpdateStub{}
	ienv := f.env(f.prefix)
	ienv.lookPath = func(string) (string, error) { return f.shim, nil }
	ienv.runVersion = versionSequence(versions...)
	ienv.writable = func(string) error { return nil }
	s.env = updateEnv{
		installEnv: ienv,
		npmLatest:  func(context.Context, string, string) (string, error) { return latest, nil },
		inUse: func(paths []string) (string, error) {
			s.probed = append(s.probed, paths)
			return held, nil
		},
		runUpdate: func(_ context.Context, run updaterRun, onLine func(string)) (int, error) {
			s.ran = &run
			onLine("changed 2 packages in 3s")
			return 0, nil
		},
	}
	return s
}

func TestNPMUpdateWindows_RunsTheProvenNPM(t *testing.T) {
	f := newWinNPMFixture(t)
	s := newWinUpdateStub(t, f, "0.160.1", "", "0.159.0", "0.160.1")

	result, err := runUpdate(context.Background(), "codex", s.env, nil)
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !result.Changed || result.VersionAfter != "0.160.1" || result.Updater != f.npm || result.Path != f.shim {
		t.Errorf("result = %+v", result)
	}
	if s.ran == nil {
		t.Fatal("npm install never ran")
	}
	if s.ran.name != f.npm || s.ran.pathDir != filepath.Dir(f.npm) {
		t.Errorf("ran %q with %q on PATH, want the proven npm beside its node", s.ran.name, s.ran.pathDir)
	}
	if got, want := strings.Join(s.ran.args, " "), "install --global --prefix "+f.prefix+" @openai/codex@0.160.1"; got != want {
		t.Errorf("args = %q, want %q: version and prefix pinned", got, want)
	}
	if !s.ran.waitOnCancel {
		t.Error("the Windows install run is cancellable; a kill mid-reify breaks the install")
	}
	if len(s.probed) != 1 || s.probed[0][0] != f.pkgRoot {
		t.Errorf("in-use probe = %v, want one probe of the package tree and shims", s.probed)
	}
}

func TestNPMUpdateWindows_HeldFileBlocks(t *testing.T) {
	f := newWinNPMFixture(t)
	exe := filepath.Join(f.pkgRoot, "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe")
	s := newWinUpdateStub(t, f, "0.160.1", exe, "0.159.0")

	result, err := runUpdate(context.Background(), "codex", s.env, nil)
	var inUse *UpdateInUseError
	if !errors.As(err, &inUse) || !errors.Is(err, ErrUpdateInUse) {
		t.Fatalf("err = %v, want *UpdateInUseError", err)
	}
	if inUse.Path != exe || inUse.Method != InstallNPMGlobal {
		t.Errorf("err = %+v, want the held file named", inUse)
	}
	if errors.Is(err, ErrUpdateNotWritable) || errors.Is(err, ErrUpdateFailed) {
		t.Errorf("err = %v also matches a permanent or attempted outcome", err)
	}
	if result != nil {
		t.Errorf("result = %+v, want nil when nothing was installed", result)
	}
	if s.ran != nil {
		t.Errorf("ran %v although a file was held", s.ran.args)
	}
}

func TestNPMUpdateWindows_AlreadyLatestIgnoresHeldFiles(t *testing.T) {
	// A running session does not turn "nothing to do" into "blocked".
	f := newWinNPMFixture(t)
	s := newWinUpdateStub(t, f, "0.160.1", filepath.Join(f.pkgRoot, "held"), "0.160.1")

	result, err := runUpdate(context.Background(), "codex", s.env, nil)
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if result.Changed || result.VersionAfter != "0.160.1" || s.ran != nil || len(s.probed) != 0 {
		t.Errorf("result = %+v, ran %v, probed %v; want an unchanged no-op", result, s.ran, s.probed)
	}
}

func TestNPMUpdateWindows_UnprobeableFileIsNotWritable(t *testing.T) {
	f := newWinNPMFixture(t)
	s := newWinUpdateStub(t, f, "0.160.1", "", "0.159.0")
	held := filepath.Join(f.pkgRoot, "README.md")
	s.env.inUse = func([]string) (string, error) { return held, os.ErrPermission }

	_, err := runUpdate(context.Background(), "codex", s.env, nil)
	var blocked *UpdateNotWritableError
	if !errors.As(err, &blocked) || blocked.Dir != f.pkgRoot {
		t.Fatalf("err = %v, want *UpdateNotWritableError for %s", err, f.pkgRoot)
	}
	if s.ran != nil {
		t.Error("npm ran after the in-use probe failed")
	}

	s.env.inUse = nil
	if _, err := runUpdate(context.Background(), "codex", s.env, nil); !errors.Is(err, ErrUpdateNotWritable) {
		t.Fatalf("err = %v, want a missing in-use probe to refuse", err)
	}
}

func TestWindowsNPM(t *testing.T) {
	root := t.TempDir()
	sep := string(os.PathListSeparator)
	first, second, prefix, rel := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "prefix"), "rel"
	for _, d := range []string{first, second, prefix} {
		mustMkdir(t, d)
	}
	mustWrite(t, filepath.Join(first, "npm.js"), "", 0o644)
	mustWrite(t, filepath.Join(second, "npm.cmd"), "", 0o644)
	mustWrite(t, filepath.Join(prefix, "npm.cmd"), "", 0o644)

	got, err := windowsNPM(prefix, rel+sep+first+sep+`"`+second+`"`, ".COM;.EXE;.BAT;.CMD;.JS")
	if err != nil || got != filepath.Join(second, "npm.cmd") {
		t.Errorf("windowsNPM = %q, %v; want the first runnable npm on PATH, %s", got, err, filepath.Join(second, "npm.cmd"))
	}
	got, err = windowsNPM(prefix, first, "")
	if err != nil || got != filepath.Join(prefix, "npm.cmd") {
		t.Errorf("windowsNPM = %q, %v; want the prefix's npm.cmd when PATH has none", got, err)
	}
	if got, err := windowsNPM(first, first, ""); err == nil {
		t.Errorf("windowsNPM = %q, want an error when there is no npm", got)
	}
}

func TestWindowsRunnableExts(t *testing.T) {
	if got := strings.Join(windowsRunnableExts(".COM;.EXE;.BAT;.CMD;.VBS;.JS;.PY"), ","); got != ".com,.exe,.bat,.cmd" {
		t.Errorf("windowsRunnableExts = %s", got)
	}
	if got := strings.Join(windowsRunnableExts(""), ","); got != ".com,.exe,.bat,.cmd" {
		t.Errorf("windowsRunnableExts(\"\") = %s, want the CreateProcess default", got)
	}
}

// fakeUpdaterScript writes a stand-in updater that prints "started", waits
// about two seconds and prints "done": a batch file on Windows, so the real
// .cmd exec path is the one exercised there. On unix it answers SIGINT the
// way npm does, by printing "interrupted", taking a second to "roll back",
// and exiting 1.
func fakeUpdaterScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		name := filepath.Join(dir, "npm.cmd")
		mustWrite(t, name, "@echo off\r\necho started\r\nping -n 3 127.0.0.1 >nul\r\necho done\r\n", 0o644)
		return name
	}
	name := filepath.Join(dir, "npm")
	mustWrite(t, name, "#!/bin/sh\ntrap 'echo interrupted; sleep 1; echo rolled back; exit 1' INT\necho started\nsleep 2\necho done\n", 0o755)
	return name
}

func TestUpdaterRunWaitOnCancel(t *testing.T) {
	// A started run is not killed by cancellation, and is waited for: run to
	// completion on Windows, where nothing can be delivered; interrupted and
	// given its rollback on unix.
	script := fakeUpdaterScript(t)
	env := osUpdateEnv("", nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var lines []string
	start := time.Now()
	code, err := env.runUpdate(ctx, updaterRun{name: script, waitOnCancel: true}, func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
		if line == "started" {
			cancel()
		}
	})
	wantLines, wantCode := "started|done", 0
	if updaterInterruptible {
		wantLines, wantCode = "started|interrupted|rolled back", 1
	}
	if code != wantCode || (err == nil) != (wantCode == 0) {
		t.Fatalf("run = %d, %v; want the stand-in's own exit %d", code, err, wantCode)
	}
	if got := strings.Join(lines, "|"); got != wantLines {
		t.Errorf("lines = %q, want %q", got, wantLines)
	}
	if time.Since(start) < time.Second {
		t.Error("returned before the stand-in could have finished")
	}

	// A run whose context ended before it started is never started.
	lines = nil
	code, err = env.runUpdate(ctx, updaterRun{name: script, waitOnCancel: true}, func(line string) { lines = append(lines, line) })
	if !errors.Is(err, context.Canceled) || code != -1 || len(lines) != 0 {
		t.Errorf("run = %d, %v, lines %v; want it not started", code, err, lines)
	}
}
