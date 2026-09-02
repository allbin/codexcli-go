package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// CLIPackageName is the npm package that publishes the codex CLI. It is the
// package a consumer would query for the latest published version, and the one
// named by the npm/pnpm/bun update commands in InstallInfo.UpdateCmd.
//
// The platform sub-packages (`@openai/codex-linux-x64` and friends) publish
// under this same `name` — they are npm aliases of it — so matching on it
// identifies both the JS wrapper and the vendored native binary.
const CLIPackageName = "@openai/codex"

// InstallMethod describes how the codex CLI on PATH was installed.
//
// The zero value is the empty string, which is not a valid method — use
// InstallUnknown for "could not be determined".
type InstallMethod string

const (
	// InstallNPMGlobal is a global npm, pnpm or bun install: the resolved
	// binary lives under a `node_modules/@openai/codex` tree owned by a
	// package manager prefix. See InstallInfo.PackageManager for which of the
	// three owns it — their update commands differ.
	InstallNPMGlobal InstallMethod = "npm-global"

	// InstallVersionManager is a binary living inside a tool-version-manager
	// root (fnm, nvm, volta, asdf, mise) with no package metadata explaining
	// how it got there. The version manager owns the directory, so no generic
	// update command is safe to suggest.
	InstallVersionManager InstallMethod = "version-manager"

	// InstallPackageManager is an OS package manager install (Homebrew cask,
	// winget, mise, asdf). See InstallInfo.PackageManager for which one.
	InstallPackageManager InstallMethod = "package-manager"

	// InstallNative is codex's own standalone installer layout: releases
	// unpacked under `$CODEX_HOME/packages/standalone/releases/<version>` with
	// a `current` symlink and a thin symlink on PATH. These update themselves
	// via `codex update`, which re-runs the standalone installer.
	//
	// Unlike claudecli-go, a bare compiled executable sitting in an ordinary
	// bin directory is *not* native here. codex's standalone installer always
	// unpacks into the layout above, so an executable outside it was put there
	// by something codex does not recognise — and `codex update` refuses it
	// outright ("Could not detect the Codex installation method. Please update
	// manually"). Such a binary is reported as InstallUnknown. See the
	// verification table on DetectInstall.
	InstallNative InstallMethod = "native"

	// InstallUnknown means detection found no conclusive evidence. This is a
	// legitimate, useful answer: InstallInfo.UpdateCmd is empty and the caller
	// should tell the user to update manually rather than run a guess.
	InstallUnknown InstallMethod = "unknown"
)

// InstallSource records which evidence produced InstallInfo.Method, so a
// caller can weigh how much to trust it.
type InstallSource string

const (
	// InstallSourcePackageMetadata means a package.json naming CLIPackageName
	// was found by walking up from the resolved binary. This is the strongest
	// signal: the packaging metadata describes the binary that actually runs,
	// and both the JS wrapper on PATH and the vendored native binary several
	// directories below it walk up to it.
	InstallSourcePackageMetadata InstallSource = "package-metadata"

	// InstallSourcePathLayout means the resolved path matched a known install
	// layout (a node_modules tree, the standalone installer's release tree, a
	// Homebrew Caskroom, a WinGet package dir, a version-manager root).
	InstallSourcePathLayout InstallSource = "path-layout"

	// InstallSourceConfig means path evidence was inconclusive and the method
	// came from codex's own on-disk record under CODEX_HOME — specifically the
	// `packages/standalone/current` symlink the standalone installer maintains.
	// That record describes what was last *installed* into CODEX_HOME, which
	// need not describe the binary currently first on PATH, so it is treated as
	// weaker than the other two.
	InstallSourceConfig InstallSource = "config"

	// InstallSourceNone means nothing conclusive was found.
	InstallSourceNone InstallSource = "none"
)

