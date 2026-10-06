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
// directory symlinked at the prefix's bin the way fnm_multishells is. The
// shell directory goes first on PATH; the prefix's own npm exists, so the
// proof never looks npm up there. systemNPM and withoutPrefixNPM turn it into
// the layout where it does.
//
// The fake scripts use shell builtins only, so a test can hand the client a
// PATH without /usr/bin and /bin, which hold this machine's real npm.
type npmFixture struct {
	root     string
	prefix   string // <tmp>/node/installation
	shellBin string // <tmp>/multishell/bin → prefix/bin
	state    string // file the fake codex reads its version from
	npmLog   string // file the fake npm appends its argv to
	latest   string
	writes   bool
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
		root:   root,
		prefix: filepath.Join(root, "node", "installation"),
		state:  filepath.Join(root, "version"),
		npmLog: filepath.Join(root, "npm.log"),
		latest: latest,
		writes: writes,
	}
	pkg := filepath.Join(f.prefix, "lib", "node_modules", "@openai", "codex")
	bin := filepath.Join(f.prefix, "bin")
	for _, d := range []string{filepath.Join(pkg, "bin"), bin, filepath.Join(root, "multishell")} {
		mustMkdir(t, d)
	}
	mustWrite(t, filepath.Join(pkg, "package.json"), cliPackageJSON, 0o644)
	mustWrite(t, f.state, installed+"\n", 0o644)
	mustWrite(t, filepath.Join(pkg, "bin", "codex.js"),
		"#!/bin/sh\nread v < '"+f.state+"'\necho \"codex-cli $v\"\n", 0o755)
	mustWrite(t, filepath.Join(bin, "node"), "#!/bin/sh\nexit 0\n", 0o755)
	mustWrite(t, filepath.Join(bin, "npm"), "#!/bin/sh\n"+f.npmBody(npmPrefix(f), f.npmLog), 0o755)
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

// npmBody is the fake npm: it logs its argv, answers `prefix` and `view`, and
// for `install` reports the node it ran under and optionally moves the version.
func (f *npmFixture) npmBody(prefix, log string) string {
	install := `echo "added 1 package in 1s"`
	if f.writes {
		install += "\necho " + f.latest + " > '" + f.state + "'"
	}
	return `echo "$*" >> '` + log + `'
case "$1" in
prefix) echo '` + prefix + `' ;;
view) echo ` + f.latest + ` ;;
install)
  echo "node=${FAKE_NODE:-$(command -v node)}"
  ` + install + `
  ;;
esac
`
}

// withoutPrefixNPM removes the prefix's own npm and node, leaving the layout
// of a system node with a user-level prefix set in .npmrc.
func (f *npmFixture) withoutPrefixNPM(t *testing.T) {
	t.Helper()
	for _, name := range []string{"npm", "node"} {
		if err := os.Remove(filepath.Join(f.prefix, "bin", name)); err != nil {
			t.Fatal(err)
		}
	}
}

// systemNPM lays out a system node outside the prefix, the way a distro
// package does: `<sys>/bin/npm` → `../lib/node_modules/npm/bin/npm-cli.js`
// with a `#!/usr/bin/env node` line, and a `<sys>/bin/node` that runs the
// script and says which node it was. It returns `<sys>/bin`; its npm logs to
// its own file.
func (f *npmFixture) systemNPM(t *testing.T, name, prefix string) (binDir, log string) {
	t.Helper()
	sys := filepath.Join(f.root, name)
	npmPkg := filepath.Join(sys, "lib", "node_modules", "npm")
	binDir = filepath.Join(sys, "bin")
	log = filepath.Join(f.root, name+"-npm.log")
	mustMkdir(t, filepath.Join(npmPkg, "bin"))
	mustMkdir(t, binDir)
	mustWrite(t, filepath.Join(npmPkg, "package.json"), `{"name":"npm","version":"11.0.0"}`, 0o644)
	mustWrite(t, filepath.Join(npmPkg, "bin", "npm-cli.js"), "#!/usr/bin/env node\n"+f.npmBody(prefix, log), 0o755)
	node := filepath.Join(binDir, "node")
	mustWrite(t, node, "#!/bin/sh\nFAKE_NODE='"+node+"' exec /bin/sh \"$@\"\n", 0o755)
	if err := os.Symlink("../lib/node_modules/npm/bin/npm-cli.js", filepath.Join(binDir, "npm")); err != nil {
		t.Fatal(err)
	}
	return binDir, log
}

func readCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (f *npmFixture) npmCalls(t *testing.T) []string { return readCalls(t, f.npmLog) }

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

// noNPMDir is a PATH entry holding nothing, so a test PATH can leave out
// /usr/bin and /bin, where this machine's real npm lives.
func noNPMDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "empty")
	mustMkdir(t, dir)
	return dir
}

func setPath(t *testing.T, dirs ...string) {
	t.Helper()
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
}

func TestNPMUpdate_SystemNPMMatchUpdates(t *testing.T) {
	// A system node and npm, with the package in a user-level prefix .npmrc
	// points npm at. The prefix has no npm of its own.
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	f.withoutPrefixNPM(t)
	sysBin, sysLog := f.systemNPM(t, "usr", f.prefix)
	setPath(t, f.shellBin, sysBin, noNPMDir(t))

	if info := detectSelfManaged(t); !info.SelfManaged {
		t.Error("SelfManaged = false for a system npm whose prefix holds the package PATH runs")
	}

	var progress []string
	result, err := New().Update(context.Background(),
		WithUpdateProgress(func(line string) { progress = append(progress, line) }))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !result.Changed || result.VersionAfter != "0.155.0" {
		t.Errorf("result = %s → %s (Changed %v), want 0.154.0 → 0.155.0", result.VersionBefore, result.VersionAfter, result.Changed)
	}
	// The PATH entry found once, not its npm-cli.js target and not a bare "npm".
	if want := filepath.Join(sysBin, "npm"); result.Updater != want {
		t.Errorf("Updater = %q, want %q", result.Updater, want)
	}
	joined := strings.Join(progress, "|")
	if !strings.Contains(joined, "node="+filepath.Join(sysBin, "node")) {
		t.Errorf("progress = %v, want npm to run under the node beside it", progress)
	}
	if !strings.Contains(joined, "into "+f.prefix) {
		t.Errorf("progress = %v, want the owning prefix named", progress)
	}
	// The prefix the proof saw is pinned on the install, for the PATH npm only.
	calls := readCalls(t, sysLog)
	if want := "install --global --prefix " + f.prefix + " @openai/codex@0.155.0"; len(calls) == 0 || calls[len(calls)-1] != want {
		t.Errorf("system npm calls = %v, want %q last", calls, want)
	}
}

func TestNPMUpdate_SystemNPMUnwritablePrefixIsBlocked(t *testing.T) {
	// The preflight probes the prefix that owns the package, not the
	// directory the npm binary sits in.
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny a write")
	}
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	f.withoutPrefixNPM(t)
	sysBin, sysLog := f.systemNPM(t, "usr", f.prefix)
	setPath(t, f.shellBin, sysBin, noNPMDir(t))
	prefixBin := filepath.Join(f.prefix, "bin")
	if err := os.Chmod(prefixBin, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(prefixBin, 0o755) })

	if info := detectSelfManaged(t); info.SelfManaged {
		t.Error("SelfManaged = true although the prefix's bin is not writable")
	}
	_, err := New().Update(context.Background())
	var blocked *UpdateNotWritableError
	if !errors.As(err, &blocked) || blocked.Dir != prefixBin {
		t.Fatalf("err = %v, want *UpdateNotWritableError on %s", err, prefixBin)
	}
	for _, call := range readCalls(t, sysLog) {
		if !strings.HasPrefix(call, "prefix") {
			t.Errorf("ran npm %q after the preflight refused", call)
		}
	}
}

func TestNPMUpdate_SystemNPMMismatchIsManual(t *testing.T) {
	// The system npm's configured prefix is not the one PATH runs codex from.
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	f.withoutPrefixNPM(t)
	other := filepath.Join(f.root, "elsewhere")
	otherPkg := filepath.Join(other, "lib", "node_modules", "@openai", "codex")
	mustMkdir(t, otherPkg)
	mustWrite(t, filepath.Join(otherPkg, "package.json"), cliPackageJSON, 0o644)
	sysBin, sysLog := f.systemNPM(t, "usr", other)
	setPath(t, f.shellBin, sysBin, noNPMDir(t))

	if info := detectSelfManaged(t); info.SelfManaged {
		t.Error("SelfManaged = true although the system npm writes a different prefix")
	}
	_, err := New().Update(context.Background())
	var manual *ManualUpdateError
	if !errors.As(err, &manual) {
		t.Fatalf("err = %v, want *ManualUpdateError", err)
	}
	if !strings.Contains(manual.Reason, "PATH runs") {
		t.Errorf("Reason = %q, want the mismatch named", manual.Reason)
	}
	for _, call := range readCalls(t, sysLog) {
		if !strings.HasPrefix(call, "prefix") {
			t.Errorf("ran npm %q after the prefix proof failed", call)
		}
	}
}

