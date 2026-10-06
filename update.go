package codexcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// defaultUpdateTimeout bounds `codex update` when the caller's context carries
// no deadline. A standalone update downloads and unpacks a release archive, so
// the bound is generous on purpose: it exists to stop a wedged updater hanging
// a consumer forever, not to cut a slow network short.
const defaultUpdateTimeout = 10 * time.Minute

// updateInterruptGrace is how long a cancelled update is given to unwind after
// its interrupt signal before the process is killed outright. The standalone
// installer stages a download in a temporary directory and swaps symlinks into
// place at the end, and it removes that directory from an EXIT/INT/TERM trap —
// so letting it notice the signal is what keeps a cancelled update from
// leaving a half-unpacked release tree behind. Unix only: on Windows no
// interrupt is deliverable and cancellation kills the tree immediately, so
// this bounds only the pipe teardown there.
const updateInterruptGrace = 5 * time.Second

// maxUpdateOutputLines caps the transcript kept in UpdateResult.Output. The
// installer is chatty about progress; the tail is what explains a failure.
const maxUpdateOutputLines = 200

// ErrManualUpdate matches the error returned by [Update] when the install is
// not one codex's own updater manages. Use errors.Is to distinguish it: this
// is a normal outcome, not a failure. It is the answer for most installs in
// the wild, and the right response is to show the user a command, not an error.
var ErrManualUpdate = errors.New("codexcli: install is not managed by codex's own updater")

// ManualUpdateError is returned when the detected install must be updated by
// something other than `codex update`. Use errors.As to read the command to
// display.
type ManualUpdateError struct {
	// Method is the detected install method that this package will not update.
	Method InstallMethod

	// Reason says why an npm-global install was refused — which step of the
	// prefix proof in [Update] failed. "" for methods never updated here.
	// Diagnostic prose, not a command.
	Reason string

	// Command is what the user has to run, verbatim and ready to display — or
	// "" when no command is known to be correct.
	//
	// An empty Command is a legitimate answer meaning "tell the user to update
	// manually". Never substitute a guess for it: `npm install -g
	// @openai/codex` against a standalone install writes a second, complete
	// copy into an npm prefix, and from then on whichever copy PATH reaches
	// first is the one that answers `codex --version`. See [DetectInstall].
	Command string
}

func (e *ManualUpdateError) Error() string {
	if e.Command == "" {
		return fmt.Sprintf("codexcli: %s install must be updated manually; no update command is known to be correct", e.Method)
	}
	return fmt.Sprintf("codexcli: %s install must be updated manually with %q", e.Method, e.Command)
}

func (e *ManualUpdateError) Is(target error) bool { return target == ErrManualUpdate }

// ErrUpdateNotWritable matches the error returned by [Update] when the install
// is self-managed but a directory codex's updater writes into cannot be
// written by this process.
//
// Keep this distinct from a failed run. "Cannot" and "tried and failed" are
// different answers to a consumer: the first means never offer the button, the
// second means show an error after it was clicked.
var ErrUpdateNotWritable = errors.New("codexcli: update target directory is not writable")

// UpdateNotWritableError reports that a directory the updater writes into is
// not writable by this process, so the update was never attempted.
type UpdateNotWritableError struct {
	// Method is the detected install method.
	Method InstallMethod

	// Dir is the directory that failed the probe. See [Update] on why this is
	// generally not the directory holding the binary on PATH.
	Dir string

	// Err is the underlying filesystem error from the write probe.
	Err error
}

func (e *UpdateNotWritableError) Error() string {
	return fmt.Sprintf("codexcli: cannot update %s install: %s is not writable: %v", e.Method, e.Dir, e.Err)
}

func (e *UpdateNotWritableError) Is(target error) bool { return target == ErrUpdateNotWritable }

func (e *UpdateNotWritableError) Unwrap() error { return e.Err }

// ErrUpdateInUse matches the error returned by [Update] when a file the
// update would replace is held by another process — on Windows, a codex
// running from the install being updated. Nothing was installed.
//
// It is the third preflight answer, distinct from both others: unlike
// [ErrUpdateNotWritable] it is transient — the same call goes through once
// the process exits, so the right response is "close codex and retry", not
// hiding the button — and unlike [ErrUpdateFailed] nothing was attempted.
// [InstallInfo.SelfManaged] does not account for it, for the same reason.
var ErrUpdateInUse = errors.New("codexcli: update target is in use by a running process")

// UpdateInUseError reports the file that blocked an update. See
// [ErrUpdateInUse].
type UpdateInUseError struct {
	// Method is the detected install method.
	Method InstallMethod

	// Path is the held file: typically the vendored codex.exe of a running
	// session, or one of the helper executables it starts.
	Path string
}