// InstallInfo describes the codex CLI installation currently first on PATH.
type InstallInfo struct {
	// Path is the binary as found on PATH, before symlinks are resolved.
	Path string

	// RealPath is Path with all symlinks resolved. Classification is driven by
	// this, not Path: an npm or version-manager shim only reveals what it is
	// once resolved.
	//
	// Note this is still the JS wrapper for an npm install — codex's own
	// "current executable" is the vendored native binary the wrapper spawns,
	// several directories deeper. Both walk up to the same package.json, so
	// the classification is the same either way; Doctor reports the deeper
	// path if a caller needs it.
	RealPath string

	// Version is what the CLI reports for itself (e.g. "0.148.0"), or "" if the
	// version probe failed. Detection does not fail just because the version
	// could not be read.
	Version string

	// Method is how the binary at RealPath was installed.
	Method InstallMethod

	// UpdateCmd is the command to show the user, or "" when no command is
	// known to be correct. Never treat "" as "use npm" — see the doc on
	// DetectInstall for why guessing is worse than saying nothing.
	UpdateCmd string

	// VersionManager names the tool version manager whose directory the binary
	// lives under ("fnm", "nvm", "volta", "asdf", "mise"), or "" if none. It is
	// set even when Method is InstallNPMGlobal: a global npm install hosted by
	// a node version manager only updates for the node version currently
	// active, which is worth telling the user.
	VersionManager string

	// PackageManager names the packaging system that owns this install:
	// "npm", "pnpm" or "bun" when Method is InstallNPMGlobal; "homebrew",
	// "winget", "mise" or "asdf" when Method is InstallPackageManager; ""
	// otherwise.
	PackageManager string

	// PackageName is the identifier that package manager knows this install by
	// — CLIPackageName for the node package managers, a Homebrew cask name, a
	// winget package id — or "" when not applicable.
	PackageName string

	// ConfigMethod is codex's own on-disk record of a self-managed install:
	// "standalone" when `$CODEX_HOME/packages/standalone/current` resolves,
	// else "". codex, unlike Claude Code, does not record an install method in
	// its config file, so this symlink is the only such record it keeps.
	ConfigMethod string

	// ConfigMismatch is true when codex's CODEX_HOME holds a standalone
	// install that PATH does not reach. That means a second copy was installed
	// by a different route and now shadows the standalone one — the exact
	// situation that makes a wrong update command destructive, because
	// updating the copy you found leaves the copy that runs untouched.
	ConfigMismatch bool

	// Source records which evidence produced Method.
	Source InstallSource
}

// ErrCLINotFound matches the error returned by DetectInstall when no codex CLI
// is on PATH. Use errors.Is to distinguish "no CLI installed" — a normal state
// for a consumer probing the environment — from a real failure.
var ErrCLINotFound = errors.New("codex CLI not found on PATH")

// CLINotFoundError is returned when the CLI binary cannot be resolved on PATH.
// Use errors.As to read which binary name was looked up.
type CLINotFoundError struct {
	Binary string // the name or path that was looked up
	Err    error  // the underlying exec.LookPath error
}

func (e *CLINotFoundError) Error() string {
	return fmt.Sprintf("codexcli: %q not found on PATH", e.Binary)
}

func (e *CLINotFoundError) Is(target error) bool {
	return target == ErrCLINotFound
}

func (e *CLINotFoundError) Unwrap() error { return e.Err }

// defaultInstallTimeout bounds the `codex --version` probe when the caller's
// context has no deadline.
const defaultInstallTimeout = 5 * time.Second

// defaultInstallClient backs the package-level DetectInstall so a caller can
// probe the environment without constructing a Client.
var defaultInstallClient = New()

