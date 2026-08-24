package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The live standalone layout: a thin symlink on PATH pointing at the
// `current` symlink, which points at the release tree under CODEX_HOME.
const (
	fakeCodexHome  = "/home/u/.codex"
	standaloneRoot = fakeCodexHome + "/packages/standalone"
	releasesDir    = standaloneRoot + "/releases"
	releaseTree    = releasesDir + "/0.148.0-x86_64-unknown-linux-musl"
	standaloneReal = releaseTree + "/bin/codex"
	standalonePath = "/home/u/.local/bin/codex"
	fakeBinDir     = "/home/u/.local/bin"
)

// updateEnvStub records what runUpdate was asked to do, so a test can assert
// on the binary that was executed as well as the result.
type updateEnvStub struct {
	env updateEnv

	ran       bool
	ranBinary string
	probed    []string
}

// fakeUpdateEnv builds an updateEnv over a synthetic standalone install: no
// codex on the machine, no filesystem writes, every ambient operation stubbed.
func fakeUpdateEnv(t *testing.T) *updateEnvStub {
	t.Helper()
	stub := &updateEnvStub{}
	stub.env = updateEnv{
		installEnv: installEnv{
			lookPath: func(string) (string, error) { return standalonePath, nil },
			evalSymlink: func(p string) (string, error) {
				switch p {
				case standalonePath, standaloneRoot + "/current":
					return standaloneReal, nil
				}
				return "", os.ErrNotExist
			},
			readFile:   func(string) ([]byte, error) { return nil, os.ErrNotExist },
			runVersion: func(context.Context, string) (string, error) { return "0.148.0", nil },
			codexHome:  fakeCodexHome,
		},
		binDir: fakeBinDir,
		writable: func(dir string) error {
			stub.probed = append(stub.probed, dir)
			return nil
		},
		runUpdate: func(_ context.Context, binary string, onLine func(string)) (int, error) {
			stub.ran = true
			stub.ranBinary = binary
			onLine("Updating Codex CLI")
			return 0, nil
		},
	}
	return stub
}

// versionSequence returns a runVersion that answers with each value in turn,
// repeating the last one. detectInstall probes once before the update and
// reprobeVersion once after.
func versionSequence(versions ...string) func(context.Context, string) (string, error) {
	i := 0
	return func(context.Context, string) (string, error) {
		v := versions[min(i, len(versions)-1)]
		i++
		return v, nil
	}
}

func TestRunUpdate_RefusesInstallsCodexDoesNotManage(t *testing.T) {
	// Every one of these is a normal outcome carrying a command to display —
	// or deliberately no command at all. None of them may run anything.
	tests := []struct {
		name        string
		realPath    string
		files       map[string]string
		wantMethod  InstallMethod
		wantCommand string
	}{
		{
			name:        "npm global",
			realPath:    "/usr/local/lib/node_modules/@openai/codex/bin/codex.js",
			files:       map[string]string{"/usr/local/lib/node_modules/@openai/codex/package.json": cliPackageJSON},
			wantMethod:  InstallNPMGlobal,
			wantCommand: "npm install -g @openai/codex@latest",
		},
		{
			name:        "pnpm global",
			realPath:    "/home/u/.local/share/pnpm/global/5/node_modules/@openai/codex/bin/codex.js",
			wantMethod:  InstallNPMGlobal,
			wantCommand: "pnpm add -g @openai/codex@latest",
		},
		{
			name:        "homebrew cask",
			realPath:    "/opt/homebrew/Caskroom/codex/0.148.0/codex",
			wantMethod:  InstallPackageManager,
			wantCommand: "brew upgrade --cask codex",
		},
		{
			// No command is known to be correct here, and an empty Command is
			// the answer — never a guess at npm.
			name:        "version manager root",
			realPath:    "/home/u/.nvm/versions/node/v22.0.0/bin/codex",
			wantMethod:  InstallVersionManager,
			wantCommand: "",
		},
		{
			name:        "bare binary in a bin dir",
			realPath:    "/usr/local/bin/codex",
			wantMethod:  InstallUnknown,
			wantCommand: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := fakeUpdateEnv(t)
			// Only the PATH entry resolves: there is no standalone install
			// under CODEX_HOME in any of these layouts.
			stub.env.evalSymlink = func(p string) (string, error) {
				if p == standalonePath {
					return tt.realPath, nil
				}
				return "", os.ErrNotExist
			}
			if tt.files != nil {
				stub.env.readFile = func(name string) ([]byte, error) {
					if content, ok := tt.files[name]; ok {
						return []byte(content), nil
					}
					return nil, os.ErrNotExist
				}
			}

			result, err := runUpdate(context.Background(), "codex", stub.env, nil)
			if !errors.Is(err, ErrManualUpdate) {
				t.Fatalf("err = %v, want ErrManualUpdate", err)
			}
			if result != nil {
				t.Errorf("result = %+v, want nil for a refusal", result)
			}
			var manual *ManualUpdateError
			if !errors.As(err, &manual) {
				t.Fatalf("err = %v, want a *ManualUpdateError", err)
			}
			if manual.Method != tt.wantMethod {
				t.Errorf("Method = %q, want %q", manual.Method, tt.wantMethod)
			}
			if manual.Command != tt.wantCommand {
				t.Errorf("Command = %q, want %q verbatim", manual.Command, tt.wantCommand)
			}
			if stub.ran {
				t.Error("ran the updater for an install codex does not manage")
			}
			if len(stub.probed) != 0 {
				t.Errorf("probed %v for writability before refusing", stub.probed)
			}
		})
	}
}