func (e *UpdateInUseError) Error() string {
	return fmt.Sprintf("codexcli: cannot update %s install while %s is in use by another process", e.Method, e.Path)
}

func (e *UpdateInUseError) Is(target error) bool { return target == ErrUpdateInUse }

// ErrUpdateFailed matches the error returned by [Update] when the updater ran
// and exited non-zero, could not be started at all, or — for an npm-global
// install — exited 0 without the version moving while a newer one was asked
// for.
var ErrUpdateFailed = errors.New("codexcli: codex update failed")

// UpdateFailedError reports that the updater ran and failed. The
// [UpdateResult] is still returned alongside it, so the before/after versions
// and the captured output are available for diagnosis.
type UpdateFailedError struct {
	// Path is the updater that was executed: the codex PATH entry, or the
	// proven npm (see [UpdateResult.Updater]).
	Path string

	// ExitCode is the updater's exit status, or -1 when it never ran to
	// completion (killed by a signal, or the process could not start).
	ExitCode int

	// Output is the tail of the updater's combined stdout and stderr.
	Output string

	// Err is the underlying exec error.
	Err error
}

func (e *UpdateFailedError) Error() string {
	msg := fmt.Sprintf("codexcli: %s update exited %d", e.Path, e.ExitCode)
	if e.ExitCode == 0 && e.Err != nil {
		return msg + ": " + e.Err.Error()
	}
	if tail := lastOutputLine(e.Output); tail != "" {
		msg += ": " + tail
	}
	return msg
}

func (e *UpdateFailedError) Is(target error) bool { return target == ErrUpdateFailed }

func (e *UpdateFailedError) Unwrap() error { return e.Err }

// UpdateResult describes one update run.
//
// # The exit code is not the answer
//
// VersionBefore and VersionAfter are read by running `codex --version` either
// side of the update, and Changed compares them. That re-read is the only
// trustworthy signal that anything happened, because codex's exit status is
// not one: `codex update` was observed exiting 0 and printing "Update ran
// successfully!" while the command it shells out to was not installed on the
// machine at all. It reports the success of *launching* an update, not of
// applying one. Believe the version, not the status.
type UpdateResult struct {
	// Method is the install method that was updated: [InstallNative], or
	// [InstallNPMGlobal] when the npm prefix was proven. See [Update].
	Method InstallMethod

	// Path is the codex binary whose version is reported — the PATH entry
	// recorded by detection, never a fresh lookup and never the symlink
	// target. For a standalone install it is also the binary executed. See
	// [Update] on why that layer.
	Path string

	// Updater is the executable that was run: Path for a standalone install,
	// the proven npm for an npm-global one — `<prefix>/bin/npm`, or the npm
	// found on the child's PATH when the prefix has none, as found (not
	// resolved to npm-cli.js). On Windows always the latter, typically
	// `C:\Program Files\nodejs\npm.cmd`.
	Updater string

	// VersionBefore is what the CLI reported for itself before the run, or ""
	// when that probe failed.
	VersionBefore string

	// VersionAfter is what it reports afterwards, or "" when the re-read
	// failed. An empty VersionAfter always comes with a non-nil error: the
	// update may well have succeeded, but nothing here can say so.
	VersionAfter string

	// Changed is true only when both versions are known and differ. A false
	// Changed with a nil error is the ordinary "already up to date" outcome,
	// and is indistinguishable from an updater that silently did nothing —
	// which is exactly why nothing here reports success on the exit code.
	Changed bool

	// ExitCode is the updater's exit status, kept for diagnostics. Do not
	// derive success from it.
	ExitCode int

	// Output is the tail of the updater's combined stdout and stderr, the last
	// maxUpdateOutputLines lines, newline-joined.
	Output string

	// Duration is how long the updater ran.
	Duration time.Duration
}

// UpdateOption configures a single [Update] call.
type UpdateOption func(*updateOptions)

type updateOptions struct {
	output   io.Writer
	progress func(string)
	timeout  time.Duration
}

// WithUpdateOutput streams the updater's combined stdout and stderr to w as it
// arrives, so a consumer can show live output for a run that takes minutes.
// Writes happen on the goroutine draining the process; w must not block for
// long and must stay valid for the duration of the call.
func WithUpdateOutput(w io.Writer) UpdateOption {
	return func(o *updateOptions) { o.output = w }
}