// DetectInstall reports how the codex CLI on PATH was installed and which
// command updates it, using the default client's binary.
//
// # Detect, never assume
//
// The update command must be derived from evidence about the binary that
// actually runs, never from a default. Suggesting the wrong one does not fail
// cleanly: `npm install -g @openai/codex` against a standalone install writes
// a second, complete copy into an npm prefix, and whichever copy PATH happens
// to reach first from then on is the one that answers `codex --version`. The
// user is now told a version that does not describe the binary their next
// session will run, and the copy they actually use is still stale. Because
// that failure is silent, InstallUnknown with an empty UpdateCmd is the
// correct answer whenever the evidence is inconclusive — "update manually"
// costs the user a web search, a wrong command costs them a broken install
// they cannot see.
//
// # What it does
//
// Detection is read-only and offline: it resolves the binary with
// exec.LookPath and filepath.EvalSymlinks, reads package metadata next to the
// resolved path, resolves codex's standalone-install symlink under CODEX_HOME,
// and runs `codex --version`. It starts no session, writes nothing, and makes
// no network calls. Notably it does not shell out to `codex doctor`, which
// reports a superset of these facts but spends ~400ms of network to do it —
// see [Doctor] if you want that report and can pay for it.
//
// Fetching the *published* version (via `npm view`, the GitHub releases API,
// or anything else) is the caller's business — this reports only what is
// installed locally.
//
// # Precedence
//
// Package metadata beats path layout, and path layout beats codex's on-disk
// record. The `packages/standalone/current` symlink under CODEX_HOME says a
// standalone install exists there, which need not describe the binary now
// first on PATH, so it is only used when path evidence is inconclusive (Source
// is then InstallSourceConfig). When PATH does not reach that install at all,
// ConfigMismatch is set.
//
// # Update commands
//
// Verified against codex 0.148.0 by running `codex update` over synthetic
// install layouts in a throwaway container:
//
//	layout                          `codex update` does            UpdateCmd
//	standalone (CODEX_HOME tree)    re-runs the installer          codex update
//	npm-managed                     runs `npm install -g …`        npm install -g @openai/codex@latest
//	pnpm-managed                    runs `pnpm add -g …`           pnpm add -g @openai/codex@latest
//	bun-managed                     runs `bun install -g …`        bun install -g @openai/codex@latest
//	bare binary in a bin dir        refuses, says update manually  "" (reported InstallUnknown)
//
// So `codex update` is the right answer for a standalone install and only for
// a standalone install. For the node package managers the explicit command is
// preferred over `codex update`: it is the same action, but it names the
// prefix being written and works even when the shim is broken.
//
// The Homebrew command (`brew upgrade --cask codex`) and the cask name come
// from codex's own binary. Homebrew and winget layouts are classified from the
// path only — codex 0.148.0 itself reports "other" for them on Linux, and no
// winget package is published by OpenAI at the time of writing; the winget id
// `OpenAI.Codex` comes from the winget community repository, not from codex.
// asdf gets no command for the same reason claudecli-go gives it none: there
// is no single correct one.
//
// # Windows
//
// Windows classification has not been verified on real hardware. npm's
// `node_modules` layout is handled, but a `.cmd`, `.bat`, or `.ps1` shim is
// not a symlink and cannot be resolved further, so unless a sibling npm layout
// confirms the install it is reported as InstallUnknown rather than guessed at.
//
// A missing CLI is not a failure: the returned error satisfies
// errors.Is(err, ErrCLINotFound) and carries no other meaning.
func DetectInstall(ctx context.Context, opts ...Option) (*InstallInfo, error) {
	return defaultInstallClient.DetectInstall(ctx, opts...)
}

// DetectInstall reports how this client's codex CLI was installed. See the
// package-level [DetectInstall] for the full contract.
//
// Options are resolved the same way Client.ListModels resolves them, so
// WithCodexHome (client default or per-call) relocates the CODEX_HOME lookup.
func (c *Client) DetectInstall(ctx context.Context, opts ...Option) (*InstallInfo, error) {
	resolved := resolveOptions(c.defaults, opts)
	info, err := detectInstall(ctx, c.binaryPath(), osInstallEnv(resolved.codexHome))
	if err != nil {
		return nil, err
	}
	c.log().Debug("detect install",
		"path", info.Path, "realPath", info.RealPath, "version", info.Version,
		"method", info.Method, "source", info.Source, "updateCmd", info.UpdateCmd,
		"versionManager", info.VersionManager, "packageManager", info.PackageManager,
		"configMethod", info.ConfigMethod, "configMismatch", info.ConfigMismatch)
	return info, nil
}

// binaryPath returns the CLI binary path from the executor, falling back to
// "codex". A non-local Executor (Docker, SSH) cannot be probed from here, so
// it falls back too.
func (c *Client) binaryPath() string {
	if le, ok := c.executor.(*LocalExecutor); ok && le.BinaryPath != "" {
		return le.BinaryPath
	}
	return "codex"
}

// installEnv is the set of ambient lookups DetectInstall performs, injectable
// so tests can drive classification over synthetic layouts.
type installEnv struct {
	lookPath    func(string) (string, error)
	evalSymlink func(string) (string, error)
	readFile    func(string) ([]byte, error) // small files: package.json, .modules.yaml
	runVersion  func(ctx context.Context, binary string) (string, error)
	codexHome   string // WithCodexHome, else $CODEX_HOME, else ~/.codex
}