func TestRunUpdate_ManualErrorMessageNeverInventsACommand(t *testing.T) {
	withCmd := &ManualUpdateError{Method: InstallNPMGlobal, Command: "npm install -g @openai/codex@latest"}
	if !strings.Contains(withCmd.Error(), "npm install -g @openai/codex@latest") {
		t.Errorf("Error() = %q, want it to quote the command", withCmd.Error())
	}
	none := &ManualUpdateError{Method: InstallUnknown}
	if strings.Contains(none.Error(), "npm") {
		t.Errorf("Error() = %q, want no invented command", none.Error())
	}
	if !strings.Contains(none.Error(), "manually") {
		t.Errorf("Error() = %q, want it to say update manually", none.Error())
	}
}

func TestRunUpdate_PreflightBlocksBeforeRunning(t *testing.T) {
	// "Cannot" and "tried and failed" are different answers: a blocked update
	// must never have run anything.
	tests := []struct {
		name    string
		denied  string
		wantDir string
	}{
		{name: "releases directory", denied: releasesDir, wantDir: releasesDir},
		{name: "visible symlink directory", denied: fakeBinDir, wantDir: fakeBinDir},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := fakeUpdateEnv(t)
			denied := errors.New("permission denied")
			stub.env.writable = func(dir string) error {
				stub.probed = append(stub.probed, dir)
				if dir == tt.denied {
					return denied
				}
				return nil
			}

			result, err := runUpdate(context.Background(), "codex", stub.env, nil)
			if !errors.Is(err, ErrUpdateNotWritable) {
				t.Fatalf("err = %v, want ErrUpdateNotWritable", err)
			}
			if !errors.Is(err, denied) {
				t.Errorf("err = %v, want the filesystem error unwrappable", err)
			}
			if result != nil {
				t.Errorf("result = %+v, want nil when nothing ran", result)
			}
			var notWritable *UpdateNotWritableError
			if !errors.As(err, &notWritable) {
				t.Fatalf("err = %v, want a *UpdateNotWritableError", err)
			}
			if notWritable.Dir != tt.wantDir {
				t.Errorf("Dir = %q, want %q", notWritable.Dir, tt.wantDir)
			}
			if stub.ran {
				t.Error("ran the updater after the preflight refused it")
			}
		})
	}
}

func TestRunUpdate_ExecutesThePathEntry(t *testing.T) {
	// Not the bare word (a second LookPath can reach a different copy), and
	// not the resolved path (that is the release the update supersedes).
	stub := fakeUpdateEnv(t)
	if _, err := runUpdate(context.Background(), "codex", stub.env, nil); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if stub.ranBinary != standalonePath {
		t.Errorf("executed %q, want the PATH entry %q", stub.ranBinary, standalonePath)
	}
	if stub.ranBinary == standaloneReal {
		t.Error("executed the symlink target, which is the binary being replaced")
	}
}