// WithUpdateProgress calls fn once per output line as the updater emits it,
// mirroring [WithStderrCallback] for sessions. Lines are split on both "\n"
// and "\r" so a progress indicator that redraws in place still narrates.
//
// The lines are plain text on purpose. codex's installer prints prose
// ("Downloading Codex CLI", "Installing standalone package to ..."), and a
// consumer renders one live line of it; a structured schema here would only be
// concatenated back into the same prose, with the labels invented by this
// package rather than taken from the installer.
func WithUpdateProgress(fn func(string)) UpdateOption {
	return func(o *updateOptions) { o.progress = fn }
}

// WithUpdateTimeout bounds the updater run. It applies only when the caller's
// context has no deadline of its own; a context deadline always wins.
func WithUpdateTimeout(d time.Duration) UpdateOption {
	return func(o *updateOptions) { o.timeout = d }
}

// Update updates the codex install on PATH, using the default client's
// binary.
//
// # The standalone install, and a proven npm install
//
// Detection runs first and decides. Two methods are updated:
//
//   - [InstallNative] — codex's own standalone installer layout under
//     CODEX_HOME — by running `codex update`.
//   - [InstallNPMGlobal] owned by npm, on unix or Windows, when the npm update
//     target is proven to be the tree PATH runs (below) — by running that
//     proven npm, by absolute path, as `npm install -g @openai/codex@<latest>`.
//
// Every other install is refused with a [ManualUpdateError] carrying
// [InstallInfo.UpdateCmd] verbatim for the user to run: pnpm, bun, Homebrew,
// winget, a version-manager root with no package metadata, an unknown binary,
// and any npm install the proof does not hold for. That refusal is a normal
// outcome, not a failure. [InstallInfo.SelfManaged] is computed by the same
// check, writability included, so it is true exactly when this function does
// not refuse — except for [ErrUpdateInUse], which is transient and which
// SelfManaged does not predict.
//
// # Why npm is not driven through `codex update`
//
// Verified against codex 0.148.0, `codex update` also acts for a node-managed
// install by shelling out to `npm install -g @openai/codex` (or the pnpm/bun
// equivalent). That is still not what runs here:
//
//   - The prefix it writes is not necessarily the prefix being run. codex
//     reports both, as [DoctorInstallation.ManagedPackageRoot] and
//     [DoctorInstallation.NPMUpdateTarget], precisely because they can differ —
//     and when they do, the "update" installs a second copy whose visibility
//     depends on PATH order. A library that owns the codex command must not
//     create that state on a user's machine.
//   - It finds npm on PATH and runs whatever answers, checking neither where
//     that npm writes nor that it ran: with npm absent it still exits 0 and
//     prints "Update ran successfully!".
//
// # The npm proof
//
// An npm-global install is updated only when all of this holds, and is manual
// otherwise. On unix:
//
//   - The resolved binary sits under `<prefix>/lib/node_modules/@openai/codex`
//     with a package.json naming the CLI.
//   - One npm is chosen, by absolute path, and every npm step below runs that
//     path; nothing looks npm up a second time.
//   - When anything exists at `<prefix>/bin/npm`, that is the npm, and it and
//     `<prefix>/bin/node` must be executable files — the node that owns the
//     package. Under fnm that is `…/node-versions/<v>/installation/bin/npm`,
//     which exists even when the per-shell `fnm_multishells` directory does
//     not. `<prefix>/bin` goes first on the child's PATH.
//   - Only when nothing exists there — a system node with a user-level prefix
//     set in .npmrc, the usual way to install globally without sudo — is npm
//     looked up on the PATH the child will run with ([WithEnv]'s PATH if set,
//     else this process's), skipping relative entries. That npm must resolve,
//     symlinks followed, to `bin/npm-cli.js` in a package named npm: a Volta,
//     asdf, mise or corepack shim is refused, because a dispatcher can answer
//     `prefix -g` from npm's config and still install somewhere else (Volta
//     does). Its `#!` line must name node — `/usr/bin/env node`, resolved on
//     that same PATH, or an absolute node — which must be an executable file;
//     that node's directory goes first on the child's PATH, so the shebang
//     reaches the node that was checked.
//   - That npm, run with that directory first on PATH and otherwise the
//     subprocess environment the install would run with (so .npmrc and
//     npm_config_* count), exits cleanly and reports a `npm prefix -g` whose
//     `lib/node_modules/@openai/codex` resolves to the same directory as the
//     package root PATH runs.
//   - For the PATH npm only, the install is run with `--prefix` set to the
//     prefix `npm prefix -g` reported. A prefix npm derives from the node it
//     runs under would otherwise follow a node version switch made between
//     the proof and the install; npm derives its global npmrc from the prefix
//     too, so the pin changes nothing else.
//
// On Windows, where a global prefix has no lib\ or bin\ and npm does not live
// in it (verified on Windows 11, node 24.20.0, npm 11.6.0, codex 0.159.0):
//
//   - The PATH entry is an npm `.cmd` shim whose run line names
//     `%dp0%\node_modules\@openai\codex\…` — the package beside it — and that
//     directory holds a package.json naming the CLI. The shim's directory is
//     the prefix, and is also where npm writes the shims.
//   - npm is resolved once, to an absolute path, as the unix fallback does: the
//     first `npm` with a PATHEXT extension on the PATH the install will run
//     with (a "Path" override counts), normally the npm.cmd beside node.exe,
//     else an npm.cmd in the prefix itself. It must be npm's own npm.cmd —
//     a batch file running `node_modules\npm\bin\npm-cli.js` beside it, in a
//     package named npm — so Volta's npm.exe, which would intercept the
//     install, is refused. That one file runs `prefix -g`, `view` and
//     `install -g`, with its own directory first on PATH so the node.exe
//     beside it is the node npm uses.
//   - That npm, under the subprocess environment, reports a `prefix -g` that
//     is the shim's directory, and whose `node_modules\@openai\codex` is the
//     package root the shim runs. Both are compared after resolving symlinks
//     and 8.3 short names, as exact paths and as the same file system object
//     ([os.SameFile]). A prefix reached through a junction does not resolve
//     and is refused.
//   - The install is run with `--prefix` set to the reported prefix, as for
//     the unix PATH npm: nvm-windows derives the prefix from the node
//     directory and switches node by repointing it.
//
// A mismatch is the known failure mode — a second copy whose visibility
// depends on PATH order — and is refused as manual, not attempted. So is an
// npm whose prefix holds no CLI package, and a prefix npm prints redacted (npm
// shows a UUID-shaped path segment as "***", which resolves to nothing).
//
// Whichever npm runs, the writes it is checked for are the owning prefix's:
// `<prefix>/lib/node_modules` and `<prefix>/bin`, or `<prefix>\node_modules`
// and the prefix on Windows — never the directory npm itself lives in.
//
// # Which binary is executed
//
// The PATH entry recorded by detection ([InstallInfo.Path]), as an absolute
// path — never the bare word "codex", and never the symlink-resolved
// [InstallInfo.RealPath].
//
// Not the bare word, because a fresh lookup at exec time could reach a
// different copy than the one just detected. Two copies on one machine is not
// hypothetical: [DoctorInstallation.PathEntries] exists because codex sees
// them, and the capture this package tests against has two.
//
// Not the resolved path either. A standalone install's PATH entry is a symlink
// into `<CODEX_HOME>/packages/standalone/current`, which is itself a symlink at
// the active release — so resolving past it runs the very binary the update is
// about to supersede, from a directory the update is about to replace. The
// PATH entry is the layer the user's own shell runs, so an update launched
// through it differs from a hand-typed `codex update` in nothing but the
// absolute path.
//
// # Preflight, then verify
//
// The directories the updater writes into are probed for writability first,
// and a failure there returns [ErrUpdateNotWritable] without running anything
// — a consumer renders "cannot update" differently from "update failed". For a
// standalone install these are not the directory holding the binary on PATH:
// the installer unpacks into `<CODEX_HOME>/packages/standalone/releases` and
// rewrites the visible symlink in `$CODEX_INSTALL_DIR` (default
// `~/.local/bin`) on every run, whichever directory PATH actually reaches the
// CLI through. For npm they are `<prefix>/lib/node_modules` and the
// `<prefix>/bin` link directory on unix, and `<prefix>\node_modules` and the
// prefix itself, which holds the shims, on Windows. (An unwritable npm prefix
// makes [InstallInfo.SelfManaged] false, so a consumer keying on it shows the
// command instead.)
//
// On Windows one more check runs, last, immediately before npm starts: no
// file in the package tree or among the codex shims may be held by another
// process, or the call returns [ErrUpdateInUse] and nothing is installed.
// npm replaces a global package by renaming it aside, extracting the new
// one, and deleting the old. Windows lets that rename happen under a running
// codex.exe, so npm installs the new version, fails to delete the running
// image, and exits 0 with the old tree left behind as
// `node_modules\@openai\.codex-<hash>` — while the running codex resolves its
// helper executables by a path that now holds the new version's. A file held
// without delete sharing fails the rename instead, and npm rolls back. The
// check runs after the version is resolved, so an install already at latest
// answers that rather than "in use". A codex that starts in the moment
// between the check and npm's rename (about two seconds of npm startup) is
// the one case it cannot see: that update succeeds and is reported as such,
// and npm's next run on the package removes the leftover tree.
//
// Afterwards the version is re-read through the PATH entry (the `.cmd` shim
// on Windows), because the exit code cannot be trusted; see [UpdateResult].
// On failure the result is returned alongside the error, because a half-run
// update still has before/after numbers worth rendering. For npm the version
// to install is resolved first with the proven npm's `npm view
// @openai/codex@latest version` and pinned: when it equals the installed
// version nothing runs, and after a clean npm exit the PATH entry must report
// exactly that version, or the run is [ErrUpdateFailed].
//
// The caller's context deadline is honoured. Without one the run is bounded by
// [WithUpdateTimeout], defaulting to ten minutes. On unix a cancelled run is
// interrupted rather than killed outright — SIGINT to the updater's process
// group — so the installer can unwind its staged download, and npm can roll
// back, instead of leaving a partial tree behind. On Windows no interrupt is
// deliverable from a windowless parent; the only stop is a tree kill via a
// job object, so:
//
//   - A Windows `npm install -g`, once started, is not stopped by
//     cancellation: Update waits for npm to exit and reports what it did,
//     success included. Killing it was observed to leave the install broken —
//     npm renames the old package and shims aside before it downloads the new
//     one, so a kill a few seconds in left no codex.cmd on PATH and a
//     half-extracted package. Its only bound is ten minutes from start, for an
//     npm that has wedged; a kill there can leave that same state, which
//     running [InstallInfo.UpdateCmd] repairs. The process calling Update
//     exiting mid-install kills npm with it, with the same effect.
//   - A cancellation before npm install starts — during detection or `npm
//     view` — stops there, and nothing is installed.
//   - A cancelled standalone `codex update` is killed immediately and may
//     leave a staged partial download for the installer to clean up on its
//     next run.
func Update(ctx context.Context, opts ...UpdateOption) (*UpdateResult, error) {
	return defaultInstallClient.Update(ctx, opts...)
}