func osInstallEnv(codexHomeOverride string) installEnv {
	home, err := resolveCodexHome(codexHomeOverride)
	if err != nil {
		// A missing home directory only costs us the standalone-install
		// record; path classification stands on its own.
		home = ""
	}
	return installEnv{
		lookPath:    exec.LookPath,
		evalSymlink: filepath.EvalSymlinks,
		readFile:    readSmallFile,
		runVersion:  runVersionProbe,
		codexHome:   home,
	}
}

// maxInstallFileSize caps reads of package.json and .modules.yaml so a
// pathological file cannot balloon memory during a probe.
const maxInstallFileSize = 16 << 20

func readSmallFile(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxInstallFileSize))
}

// runVersionProbe runs `<binary> --version` and returns the version token the
// CLI prints (it emits e.g. "codex-cli 0.148.0").
func runVersionProbe(ctx context.Context, binary string) (string, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultInstallTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, binary, "--version")
	hideConsoleWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("codexcli: version probe: %w", err)
	}
	return parseVersionOutput(string(out)), nil
}

// parseVersionOutput extracts the version token from `codex --version` output.
// codex prefixes it with a product name ("codex-cli 0.148.0"), so the version
// is the trailing field, not the leading one. Any prerelease suffix is
// preserved.
func parseVersionOutput(out string) string {
	fields := strings.Fields(out)
	for i := len(fields) - 1; i >= 0; i-- {
		v := strings.TrimPrefix(fields[i], "v")
		if v != "" && v[0] >= '0' && v[0] <= '9' {
			return v
		}
	}
	return ""
}

func detectInstall(ctx context.Context, binary string, env installEnv) (*InstallInfo, error) {
	if binary == "" {
		binary = "codex"
	}
	found, err := env.lookPath(binary)
	if err != nil {
		return nil, &CLINotFoundError{Binary: binary, Err: err}
	}

	real := found
	if resolved, err := env.evalSymlink(found); err == nil && resolved != "" {
		real = resolved
	}

	cls := classifyInstall(real, env)
	info := &InstallInfo{
		Path:           found,
		RealPath:       real,
		Method:         cls.method,
		VersionManager: cls.versionManager,
		PackageManager: cls.packageManager,
		PackageName:    cls.packageName,
		Source:         cls.source,
	}
	applyStandaloneRecord(info, env)

	info.UpdateCmd = updateCommand(info.Method, info.PackageManager, info.PackageName)
	info.Version, _ = env.runVersion(ctx, found)
	return info, nil
}

// applyStandaloneRecord folds in codex's own record of a self-managed install
// — the `packages/standalone/current` symlink the standalone installer keeps
// under CODEX_HOME. It only breaks ties: the record describes what was
// installed into CODEX_HOME, which may be a different copy than the one on
// PATH. That divergence is the point of ConfigMismatch.
func applyStandaloneRecord(info *InstallInfo, env installEnv) {
	current := standaloneCurrent(env)
	if current == "" {
		return
	}
	info.ConfigMethod = "standalone"

	if underDir(normalizeInstallPath(info.RealPath), normalizeInstallPath(current)) {
		if info.Method == InstallUnknown {
			info.Method = InstallNative
			info.Source = InstallSourceConfig
		}
		return
	}
	// A standalone install exists under CODEX_HOME, but PATH reaches a
	// different copy — either another install method entirely, or a stale
	// symlink to a superseded standalone release.
	info.ConfigMismatch = true
}

// standaloneCurrent resolves `$CODEX_HOME/packages/standalone/current` to the
// release directory it points at, or "" when there is no standalone install.
func standaloneCurrent(env installEnv) string {
	if env.codexHome == "" || env.evalSymlink == nil {
		return ""
	}
	resolved, err := env.evalSymlink(filepath.Join(env.codexHome, "packages", "standalone", "current"))
	if err != nil {
		return ""
	}
	return resolved
}

type classification struct {
	method         InstallMethod
	versionManager string
	packageManager string
	packageName    string
	source         InstallSource
}