func TestNPMUpdate_NoNPMReachableIsManual(t *testing.T) {
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	f.withoutPrefixNPM(t)
	setPath(t, f.shellBin, noNPMDir(t))

	if info := detectSelfManaged(t); info.SelfManaged {
		t.Error("SelfManaged = true with no npm anywhere")
	}
	_, err := New().Update(context.Background())
	var manual *ManualUpdateError
	if !errors.As(err, &manual) {
		t.Fatalf("err = %v, want *ManualUpdateError", err)
	}
	if !strings.Contains(manual.Reason, "no npm on PATH") {
		t.Errorf("Reason = %q, want the missing npm named", manual.Reason)
	}
}

func TestNPMUpdate_PrefixNPMWinsOverPATH(t *testing.T) {
	// Both exist: the prefix's own npm is the one proven and run, and the npm
	// first on PATH is never touched.
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	sysBin, sysLog := f.systemNPM(t, "usr", f.prefix)
	setPath(t, sysBin, f.shellBin, noNPMDir(t))

	if info := detectSelfManaged(t); !info.SelfManaged {
		t.Error("SelfManaged = false for a proven prefix npm")
	}
	result, err := New().Update(context.Background())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if want := filepath.Join(f.prefix, "bin", "npm"); result.Updater != want {
		t.Errorf("Updater = %q, want the prefix's own npm %q", result.Updater, want)
	}
	if calls := readCalls(t, sysLog); len(calls) != 0 {
		t.Errorf("PATH npm ran %v, want it untouched", calls)
	}
}

func TestNPMUpdate_PrefixNPMWithoutNodeStaysManual(t *testing.T) {
	// Something sits at <prefix>/bin/npm, so that npm is the only candidate,
	// exactly as before: a missing node beside it is a refusal, not a reason
	// to try PATH.
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	if err := os.Remove(filepath.Join(f.prefix, "bin", "node")); err != nil {
		t.Fatal(err)
	}
	sysBin, sysLog := f.systemNPM(t, "usr", f.prefix)
	setPath(t, sysBin, f.shellBin, noNPMDir(t))

	if _, err := New().Update(context.Background()); !errors.Is(err, ErrManualUpdate) {
		t.Fatalf("err = %v, want ErrManualUpdate", err)
	}
	if calls := readCalls(t, sysLog); len(calls) != 0 {
		t.Errorf("PATH npm ran %v, want it untouched", calls)
	}
}

func TestNPMUpdate_PATHShimIsManual(t *testing.T) {
	// An `npm` on PATH that is not npm's own npm-cli.js can answer
	// `prefix -g` one way and install another (Volta intercepts `install -g`).
	// This one even runs under node, like corepack's shim. It is refused
	// before it runs at all.
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	f.withoutPrefixNPM(t)
	sysBin, sysLog := f.systemNPM(t, "usr", f.prefix)
	shimPkg := filepath.Join(f.root, "dispatcher", "lib", "node_modules", "dispatcher")
	shims := filepath.Join(f.root, "dispatcher", "bin")
	shimLog := filepath.Join(f.root, "shim.log")
	mustMkdir(t, filepath.Join(shimPkg, "shims"))
	mustMkdir(t, shims)
	mustWrite(t, filepath.Join(shimPkg, "package.json"), `{"name":"dispatcher"}`, 0o644)
	mustWrite(t, filepath.Join(shimPkg, "shims", "npm"), "#!/usr/bin/env node\n"+f.npmBody(f.prefix, shimLog), 0o755)
	if err := os.Symlink("../lib/node_modules/dispatcher/shims/npm", filepath.Join(shims, "npm")); err != nil {
		t.Fatal(err)
	}
	setPath(t, f.shellBin, shims, sysBin, noNPMDir(t))

	if info := detectSelfManaged(t); info.SelfManaged {
		t.Error("SelfManaged = true for an npm shim")
	}
	_, err := New().Update(context.Background())
	var manual *ManualUpdateError
	if !errors.As(err, &manual) {
		t.Fatalf("err = %v, want *ManualUpdateError", err)
	}
	if !strings.Contains(manual.Reason, "npm-cli.js") {
		t.Errorf("Reason = %q, want the shim named", manual.Reason)
	}
	// Neither the shim nor the real npm behind it on PATH is run: the one
	// lookup found the shim, and there is no second.
	if calls := append(readCalls(t, shimLog), readCalls(t, sysLog)...); len(calls) != 0 {
		t.Errorf("npm ran %v, want nothing run", calls)
	}
}