// Update runs `codex update` for this client's binary. See the package-level
// [Update] for the full contract.
//
// The client's own defaults apply: WithCodexHome relocates both the detection
// and the CODEX_HOME the updater runs against, and WithEnv/WithWorkDir shape
// the subprocess the same way they shape [Client.Doctor]. Per-call Options are
// not accepted here — [UpdateOption] is a separate set, so anything that has
// to differ per call belongs on a client built for it.
func (c *Client) Update(ctx context.Context, opts ...UpdateOption) (*UpdateResult, error) {
	resolved := resolveOptions(c.defaults, nil)

	childEnv := resolved.env
	if resolved.codexHome != "" {
		childEnv = withCodexHome(childEnv, resolved.codexHome)
	}
	env := osUpdateEnv(resolved.codexHome, childEnv, resolved.workDir)

	result, err := runUpdate(ctx, c.binaryPath(), env, opts)
	if err != nil {
		c.log().Debug("update", "err", err)
		return result, err
	}
	c.log().Debug("update",
		"method", result.Method, "path", result.Path,
		"versionBefore", result.VersionBefore, "versionAfter", result.VersionAfter,
		"changed", result.Changed, "exitCode", result.ExitCode,
		"duration", result.Duration)
	return result, nil
}

// updateEnv is the set of ambient operations Update performs beyond detection,
// injectable so tests can drive every accept/refuse decision without a codex
// CLI on the machine.
type updateEnv struct {
	installEnv

	// binDir is where codex's standalone installer maintains the visible
	// `codex` symlink, which it rewrites on every run.
	binDir string

	// npmLatest runs `<npm> view @openai/codex@latest version` with pathDir
	// first on PATH, so the registry npm is configured for answers.
	npmLatest func(ctx context.Context, npm, pathDir string) (string, error)

	// inUse reports the first of an npm plan's inUse paths another process
	// holds; see firstFileInUse.
	inUse func(paths []string) (string, error)

	runUpdate func(ctx context.Context, run updaterRun, onLine func(string)) (int, error)
}