// classifyInstall derives the install method from a fully symlink-resolved
// path. The order matters: the node package layouts are the most specific and
// carry real metadata, then codex's own standalone tree, then OS package
// managers, and only then the version-manager fallback.
func classifyInstall(realPath string, env installEnv) classification {
	p := normalizeInstallPath(realPath)
	segs := strings.Split(p, "/")
	vm := detectVersionManager(segs)

	if pm, src, ok := npmEvidence(p, env); ok {
		return classification{InstallNPMGlobal, vm, pm, CLIPackageName, src}
	}

	if isStandaloneLayout(p, env.codexHome) {
		return classification{InstallNative, vm, "", "", InstallSourcePathLayout}
	}

	if pm, name, ok := detectOSPackageManager(p, segs); ok {
		return classification{InstallPackageManager, vm, pm, name, InstallSourcePathLayout}
	}

	// A version-manager root with no package metadata explaining how the
	// binary got there: name the manager, but do not invent an update command.
	if vm != "" {
		return classification{InstallVersionManager, vm, "", "", InstallSourcePathLayout}
	}

	// Anything else — including a compiled executable in an ordinary bin dir.
	// codex itself calls that "other" and refuses to update it, so we say
	// nothing rather than guess. See InstallNative's doc comment.
	return classification{InstallUnknown, vm, "", "", InstallSourceNone}
}

// normalizeInstallPath converts a path to forward slashes so a single set of
// segment rules covers both Windows and Unix layouts.
func normalizeInstallPath(p string) string {
	return strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
}