func TestRunUpdate_VerifiesByVersionNotExitCode(t *testing.T) {
	tests := []struct {
		name        string
		versions    []string
		exitCode    int
		wantChanged bool
	}{
		{
			name:        "version moved",
			versions:    []string{"0.148.0", "0.149.1"},
			wantChanged: true,
		},
		{
			// The trap: codex 0.148.0 was observed exiting 0 and printing
			// "Update ran successfully!" while the command it shells out to
			// was not installed. Only the re-read catches it.
			name:        "exit 0 but nothing moved",
			versions:    []string{"0.148.0", "0.148.0"},
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := fakeUpdateEnv(t)
			stub.env.runVersion = versionSequence(tt.versions...)
			stub.env.runUpdate = func(_ context.Context, _ string, onLine func(string)) (int, error) {
				onLine("Update ran successfully!")
				return tt.exitCode, nil
			}

			result, err := runUpdate(context.Background(), "codex", stub.env, nil)
			if err != nil {
				t.Fatalf("runUpdate: %v", err)
			}
			if result.Changed != tt.wantChanged {
				t.Errorf("Changed = %v, want %v (before %q, after %q)",
					result.Changed, tt.wantChanged, result.VersionBefore, result.VersionAfter)
			}
			if result.VersionBefore != tt.versions[0] {
				t.Errorf("VersionBefore = %q, want %q", result.VersionBefore, tt.versions[0])
			}
			if result.VersionAfter != tt.versions[len(tt.versions)-1] {
				t.Errorf("VersionAfter = %q, want %q", result.VersionAfter, tt.versions[len(tt.versions)-1])
			}
			if result.Method != InstallNative {
				t.Errorf("Method = %q, want %q", result.Method, InstallNative)
			}
			if !strings.Contains(result.Output, "Update ran successfully!") {
				t.Errorf("Output = %q, want the updater transcript", result.Output)
			}
		})
	}
}

func TestRunUpdate_ChangedNeedsBothVersions(t *testing.T) {
	// An unread version is not an unmoved one: "" before or after can never
	// make Changed true, and an unread VersionAfter always carries an error.
	stub := fakeUpdateEnv(t)
	probeErr := errors.New("version probe: exec: no such file")
	calls := 0
	stub.env.runVersion = func(context.Context, string) (string, error) {
		calls++
		if calls == 1 {
			return "0.148.0", nil
		}
		return "", probeErr
	}

	result, err := runUpdate(context.Background(), "codex", stub.env, nil)
	if err == nil {
		t.Fatal("err = nil, want an error when the version could not be re-read")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("err = %v, want the probe error unwrappable", err)
	}
	if result == nil {
		t.Fatal("result = nil, want the half-run update's numbers alongside the error")
	}
	if result.Changed {
		t.Error("Changed = true with an unknown VersionAfter")
	}
	if result.VersionBefore != "0.148.0" || result.VersionAfter != "" {
		t.Errorf("versions = %q → %q, want 0.148.0 → \"\"", result.VersionBefore, result.VersionAfter)
	}
}

func TestRunUpdate_FailedRunReturnsResultAlongsideError(t *testing.T) {
	stub := fakeUpdateEnv(t)
	stub.env.runVersion = versionSequence("0.148.0", "0.148.0")
	exitErr := errors.New("exit status 1")
	stub.env.runUpdate = func(_ context.Context, _ string, onLine func(string)) (int, error) {
		onLine("Downloading Codex CLI")
		onLine("curl: (6) Could not resolve host: releases.openai.com")
		return 1, exitErr
	}

	result, err := runUpdate(context.Background(), "codex", stub.env, nil)
	if !errors.Is(err, ErrUpdateFailed) {
		t.Fatalf("err = %v, want ErrUpdateFailed", err)
	}
	if !errors.Is(err, exitErr) {
		t.Errorf("err = %v, want the exec error unwrappable", err)
	}
	if result == nil {
		t.Fatal("result = nil, want the before/after numbers alongside the error")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
	if result.VersionBefore != "0.148.0" {
		t.Errorf("VersionBefore = %q, want it reported for a failed run too", result.VersionBefore)
	}
	var failed *UpdateFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("err = %v, want a *UpdateFailedError", err)
	}
	if !strings.Contains(failed.Error(), "Could not resolve host") {
		t.Errorf("Error() = %q, want the last output line", failed.Error())
	}
}