// updaterRun is one updater invocation: the executable, its arguments, and a
// directory to put first on the child's PATH ("" for none).
type updaterRun struct {
	name    string
	args    []string
	pathDir string

	// finishOnCancel means a cancellation arriving after the updater started
	// does not stop it: the run is bounded by uncancellableUpdateLimit alone,
	// and Update reports whatever it did. A run not yet started when the
	// context ends is not started.
	finishOnCancel bool
}

// uncancellableUpdateLimit bounds an updater run that ignores cancellation
// (a Windows `npm install -g`). It exists to stop a wedged npm holding the
// caller forever; killing npm at this point can leave the install broken,
// which is still better than never returning.
const uncancellableUpdateLimit = defaultUpdateTimeout

func osUpdateEnv(codexHome string, childEnv map[string]string, workDir string) updateEnv {
	return updateEnv{
		installEnv: osInstallEnv(codexHome).withChildEnv(childEnv, workDir),
		binDir:     standaloneBinDir(childEnv),
		npmLatest: func(ctx context.Context, npm, pathDir string) (string, error) {
			return runNPMLatest(ctx, npm, pathDir, childEnv, workDir)
		},
		inUse: firstFileInUse,
		runUpdate: func(ctx context.Context, run updaterRun, onLine func(string)) (int, error) {
			overrides := childEnv
			if run.pathDir != "" {
				overrides = withPathPrefix(childEnv, run.pathDir)
			}
			if run.finishOnCancel {
				if err := ctx.Err(); err != nil {
					return -1, err
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), uncancellableUpdateLimit)
				defer cancel()
			}
			return execUpdater(ctx, run.name, run.args, buildEnv(overrides), workDir, onLine)
		},
	}
}