// underDir reports whether p is dir itself or lives inside it. Both arguments
// must already be normalized to forward slashes.
func underDir(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		return false
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// npmEvidence reports whether the resolved path belongs to a node-package
// install of the CLI, strongest evidence first, along with which of npm, pnpm
// or bun owns it.
func npmEvidence(p string, env installEnv) (packageManager string, src InstallSource, ok bool) {
	// The packaging metadata describing this very binary. Both codex paths
	// walk up to a package.json naming CLIPackageName: the JS wrapper sits two
	// levels under `@openai/codex`, and the vendored native binary four levels
	// under the platform sub-package (which is an npm alias of the same name).
	// That shared ancestor is the strongest classifier available, and it beats
	// matching on any single directory name.
	dir := path.Dir(p)
	for i := 0; i < maxPackageWalkUp; i++ {
		if isCLIPackageJSON(path.Join(dir, "package.json"), env) {
			return nodePackageManager(p, env), InstallSourcePackageMetadata, true
		}
		parent := path.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Windows shims are plain text files, not symlinks, so EvalSymlinks cannot
	// reach the package. npm writes them beside the prefix's node_modules tree.
	if isWindowsShim(p) {
		shimDir := path.Dir(p)
		for _, rel := range []string{
			path.Join("node_modules", CLIPackageName, "package.json"),
			path.Join("..", "lib", "node_modules", CLIPackageName, "package.json"),
		} {
			if isCLIPackageJSON(path.Join(shimDir, rel), env) {
				return nodePackageManager(p, env), InstallSourcePackageMetadata, true
			}
		}
		// An unresolvable shim with nothing to confirm it: say so, do not guess.
		return "", "", false
	}

	if strings.Contains(p, "/node_modules/"+CLIPackageName+"/") ||
		strings.Contains(p, "/node_modules/@openai/") {
		return nodePackageManager(p, env), InstallSourcePathLayout, true
	}
	return "", "", false
}

// maxPackageWalkUp bounds the ancestor search for a package.json. Four levels
// covers the deepest real layout (vendored binary → platform sub-package);
// the extra headroom absorbs a nested prefix without walking to the root.
const maxPackageWalkUp = 8

func isCLIPackageJSON(file string, env installEnv) bool {
	b, err := env.readFile(file)
	if err != nil {
		return false
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return false
	}
	return pkg.Name == CLIPackageName
}

// nodePackageManager reports which of npm, pnpm or bun owns a node-package
// install, mirroring the heuristics codex's own JS shim uses to decide which
// update command to print.
//
// pnpm is identified the way the shim identifies it — an owning node_modules
// directory holding a `.modules.yaml` alongside the CLI package — because
// pnpm's global layout has no distinctive path segment on every platform.
// bun and pnpm path segments are a fallback for when the metadata is
// unreadable. npm is the residue: it is the only one of the three that leaves
// no marker of its own.
func nodePackageManager(p string, env installEnv) string {
	dir := path.Dir(p)
	for i := 0; i < maxPackageWalkUp; i++ {
		nm := path.Join(dir, "node_modules")
		if _, err := env.readFile(path.Join(nm, ".modules.yaml")); err == nil &&
			isCLIPackageJSON(path.Join(nm, CLIPackageName, "package.json"), env) {
			return "pnpm"
		}
		parent := path.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	lower := strings.ToLower(p)
	switch {
	case strings.Contains(lower, "/.bun/install/global/"), strings.Contains(lower, "/bun/install/global/"):
		return "bun"
	case strings.Contains(lower, "/node_modules/.pnpm/"),
		strings.Contains(lower, "/pnpm/global/"),
		strings.Contains(lower, "/.pnpm-global/"):
		return "pnpm"
	}
	return "npm"
}

func isWindowsShim(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".cmd", ".bat", ".ps1":
		return true
	}
	return false
}

// isStandaloneLayout matches codex's standalone installer tree:
// `<CODEX_HOME>/packages/standalone/releases/<version>/bin/codex`, with
// `<CODEX_HOME>/packages/standalone/current` symlinked at the active release.
// The literal segment run is matched as well as the resolved CODEX_HOME, so a
// relocated CODEX_HOME still classifies when the env lookup is unavailable.
func isStandaloneLayout(p, codexHome string) bool {
	if strings.Contains(p, "/packages/standalone/releases/") {
		return true
	}
	if codexHome == "" {
		return false
	}
	root := strings.TrimSuffix(normalizeInstallPath(codexHome), "/") + "/packages/standalone/"
	return strings.HasPrefix(p, root)
}

// versionManagerSegments maps a path segment to the tool version manager that
// owns it. Matching is segment-exact so an unrelated directory whose name
// merely contains "nvm" cannot trigger it. `fnm_multishells` is fnm's
// per-shell runtime directory, which is what actually lands on PATH under fnm.
var versionManagerSegments = map[string]string{
	".nvm": "nvm", "nvm": "nvm",
	".fnm": "fnm", "fnm": "fnm", "fnm_multishells": "fnm",
	".volta": "volta", "volta": "volta",
	".asdf": "asdf", "asdf": "asdf",
	".mise": "mise", "mise": "mise",
}

func detectVersionManager(segs []string) string {
	for _, s := range segs {
		if name, ok := versionManagerSegments[s]; ok {
			return name
		}
	}
	return ""
}

// detectOSPackageManager keys entirely off where the running binary lives.
// Note codex 0.148.0 does not itself recognise any of these — it reports
// "other" — so these classifications are ours, and only the Homebrew command
// comes from codex's own vocabulary.
func detectOSPackageManager(p string, segs []string) (manager, name string, ok bool) {
	for i, s := range segs {
		if !strings.EqualFold(s, "Caskroom") {
			continue
		}
		cask := "codex"
		if i+1 < len(segs) && segs[i+1] != "" {
			cask = segs[i+1]
		}
		return "homebrew", cask, true
	}

	lower := strings.ToLower(p)
	if strings.Contains(lower, "/microsoft/winget/packages/") ||
		strings.Contains(lower, "/microsoft/winget/links/") {
		return "winget", "OpenAI.Codex", true
	}
	// Both managers keep tools under `<root>/installs/`, and both roots appear
	// dotted or undotted depending on how they were set up.
	for _, m := range []string{"mise", "asdf"} {
		if strings.Contains(lower, "/"+m+"/installs/") || strings.Contains(lower, "/."+m+"/installs/") {
			return m, "codex", true
		}
	}
	return "", "", false
}

// updateCommand returns the command that updates this install, or "" when none
// is known to be correct. See the verification table on DetectInstall for how
// each of these was established.
func updateCommand(m InstallMethod, packageManager, packageName string) string {
	switch m {
	case InstallNative:
		// Verified: `codex update` re-runs the standalone installer for this
		// layout, and refuses for every layout it cannot attribute.
		return "codex update"
	case InstallNPMGlobal:
		switch packageManager {
		case "bun":
			return "bun install -g " + CLIPackageName + "@latest"
		case "pnpm":
			return "pnpm add -g " + CLIPackageName + "@latest"
		default:
			return "npm install -g " + CLIPackageName + "@latest"
		}
	case InstallPackageManager:
		switch packageManager {
		case "homebrew":
			if packageName == "" {
				packageName = "codex"
			}
			return "brew upgrade --cask " + packageName
		case "winget":
			return "winget upgrade OpenAI.Codex"
		case "mise":
			return "mise upgrade codex"
		}
		// asdf and anything else: there is no single correct command here.
		return ""
	default:
		return ""
	}
}