func TestNPMUpdate_SystemNPMRunsUnderTheCheckedNode(t *testing.T) {
	// A relative PATH entry ahead of the system node holds another node.
	// The lookup skips it, so `env node` must be made to skip it too.
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	f.withoutPrefixNPM(t)
	sysBin, _ := f.systemNPM(t, "usr", f.prefix)
	decoy := filepath.Join(f.root, "decoy")
	mustMkdir(t, decoy)
	mustWrite(t, filepath.Join(decoy, "node"), "#!/bin/sh\nFAKE_NODE=decoy exec /bin/sh \"$@\"\n", 0o755)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(f.root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	setPath(t, f.shellBin, "decoy", sysBin, noNPMDir(t))

	var progress []string
	if _, err := New().Update(context.Background(),
		WithUpdateProgress(func(line string) { progress = append(progress, line) })); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if joined := strings.Join(progress, "|"); !strings.Contains(joined, "node="+filepath.Join(sysBin, "node")) {
		t.Errorf("progress = %v, want npm under the checked node, not the relative decoy", progress)
	}
}

func TestNPMUpdate_LooksUpNPMOnTheChildPATH(t *testing.T) {
	// This process's PATH reaches no npm; the PATH the client hands its
	// subprocesses does. The lookup follows the child.
	f := newNPMFixture(t, "0.154.0", "0.155.0", ownPrefix, true)
	f.withoutPrefixNPM(t)
	sysBin, _ := f.systemNPM(t, "usr", f.prefix)
	empty := noNPMDir(t)
	setPath(t, f.shellBin, empty)
	client := New(WithEnv(map[string]string{"PATH": strings.Join([]string{sysBin, empty}, string(os.PathListSeparator))}))

	info, err := client.DetectInstall(context.Background())
	if err != nil {
		t.Fatalf("DetectInstall: %v", err)
	}
	if !info.SelfManaged {
		t.Error("SelfManaged = false although the child PATH reaches a proven npm")
	}
	if detectSelfManaged(t).SelfManaged {
		t.Error("SelfManaged = true for a client whose child PATH reaches no npm")
	}
	result, err := client.Update(context.Background())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if want := filepath.Join(sysBin, "npm"); result.Updater != want || !result.Changed {
		t.Errorf("Updater = %q Changed = %v, want %q and a change", result.Updater, result.Changed, want)
	}
}

func TestShebangNode(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	mustWrite(t, node, "", 0o755)
	tests := []struct {
		line    string
		want    string
		wantErr bool
	}{
		{line: "#!/usr/bin/env node\nrest", want: node},
		{line: "#!/opt/node/bin/node", want: "/opt/node/bin/node"},
		{line: "#!/usr/bin/nodejs\n", want: "/usr/bin/nodejs"},
		{line: "#!/bin/sh\n", wantErr: true},
		{line: "#!/usr/bin/env -S node --x\n", wantErr: true},
		{line: "console.log(1)\n", wantErr: true},
	}
	for _, tt := range tests {
		got, err := shebangNode([]byte(tt.line), "relative"+string(os.PathListSeparator)+dir)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("shebangNode(%q) = %q, %v; want %q, err %v", tt.line, got, err, tt.want, tt.wantErr)
		}
	}
	if _, err := shebangNode([]byte("#!/usr/bin/env node"), ""); err == nil {
		t.Error("shebangNode with an empty PATH found a node")
	}
}

func TestLookPathInSkipsRelativeEntries(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "npm"), "", 0o755)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	sep := string(os.PathListSeparator)
	for _, p := range []string{".", "", sep + "." + sep} {
		if got, err := lookPathIn("npm", p); err == nil {
			t.Errorf("lookPathIn(%q) = %q, want no match from a relative entry", p, got)
		}
	}
	if got, err := lookPathIn("npm", "."+sep+dir); err != nil || got != filepath.Join(dir, "npm") {
		t.Errorf("lookPathIn = %q, %v", got, err)
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