func runUpdate(ctx context.Context, binary string, env updateEnv, opts []UpdateOption) (*UpdateResult, error) {
	var o updateOptions
	for _, opt := range opts {
		opt(&o)
	}

	info, err := detectInstall(ctx, binary, env.installEnv)
	if err != nil {
		return nil, err
	}

	var (
		run     updaterRun
		targets []string
		npm     *npmUpdatePlan
	)
	switch info.Method {
	case InstallNative:
		run = updaterRun{name: info.Path, args: []string{"update"}}
		targets = updateTargets(info, env)
	case InstallNPMGlobal:
		plan, err := proveNPMUpdate(ctx, info, env.installEnv)
		if err != nil {
			return nil, &ManualUpdateError{Method: info.Method, Command: info.UpdateCmd, Reason: err.Error()}
		}
		npm = plan
		run = updaterRun{name: plan.npm, pathDir: plan.pathDir, finishOnCancel: plan.finishOnCancel}
		targets = plan.targets
	default:
		return nil, &ManualUpdateError{Method: info.Method, Command: info.UpdateCmd}
	}

	if len(targets) == 0 {
		return nil, &UpdateNotWritableError{
			Method: info.Method,
			Err:    errors.New("no update target directory could be determined"),
		}
	}
	for _, dir := range targets {
		if err := probeWritable(env.installEnv, dir); err != nil {
			return nil, &UpdateNotWritableError{Method: info.Method, Dir: dir, Err: err}
		}
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		timeout := o.timeout
		if timeout <= 0 {
			timeout = defaultUpdateTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	ring := &lineRing{max: maxUpdateOutputLines}
	onLine := func(line string) {
		ring.add(line)
		if o.progress != nil {
			o.progress(line)
		}
		if o.output != nil {
			_, _ = io.WriteString(o.output, line+"\n")
		}
	}

	result := &UpdateResult{
		Method:        info.Method,
		Path:          info.Path,
		Updater:       run.name,
		VersionBefore: info.Version,
	}
	start := time.Now()
	finish := func() {
		result.Output = strings.Join(ring.lines(), "\n")
		result.Duration = time.Since(start)
	}

	// npm installs whatever it is told to, current or not, so the version to
	// install is resolved first and pinned. That makes the re-read an exact
	// check: after a clean exit the PATH entry must report that version.
	var want string
	if npm != nil {
		onLine("Resolving " + CLIPackageName + "@latest")
		latest, err := env.npmLatest(ctx, npm.npm, npm.pathDir)
		if err == nil && latest == "" {
			err = errors.New("npm reported no version")
		}
		if err != nil {
			onLine(err.Error())
			result.ExitCode = -1
			finish()
			return result, &UpdateFailedError{
				Path:     npm.npm,
				ExitCode: -1,
				Output:   result.Output,
				Err:      fmt.Errorf("resolve %s@latest: %w", CLIPackageName, err),
			}
		}
		if latest == info.Version {
			onLine(fmt.Sprintf("%s %s is already the latest version", CLIPackageName, latest))
			result.VersionAfter = info.Version
			finish()
			return result, nil
		}
		want = latest

		// Checked last, right before npm starts, so an install already at
		// latest answers that instead, and the window for a codex to start in
		// between is as short as it can be made.
		if len(npm.inUse) > 0 {
			if err := checkNotInUse(info.Method, npm.inUse, env); err != nil {
				return nil, err
			}
		}
		run.args = []string{"install", "--global"}
		if npm.pinPrefix != "" {
			run.args = append(run.args, "--prefix", npm.pinPrefix)
		}
		run.args = append(run.args, CLIPackageName+"@"+latest)
		onLine(fmt.Sprintf("Installing %s@%s into %s", CLIPackageName, latest, npm.prefix))
	}

	exitCode, runErr := env.runUpdate(ctx, run, onLine)
	finish()

	after, probeErr := reprobeVersion(ctx, info.Path, env.installEnv)

	result.VersionAfter = after
	result.Changed = info.Version != "" && after != "" && info.Version != after
	result.ExitCode = exitCode

	if runErr != nil {
		failed := &UpdateFailedError{
			Path:     run.name,
			ExitCode: exitCode,
			Output:   result.Output,
			Err:      runErr,
		}
		// A failed run whose version could not be re-read leaves two facts
		// worth reporting, not one.
		return result, errors.Join(failed, probeErr)
	}
	if probeErr != nil {
		return result, fmt.Errorf("codexcli: update ran but the installed version could not be re-read, so nothing confirms it applied: %w", probeErr)
	}
	if want != "" && after != want {
		// npm exited 0, but the binary PATH runs does not report what it was
		// told to install. Believe the version.
		return result, &UpdateFailedError{
			Path:     run.name,
			ExitCode: exitCode,
			Output:   result.Output,
			Err:      fmt.Errorf("installed %s@%s, but %s reports %q", CLIPackageName, want, info.Path, after),
		}
	}
	return result, nil
}

// probeWritable runs the environment's writability check. A missing check is
// a refusal, never a pass.
func probeWritable(env installEnv, dir string) error {
	if env.writable == nil {
		return errors.New("no writability check available")
	}
	return env.writable(dir)
}

// checkNotInUse runs the environment's in-use probe over paths. A held file
// is [ErrUpdateInUse]; a file that could not be probed is
// [ErrUpdateNotWritable], since npm could not replace it either; and a
// missing probe is a refusal, never a pass.
func checkNotInUse(method InstallMethod, paths []string, env updateEnv) error {
	if env.inUse == nil {
		return &UpdateNotWritableError{Method: method, Err: errors.New("no in-use check available")}
	}
	held, err := env.inUse(paths)
	if err != nil {
		return &UpdateNotWritableError{Method: method, Dir: filepath.Dir(held), Err: err}
	}
	if held != "" {
		return &UpdateInUseError{Method: method, Path: held}
	}
	return nil
}

// reprobeVersion re-reads the installed version after an update.
//
// It runs on a short bound detached from the caller's context. The update has
// already happened by this point, and the re-read is the only signal that says
// whether it did anything — inheriting a context the update itself may have
// just exhausted would throw that signal away exactly when it matters most.
func reprobeVersion(ctx context.Context, binary string, env installEnv) (string, error) {
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultInstallTimeout)
	defer cancel()
	return env.runVersion(probeCtx, binary)
}

// updateTargets reports the directories codex's standalone installer writes
// into for this install — the ones whose permissions decide whether an update
// can work at all.
//
// Neither is necessarily the directory holding the binary on PATH. Verified
// against the installer script codex fetches (releases.openai.com/codex/
// install.sh, 2026-08-24): it unpacks the release under
// `<CODEX_HOME>/packages/standalone/releases/<version>-<target>`, repoints the
// sibling `current` symlink, and then rewrites `$CODEX_INSTALL_DIR/codex`
// (default `~/.local/bin/codex`) unconditionally — even when PATH reaches the
// CLI through some other directory entirely.
//
// A directory that cannot be determined is omitted rather than guessed at: an
// unnecessary "cannot update" is a button the consumer never offers.
func updateTargets(info *InstallInfo, env updateEnv) []string {
	var dirs []string
	if d := standaloneReleasesDir(info.RealPath, env.codexHome); d != "" {
		dirs = append(dirs, d)
	}
	if env.binDir != "" {
		dirs = append(dirs, env.binDir)
	}
	return dirs
}

// standaloneReleasesDir reports where the standalone installer unpacks release
// trees. The resolved binary's own release root is preferred — it describes
// the install that actually runs — and the `<CODEX_HOME>/packages/standalone/
// releases` layout is the fallback when the binary sits elsewhere (a stale
// symlink, a relocated home).
func standaloneReleasesDir(realPath, codexHome string) string {
	const marker = "/packages/standalone/releases/"
	if p := normalizeInstallPath(realPath); strings.Contains(p, marker) {
		i := strings.Index(p, marker)
		return filepath.FromSlash(p[:i+len(marker)-1])
	}
	if codexHome == "" {
		return ""
	}
	return filepath.Join(codexHome, "packages", "standalone", "releases")
}

// standaloneBinDir reports the directory codex's standalone installer keeps
// the visible `codex` symlink in: $CODEX_INSTALL_DIR, else ~/.local/bin. It
// returns "" when neither can be determined, which drops it from the preflight
// rather than failing one.
//
// The subprocess overrides ([WithEnv]) are consulted before this process's own
// environment, because they are what the installer will actually see. A
// preflight that probed this process's ~/.local/bin while the updater wrote
// into an overridden one would be answering about the wrong directory.
func standaloneBinDir(overrides map[string]string) string {
	if dir := overrides["CODEX_INSTALL_DIR"]; dir != "" {
		return dir
	}
	if dir := os.Getenv("CODEX_INSTALL_DIR"); dir != "" {
		return dir
	}
	home := overrides["HOME"]
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return ""
		}
	}
	return filepath.Join(home, ".local", "bin")
}

