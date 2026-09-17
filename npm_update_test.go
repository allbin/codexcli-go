package codexcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// npmFixture is a real npm-global layout in a temp dir: a node prefix holding
// the CLI package, fake `node`/`npm` scripts beside it, and a per-shell bin
// directory symlinked at the prefix's bin the way fnm_multishells is. Only the
// shell directory goes on PATH, so nothing can find npm by lookup.
type npmFixture struct {
	prefix   string // <tmp>/node/installation
	shellBin string // <tmp>/multishell/bin → prefix/bin
	state    string // file the fake codex reads its version from
	npmLog   string // file the fake npm appends its argv to
}

// newNPMFixture builds the layout. npmPrefix is what the fake `npm prefix -g`
// prints; latest is what `npm view` prints; writes decides whether the fake
// `npm install` actually changes the installed version.
func newNPMFixture(t *testing.T, installed, latest string, npmPrefix func(f *npmFixture) string, writes bool) *npmFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("npm update is unix only")
	}
	root := t.TempDir()
	f := &npmFixture{
		prefix: filepath.Join(root, "node", "installation"),
		state:  filepath.Join(root, "version"),
		npmLog: filepath.Join(root, "npm.log"),
	}
	pkg := filepath.Join(f.prefix, "lib", "node_modules", "@openai", "codex")
	bin := filepath.Join(f.prefix, "bin")
	for _, d := range []string{filepath.Join(pkg, "bin"), bin, filepath.Join(root, "multishell")} {
		mustMkdir(t, d)
	}
	mustWrite(t, filepath.Join(pkg, "package.json"), cliPackageJSON, 0o644)
	mustWrite(t, f.state, installed+"\n", 0o644)
	mustWrite(t, filepath.Join(pkg, "bin", "codex.js"),
		"#!/bin/sh\necho \"codex-cli $(cat '"+f.state+"')\"\n", 0o755)
	mustWrite(t, filepath.Join(bin, "node"), "#!/bin/sh\nexit 0\n", 0o755)

	install := `echo "added 1 package in 1s"`
	if writes {
		install += "\necho " + latest + " > '" + f.state + "'"
	}
	mustWrite(t, filepath.Join(bin, "npm"), `#!/bin/sh
echo "$*" >> '`+f.npmLog+`'
case "$1" in
prefix) echo '`+npmPrefix(f)+`' ;;
view) echo `+latest+` ;;
install)
  echo "node=$(command -v node)"
  `+install+`
  ;;
esac
`, 0o755)
	if err := os.Symlink("../lib/node_modules/@openai/codex/bin/codex.js", filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	f.shellBin = filepath.Join(root, "multishell", "bin")
	if err := os.Symlink(bin, f.shellBin); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", f.shellBin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	return f
}

func (f *npmFixture) npmCalls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.npmLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, name, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func ownPrefix(f *npmFixture) string { return f.prefix }

// detectSelfManaged runs the public detection over the fixture, so the verdict
// under test is the one a consumer keys its button on.
func detectSelfManaged(t *testing.T) *InstallInfo {
	t.Helper()
	info, err := New().DetectInstall(context.Background())
	if err != nil {
		t.Fatalf("DetectInstall: %v", err)
	}
	if info.Method != InstallNPMGlobal || info.PackageManager != "npm" {
		t.Fatalf("detected %s/%s, want npm-global/npm", info.Method, info.PackageManager)
	}
	return info
}

func TestNPMUpdate_PrefixMatchUpdates(t *testing.T) {
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)

	if info := detectSelfManaged(t); !info.SelfManaged {
		t.Error("SelfManaged = false for a proven, writable npm prefix")
	}

	var progress []string
	result, err := New().Update(context.Background(),
		WithUpdateProgress(func(line string) { progress = append(progress, line) }))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !result.Changed || result.VersionBefore != "0.154.0" || result.VersionAfter != "0.155.0" {
		t.Errorf("result = %s → %s (Changed %v), want 0.154.0 → 0.155.0", result.VersionBefore, result.VersionAfter, result.Changed)
	}
	if result.Method != InstallNPMGlobal {
		t.Errorf("Method = %q", result.Method)
	}
	wantNPM := filepath.Join(f.prefix, "bin", "npm")
	if result.Updater != wantNPM {
		t.Errorf("Updater = %q, want the prefix's own npm %q", result.Updater, wantNPM)
	}
	if result.Path != filepath.Join(f.shellBin, "codex") {
		t.Errorf("Path = %q, want the PATH entry", result.Path)
	}

	calls := f.npmCalls(t)
	if last := calls[len(calls)-1]; last != "install --global @openai/codex@0.155.0" {
		t.Errorf("last npm call = %q, want the pinned install", last)
	}
	joined := strings.Join(progress, "|")
	if !strings.Contains(joined, "added 1 package") {
		t.Errorf("progress = %v, want npm's output streamed", progress)
	}
	// npm is `#!/usr/bin/env node`: the prefix's own node has to win.
	if !strings.Contains(joined, "node="+filepath.Join(f.prefix, "bin", "node")) {
		t.Errorf("progress = %v, want npm to run under the prefix's node", progress)
	}
}