func TestRunUpdate_ProgressAndOutput(t *testing.T) {
	stub := fakeUpdateEnv(t)
	stub.env.runUpdate = func(_ context.Context, _ string, onLine func(string)) (int, error) {
		onLine("Detected platform: Linux (x64)")
		onLine("Resolved version: 0.149.1")
		return 0, nil
	}

	var progress []string
	var sink strings.Builder
	result, err := runUpdate(context.Background(), "codex", stub.env,
		[]UpdateOption{
			WithUpdateProgress(func(line string) { progress = append(progress, line) }),
			WithUpdateOutput(&sink),
		})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}

	want := []string{"Detected platform: Linux (x64)", "Resolved version: 0.149.1"}
	if strings.Join(progress, "|") != strings.Join(want, "|") {
		t.Errorf("progress = %v, want %v", progress, want)
	}
	if sink.String() != strings.Join(want, "\n")+"\n" {
		t.Errorf("output = %q, want the lines newline-terminated", sink.String())
	}
	if result.Output != strings.Join(want, "\n") {
		t.Errorf("Output = %q, want %q", result.Output, strings.Join(want, "\n"))
	}
}

func TestUpdateTargets(t *testing.T) {
	// Neither target is the directory holding the binary on PATH.
	tests := []struct {
		name      string
		realPath  string
		codexHome string
		binDir    string
		want      []string
	}{
		{
			name:      "release tree on disk",
			realPath:  standaloneReal,
			codexHome: fakeCodexHome,
			binDir:    fakeBinDir,
			want:      []string{filepath.FromSlash(releasesDir), fakeBinDir},
		},
		{
			name:      "binary elsewhere falls back to CODEX_HOME",
			realPath:  "/opt/codex/bin/codex",
			codexHome: "/srv/codexhome",
			binDir:    fakeBinDir,
			want:      []string{filepath.Join("/srv/codexhome", "packages", "standalone", "releases"), fakeBinDir},
		},
		{
			name:     "no bin dir drops it rather than failing the preflight",
			realPath: standaloneReal,
			want:     []string{filepath.FromSlash(releasesDir)},
		},
		{
			name:     "nothing determinable",
			realPath: "/opt/codex/bin/codex",
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := updateEnv{installEnv: installEnv{codexHome: tt.codexHome}, binDir: tt.binDir}
			got := updateTargets(&InstallInfo{RealPath: tt.realPath, Method: InstallNative}, env)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("updateTargets = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunUpdate_RelocatedCodexHomeIsStillWritable(t *testing.T) {
	// A relocated CODEX_HOME (WithCodexHome, or $CODEX_HOME) moves the release
	// tree with it, and the preflight has to follow rather than probe the
	// default location.
	const relocated = "/srv/codexhome"
	stub := fakeUpdateEnv(t)
	stub.env.codexHome = relocated
	stub.env.evalSymlink = func(p string) (string, error) {
		if p == standalonePath {
			return relocated + "/packages/standalone/releases/0.148.0-x86_64-unknown-linux-musl/bin/codex", nil
		}
		return "", os.ErrNotExist
	}

	if _, err := runUpdate(context.Background(), "codex", stub.env, nil); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	want := []string{filepath.FromSlash(relocated + "/packages/standalone/releases"), fakeBinDir}
	if fmt.Sprint(stub.probed) != fmt.Sprint(want) {
		t.Errorf("probed %v, want %v", stub.probed, want)
	}
}

func TestStandaloneBinDirPrefersSubprocessOverrides(t *testing.T) {
	// The preflight has to probe the directory the updater will write into,
	// which is the one the subprocess environment names.
	t.Setenv("CODEX_INSTALL_DIR", "/process/bin")
	if got := standaloneBinDir(map[string]string{"CODEX_INSTALL_DIR": "/override/bin"}); got != "/override/bin" {
		t.Errorf("standaloneBinDir = %q, want the subprocess override", got)
	}
	if got := standaloneBinDir(nil); got != "/process/bin" {
		t.Errorf("standaloneBinDir = %q, want this process's own value", got)
	}

	t.Setenv("CODEX_INSTALL_DIR", "")
	want := filepath.Join("/home/elsewhere", ".local", "bin")
	if got := standaloneBinDir(map[string]string{"HOME": "/home/elsewhere"}); got != want {
		t.Errorf("standaloneBinDir = %q, want %q", got, want)
	}
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	if err := checkWritable(dir); err != nil {
		t.Errorf("checkWritable(%q) = %v, want nil", dir, err)
	}

	// The installer runs `mkdir -p`, so a directory that does not exist yet is
	// answered by the nearest ancestor that does.
	if err := checkWritable(filepath.Join(dir, "packages", "standalone", "releases")); err != nil {
		t.Errorf("checkWritable(missing nested dir) = %v, want nil", err)
	}

	if err := checkWritable(""); err == nil {
		t.Error("checkWritable(\"\") = nil, want an error")
	}

	file := filepath.Join(dir, "codex")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkWritable(file); err == nil {
		t.Error("checkWritable(regular file) = nil, want an error")
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny a write")
	}
	readonly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := checkWritable(readonly); err == nil {
		t.Error("checkWritable(read-only dir) = nil, want an error")
	}
}

func TestCheckWritableLeavesNothingBehind(t *testing.T) {
	// A probe file left in a release directory is something a later
	// DetectInstall could trip over.
	dir := t.TempDir()
	if err := checkWritable(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("probe left %d entries behind", len(entries))
	}
}

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{fn: func(s string) { got = append(got, s) }}

	// A progress indicator that redraws in place separates with "\r".
	if _, err := w.Write([]byte("Downloading\rDownloading 50%\rDone\nInstalling")); err != nil {
		t.Fatal(err)
	}
	w.flush()

	want := []string{"Downloading", "Downloading 50%", "Done", "Installing"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %v, want %v", got, want)
	}
}

func TestLineWriterSplitsAcrossWrites(t *testing.T) {
	var got []string
	w := &lineWriter{fn: func(s string) { got = append(got, s) }}
	for _, chunk := range []string{"Resolved ", "version: 0.1", "49.1\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	if len(got) != 1 || got[0] != "Resolved version: 0.149.1" {
		t.Errorf("lines = %v, want one reassembled line", got)
	}
}

func TestLineRingKeepsTheTail(t *testing.T) {
	r := &lineRing{max: 3}
	for i := 0; i < 10; i++ {
		r.add(fmt.Sprintf("line %d", i))
	}
	want := []string{"line 7", "line 8", "line 9"}
	if strings.Join(r.lines(), "|") != strings.Join(want, "|") {
		t.Errorf("lines = %v, want %v", r.lines(), want)
	}
}

func TestLastOutputLine(t *testing.T) {
	if got := lastOutputLine("Downloading\nfailed: no such host\n\n"); got != "failed: no such host" {
		t.Errorf("lastOutputLine = %q", got)
	}
	if got := lastOutputLine(""); got != "" {
		t.Errorf("lastOutputLine(\"\") = %q, want empty", got)
	}
}

func TestSelfManaged(t *testing.T) {
	// codex will also shell out to npm/pnpm/bun for a node-managed install,
	// but this package deliberately does not drive that. See Update's doc.
	for _, m := range []InstallMethod{InstallNPMGlobal, InstallPackageManager, InstallVersionManager, InstallUnknown} {
		if selfManaged(m) {
			t.Errorf("selfManaged(%q) = true", m)
		}
	}
	if !selfManaged(InstallNative) {
		t.Error("selfManaged(native) = false")
	}
}

// fakeCodexScript writes a shell script standing in for the codex CLI, so the
// real exec path can be exercised on a machine with no codex installed.
func fakeCodexScript(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in is POSIX only")
	}
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecUpdate(t *testing.T) {
	// The subcommand, the interleaved streams, the carriage-return progress
	// line and the exit code, through os/exec rather than a stub.
	script := fakeCodexScript(t, `
echo "subcommand=$1"
printf 'Downloading\rDownloading 100%%\n'
echo "to stderr" >&2
exit 3
`)

	var got []string
	code, err := execUpdate(context.Background(), script, os.Environ(), "",
		func(line string) { got = append(got, line) })
	if err == nil {
		t.Fatal("err = nil, want the non-zero exit reported")
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	joined := strings.Join(got, "|")
	for _, want := range []string{"subcommand=update", "Downloading", "Downloading 100%", "to stderr"} {
		if !strings.Contains(joined, want) {
			t.Errorf("lines = %v, want one of them to be %q", got, want)
		}
	}
}

func TestExecUpdateCancellationInterrupts(t *testing.T) {
	// A cancelled run is signalled rather than killed outright, so the
	// installer gets to remove its staging directory.
	script := fakeCodexScript(t, `
trap 'echo interrupted; exit 130' INT
echo started
sleep 30
`)

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	done := make(chan struct{})

	go func() {
		defer close(done)
		_, _ = execUpdate(ctx, script, os.Environ(), "", func(line string) {
			if strings.Contains(line, "started") {
				once.Do(func() { close(started) })
			}
		})
	}()

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("the stand-in never started")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("execUpdate did not return after cancellation")
	}
}