// maxWritableWalkUp bounds the search for an existing ancestor in
// checkWritable. A target more than this many levels below anything that
// exists is not a directory an installer is about to create.
const maxWritableWalkUp = 16

// checkWritable reports whether this process could write into dir.
//
// It creates and removes a temporary file rather than inspecting permission
// bits, because the bits are not the whole answer: ACLs, read-only mounts and
// root-owned prefixes all deny a write that mode bits appear to allow. The
// probe file is a dotfile and is removed before returning, so no later
// DetectInstall can mistake it for an install artifact.
//
// A directory that does not exist yet is not a failure — the installer runs
// `mkdir -p` — so the nearest existing ancestor is probed instead, which is
// the directory that mkdir would actually have to write into.
func checkWritable(dir string) error {
	if dir == "" {
		return errors.New("no update target directory could be determined")
	}
	probe, err := nearestExistingDir(dir)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(probe, ".codexcli-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	return errors.Join(f.Close(), os.Remove(name))
}

func nearestExistingDir(dir string) (string, error) {
	for i := 0; i < maxWritableWalkUp; i++ {
		info, err := os.Stat(dir)
		switch {
		case err == nil && info.IsDir():
			return dir, nil
		case err == nil:
			return "", fmt.Errorf("%s is not a directory", dir)
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no existing ancestor of %s could be found", dir)
}

// execUpdater runs an updater — `<codex> update` or `<npm> install --global …`
// — forwarding every output line to onLine as it arrives. On Windows npm is
// npm.cmd: os/exec runs a batch file through cmd.exe and refuses arguments it
// cannot quote safely, which the fixed `install --global @openai/codex@<v>`
// never contains, and the hidden console set by setUpdateCancel is inherited
// by cmd.exe and the node it starts, so nothing flashes on screen.
//
// On unix, cancellation interrupts rather than kills: the installer traps
// INT/TERM to remove its staging directory, so SIGINT to the process group
// gives it — and any children doing the actual download — a moment to unwind
// before the kill lands after updateInterruptGrace. Windows has no
// deliverable interrupt from a windowless parent (GenerateConsoleCtrlEvent
// only reaches processes on the caller's own console), so cancellation there
// is an immediate job-object tree kill: no grace period, but no orphaned
// children either.
func execUpdater(ctx context.Context, name string, args []string, env []string, workDir string, onLine func(string)) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	if workDir != "" {
		cmd.Dir = workDir
	}
	pp := setUpdateCancel(cmd)
	cmd.WaitDelay = updateInterruptGrace

	// os/exec serializes writes when Stdout and Stderr are the same comparable
	// writer, so one line splitter safely sees both streams interleaved.
	w := &lineWriter{fn: onLine}
	cmd.Stdout = w
	cmd.Stderr = w

	err := cmd.Start()
	if err == nil {
		pp.afterStart(cmd)
		err = cmd.Wait()
	}
	pp.release()
	w.flush()

	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), err
	}
	return -1, err
}

// lineWriter splits a byte stream into lines and hands each to fn. It breaks
// on carriage returns as well as newlines so an updater that redraws a
// progress indicator in place still produces something to narrate.
type lineWriter struct {
	fn  func(string)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\n\r")
		if i < 0 {
			break
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// flush emits whatever trailing text arrived without a line break.
func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}

func (w *lineWriter) emit(line string) {
	if line = strings.TrimRight(line, " \t"); line != "" && w.fn != nil {
		w.fn(line)
	}
}

// lineRing keeps the last max lines of a stream. The head is what the
// installer says it is about to do; the tail is what explains a failure.
type lineRing struct {
	max int
	buf []string
}

func (r *lineRing) add(line string) {
	if r.max <= 0 {
		return
	}
	r.buf = append(r.buf, line)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
}

func (r *lineRing) lines() []string { return r.buf }

// lastOutputLine returns the final non-empty line of s, for one-line error
// messages.
func lastOutputLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