func TestNPMUpdate_AlreadyLatestRunsNothing(t *testing.T) {
	f := newNPMFixture(t, "0.155.0", "0.155.0", ownPrefix, true)

	result, err := New().Update(context.Background())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.Changed || result.VersionAfter != "0.155.0" {
		t.Errorf("result = %+v, want unchanged at 0.155.0", result)
	}
	for _, call := range f.npmCalls(t) {
		if strings.HasPrefix(call, "install") {
			t.Errorf("ran %q for an install already at latest", call)
		}
	}
}

func TestNPMUpdate_PrefixMismatchIsManual(t *testing.T) {
	// npm's configured prefix holds a second copy. Installing there would
	// leave the copy PATH runs stale.
	var other string
	f := newNPMFixture(t, "0.154.0", "0.155.0", func(f *npmFixture) string {
		other = filepath.Join(filepath.Dir(f.prefix), "..", "npm-global")
		return other
	}, true)
	otherPkg := filepath.Join(other, "lib", "node_modules", "@openai", "codex")
	mustMkdir(t, otherPkg)
	mustWrite(t, filepath.Join(otherPkg, "package.json"), cliPackageJSON, 0o644)

	if info := detectSelfManaged(t); info.SelfManaged {
		t.Error("SelfManaged = true although npm writes a different prefix")
	}

	result, err := New().Update(context.Background())
	var manual *ManualUpdateError
	if !errors.As(err, &manual) {
		t.Fatalf("err = %v, want *ManualUpdateError", err)
	}
	if manual.Command != "npm install -g @openai/codex@latest" {
		t.Errorf("Command = %q", manual.Command)
	}
	if !strings.Contains(manual.Reason, "PATH runs") {
		t.Errorf("Reason = %q, want the mismatch named", manual.Reason)
	}
	if result != nil {
		t.Errorf("result = %+v, want nil", result)
	}
	for _, call := range f.npmCalls(t) {
		if !strings.HasPrefix(call, "prefix") {
			t.Errorf("ran npm %q after the prefix proof failed", call)
		}
	}
}

func TestNPMUpdate_NoPackageAtReportedPrefixIsManual(t *testing.T) {
	newNPMFixture(t, "0.154.0", "0.155.0", func(*npmFixture) string { return "/nonexistent/prefix" }, true)
	if _, err := New().Update(context.Background()); !errors.Is(err, ErrManualUpdate) {
		t.Fatalf("err = %v, want ErrManualUpdate", err)
	}
}

func TestNPMUpdate_UnwritablePrefixIsBlocked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny a write")
	}
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	nodeModules := filepath.Join(f.prefix, "lib", "node_modules")
	if err := os.Chmod(nodeModules, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nodeModules, 0o755) })

	if info := detectSelfManaged(t); info.SelfManaged {
		t.Error("SelfManaged = true for an unwritable npm prefix")
	}

	result, err := New().Update(context.Background())
	var blocked *UpdateNotWritableError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want *UpdateNotWritableError", err)
	}
	if blocked.Dir != nodeModules {
		t.Errorf("Dir = %q, want %q", blocked.Dir, nodeModules)
	}
	if result != nil {
		t.Errorf("result = %+v, want nil when nothing ran", result)
	}
	for _, call := range f.npmCalls(t) {
		if strings.HasPrefix(call, "install") || strings.HasPrefix(call, "view") {
			t.Errorf("ran npm %q after the preflight refused", call)
		}
	}
}

func TestNPMUpdate_CleanExitWithoutNewVersionFails(t *testing.T) {
	// npm exits 0, but the binary PATH runs still reports the old version.
	newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, false)

	result, err := New().Update(context.Background())
	if !errors.Is(err, ErrUpdateFailed) {
		t.Fatalf("err = %v, want ErrUpdateFailed", err)
	}
	var failed *UpdateFailedError
	if !errors.As(err, &failed) || failed.ExitCode != 0 {
		t.Fatalf("err = %#v, want an exit-0 *UpdateFailedError", err)
	}
	if !strings.Contains(err.Error(), "0.155.0") {
		t.Errorf("Error() = %q, want the version npm was told to install", err.Error())
	}
	if result == nil || result.Changed || result.VersionAfter != "0.154.0" {
		t.Errorf("result = %+v, want unchanged numbers alongside the error", result)
	}
}

func TestWithPathPrefix(t *testing.T) {
	sep := string(os.PathListSeparator)
	in := map[string]string{"PATH": "/a" + sep + "/b", "X": "1"}
	got := withPathPrefix(in, "/prefix/bin")
	if got["PATH"] != "/prefix/bin"+sep+"/a"+sep+"/b" || got["X"] != "1" {
		t.Errorf("withPathPrefix = %v", got)
	}
	if in["PATH"] != "/a"+sep+"/b" {
		t.Error("withPathPrefix mutated its input")
	}
	t.Setenv("PATH", "/proc/path")
	if got := withPathPrefix(nil, "/p"); got["PATH"] != "/p"+sep+"/proc/path" {
		t.Errorf("withPathPrefix(nil) = %v, want this process's PATH appended", got)
	}
}
