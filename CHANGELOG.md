# Changelog

All notable changes to `codexcli-go` are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Install the latest release with:

```
go get github.com/allbin/codexcli-go@latest
```

or pin a specific version (e.g. `@v0.3.0`).

## [Unreleased]

### Fixed

- **Cancelling a connection now kills codex's whole process tree.**
  Previously context cancellation killed only the codex process — the MCP
  servers and turn shell commands it spawned survived until their own
  stdin-EOF handling exited them, or forever if wedged. `LocalExecutor` now
  confines each spawn: on unix the child gets its own process group
  (`Setpgid`) and cancellation sends SIGTERM to the group, with a kill after
  a 5s unwind; on Windows each spawn is placed in an anonymous kill-on-close
  job object — cancel calls `TerminateJobObject` (tree kill), and closing
  the handle after `Wait()` reaps stragglers that outlive a clean codex
  exit. If job creation or assignment fails, the executor degrades to the
  old single-PID kill rather than failing the spawn. Known caveat:
  assignment happens just after `Start()` (os/exec has no `CREATE_SUSPENDED`
  path), so a child forked in that instant could escape — in practice codex
  takes far longer to start MCP servers. There is no Windows CI, so the job
  path needs a manual smoke test on real Windows.

  Upgrade note: adds a `golang.org/x/sys` dependency (Windows builds only);
  no API changes.

- **Windows: the `--version` probe now suppresses its console window.** The
  executor, `Doctor` and `Update` spawns already set CREATE_NO_WINDOW, but
  the probe `DetectInstall` and `Update` run to read the installed version
  did not — so a windowless parent (a service, a GUI app) flashed a console
  on screen for each detection.

- **Windows: npm's `codex.cmd` shim is bypassed.** When the resolved binary
  is npm's cmd.exe shim and the layout confirms it wraps `@openai/codex`,
  the executor runs node on the wrapped `bin/codex.js` directly — os/exec
  refuses to start batch files with arguments cmd.exe cannot safely escape
  (the CVE-2024-24576 hardening), so an argument containing `%` or `"`
  failed at `Start()` behind a shim. The bypass also removes the cmd.exe
  layer from the process tree. Falls back to running the shim when node is
  missing or the layout is unconfirmed. The resolver is platform-neutral
  (`shim.go`) and unit-tested on linux; the live path needs a manual smoke
  test on real Windows.

- **Cancelling `Update` interrupts the whole process group.** On unix,
  SIGINT now goes to the updater's process group (previously the single
  codex process), so any children unwinding a staged download are
  interrupted too. On Windows — where no console interrupt is deliverable
  from a windowless parent — cancellation is an immediate job-object tree
  kill instead of a lone `codex.exe` kill that orphaned children; a
  cancelled Windows update may leave a staged partial download for the
  installer to clean up on its next run.

## [0.3.0] - 2026-08-27

Adds the two idle-connection reads a usage indicator needs: who is signed in,
and how much quota is left, neither of which required a running thread.
Additive: nothing existing changed shape.

`SDKVersion` is `0.3.0` (was `0.2.0`).

### Added

- **`Conn.AccountRateLimits(ctx)`** — reads the rate-limit snapshot on demand
  via `account/rateLimits/read`, returning the same `*schema.RateLimitSnapshot`
  that `RateLimitsUpdatedEvent` carries.

  The event only fires while a turn is running, so anything built on it goes
  blank when the connection is idle. This is the pull counterpart: it works on
  a connection that has never started a thread, which is what a usage
  indicator needs.

  Verified end-to-end against codex 0.148.0 on a live ChatGPT account with no
  thread open: both windows, plan type, credits and spend-control state came
  back populated.

- **`Conn.Account(ctx)`** — reads the signed-in account via `account/read`.
  Returns the new `*schema.Account`, a union discriminated on `Type`
  (`chatgpt` / `apiKey` / `amazonBedrock`); `Email` and `PlanType` are
  populated for `chatgpt` only. It never asks the server to refresh
  credentials.

- **`ErrMethodNotSupported`** — returned by both reads when the app-server
  does not implement the method, so a consumer degrades the feature to
  "unavailable" instead of reporting a failure.

  codex rejects an unrecognised method while deserializing its request union,
  so it answers `-32600 "Invalid request: unknown variant ..."` rather than
  the JSON-RPC spec's `-32601`. Classification matches both, plus the
  `requires experimentalApi capability` gate — and deliberately does *not*
  match a plain `-32600` for malformed params, which stays a real error.

- **`ErrNotSignedIn`** — returned by `Conn.Account` when the server reports a
  null account, rather than a nil-nil return the caller has to remember to
  check.

- **`schema.AccountRateLimitsReadResponse`** models the full reply, including
  the per-`limitId` bucket map and the reset-credit block. Neither is surfaced
  on `Conn` yet; the reset-credit shape stays `json.RawMessage` because it is
  still moving between codex releases.

- **`BidiFixtureExecutor.SendErrorResponse`** for scripting server-side
  rejections in tests.

## [0.2.0] - 2026-08-24

Closes the last asymmetry with `claudecli-go`, which shipped these two in
v0.6.0/v0.7.0. Additive: nothing existing changed shape.

`SDKVersion` is `0.2.0` (was `0.1.0`). It is sent as
`initialize.params.clientInfo.version` and is now also the User-Agent
`LatestPublished` identifies itself with.

### Added

- **`Update`** — runs codex's own updater for the install on PATH, and reports
  what actually happened.

  Verified end-to-end against codex 0.148.0 → 0.149.1 on a synthetic standalone
  install in a sandboxed `CODEX_HOME`: the real installer ran, the version
  moved, `Changed` came back true.

  Four things it refuses to get wrong, each of them learned rather than
  designed:

  - **Only the standalone install is updated from here.** Every other method
    comes back as `*ManualUpdateError` carrying `InstallInfo.UpdateCmd`
    **verbatim** — a normal outcome, and the common one in the wild. An empty
    `Command` means "tell the user to update manually" and must never be filled
    in with a guess. This is narrower than `codex update` itself, which also
    shells out to `npm install -g @openai/codex` for a node-managed install:
    codex reports `managed package root` and `npm update target` separately
    because they can differ, and when they do that "update" writes a second
    copy whose visibility depends on PATH order.

  - **It executes the PATH entry** recorded by detection, never the bare word
    `codex` (a second `exec.LookPath` can reach a different copy — `Doctor`
    reports two on an ordinary machine) and never the symlink-resolved path
    (which is the very release the update supersedes).

  - **Success is verified by re-reading the version, never by the exit code.**
    `codex update` was observed exiting 0 and printing "Update ran
    successfully!" while the command it shells out to was not installed at all.
    `VersionBefore`/`VersionAfter`/`Changed` are the answer; `ExitCode` is
    diagnostics.

  - **A writability preflight** returns `ErrUpdateNotWritable` before anything
    runs, because "cannot" and "tried and failed" render differently: one never
    offers the button, the other shows an error after a click. It probes the two
    directories the installer actually writes —
    `<CODEX_HOME>/packages/standalone/releases` and `$CODEX_INSTALL_DIR`
    (default `~/.local/bin`) — neither of which is necessarily the directory
    holding the binary on PATH.

  The result is returned **alongside** the error, never instead of it: a
  half-run update still has before/after numbers worth rendering.

  Surface: `Update(ctx, opts...)`, `(*Client).Update`, `UpdateResult`,
  `WithUpdateProgress` (plain-text lines, one per line the installer prints),
  `WithUpdateOutput`, `WithUpdateTimeout`, `ErrManualUpdate` /
  `ManualUpdateError`, `ErrUpdateNotWritable` / `UpdateNotWritableError`,
  `ErrUpdateFailed` / `UpdateFailedError`.

- **`LatestPublished`** — the published version for an install's own release
  stream, in one HTTP request.

  `Doctor` already answers this, but spends a CLI spawn, DNS and a WebSocket to
  the model provider (~1.2s) getting there. This reads the registry or release
  feed directly, chosen from the detected install: the npm registry's dist-tags
  for node-managed installs (never `npm view` — a server has no npm on PATH),
  codex's own `releases.openai.com/codex/channels/latest` for standalone with
  the GitHub releases API as the mirror the installer also accepts, and the
  Homebrew cask API for brew. Everything else returns `ErrPublishedUnknown`.

  **The verdict is three states, not a bool.** `Published.Status` is `behind`,
  `current`, or unknown — and unknown is the zero value, so a half-built result
  cannot read as good news. `Status.Known()` gates anything reassuring, and
  `StatusReason` says why there is no verdict. `Version` is reported either way,
  because "what is published?" stays a fair question when "am I behind?" has no
  honest answer. `claudecli-go` shipped a bare `UpdateAvailable bool` in v0.6.0
  and had to fix it in v0.7.0; the codex-specific version of that trap is the
  `alpha`/`beta` dist-tags, where semver would happily call a prerelease install
  "behind" a release it never tracked.

  `ErrPublishedUnknown` means "no trustworthy source for this install, and no
  substitute would be correct" — never "the lookup failed". A DNS failure or a
  503 is an ordinary wrapped error, because that one is worth retrying and this
  one never is.

  Surface: `LatestPublished(ctx, opts...)`, `(*Client).LatestPublished`,
  `Published`, `VersionStatus` (`VersionStatusUnknown`, `VersionStatusCurrent`,
  `VersionStatusBehind`) with `Known()`, `PublishedSource`,
  `ErrPublishedUnknown` / `PublishedUnknownError`, `WithPublishedHTTPClient`,
  `WithPublishedTimeout`.

### Notes for consumers

- `agentkit` maps these onto `InstallUpdatable` / `UpdateInstall` and
  `PublishedVersionReportable` / `PublishedVersion`. Its `manual` and `blocked`
  outcomes are `ErrManualUpdate` and `ErrUpdateNotWritable`; its `unverified` is
  a non-nil result with an empty `VersionAfter`.
- Tests need no codex on PATH: classification stays pure and the ambient
  operations (writability probe, updater exec, version probe, HTTP client) are
  injected, mirroring `claudecli-go`'s `updateEnv` / `installEnv`.

## [0.1.0] - 2026-08-24

First tagged release. Earlier consumers pinned commit SHAs; pin `@v0.1.0` from
here on. No breaking changes — everything below is additive.

### Added

- **`DetectInstall`** — read-only detection of how the `codex` CLI on PATH was
  installed, plus the command that updates *that* install.

  A host that wants to tell a user "you're on 0.148.0, 0.149.1 is published"
  also has to tell them how to update, and getting that wrong does not fail
  cleanly: `npm install -g @openai/codex` against a standalone install writes a
  second, complete copy into an npm prefix, and whichever copy PATH reaches
  first from then on is the one that answers `codex --version`. The version
  probe stops describing the binary that actually runs, and the copy the user
  reaches is still stale. Because that failure is silent, `InstallUnknown` with
  an empty `UpdateCmd` is the deliberate answer whenever the evidence is
  inconclusive.

  Offline and read-only: `exec.LookPath` + `filepath.EvalSymlinks`, package
  metadata next to the resolved path, codex's standalone-install symlink under
  `CODEX_HOME`, and one `codex --version`. No network, no session, no writes.

  Surface: `CLIPackageName`, `InstallMethod` (`InstallNPMGlobal`,
  `InstallVersionManager`, `InstallPackageManager`, `InstallNative`,
  `InstallUnknown`), `InstallSource`, `InstallInfo`, `ErrCLINotFound`,
  `CLINotFoundError`, `DetectInstall(ctx, opts...)` and
  `(*Client).DetectInstall`. Mirrors `claudecli-go`'s `install.go` so the two
  libraries read alike.

  Update commands were verified against codex 0.148.0 by running `codex update`
  over synthetic layouts in a throwaway container:

  | `Method` | Layout | `UpdateCmd` |
  |---|---|---|
  | `InstallNative` | `$CODEX_HOME/packages/standalone/releases/<v>` | `codex update` — re-runs the standalone installer |
  | `InstallNPMGlobal` | a `node_modules/@openai/codex` tree | `npm install -g` / `pnpm add -g` / `bun install -g …@latest`, per `PackageManager` |
  | `InstallPackageManager` | Homebrew cask, winget, mise | `brew upgrade --cask codex`, `winget upgrade OpenAI.Codex`, `mise upgrade codex` (asdf: none) |
  | `InstallVersionManager` | an fnm/nvm/volta root, no package metadata | none — the version manager owns the directory |
  | `InstallUnknown` | anything else, including a bare binary in a bin dir | none — `codex update` refuses these too |

- **`Doctor`** — `codex doctor --json` projected into a typed `*DoctorReport`,
  with `Installation` and `Updates` covering the two checks a consumer needs:
  the executable codex is actually running, which package manager owns it, the
  published version, and every codex copy found on PATH.

  Kept a separate entry point from `DetectInstall` because it **touches the
  network** — a provider WebSocket handshake and a registry lookup, ~1.4s wall
  clock on a healthy machine, however long the timeouts take on a broken one.
  Choose deliberately: a launch-time probe wants `DetectInstall`, a "diagnose
  my install" button wants this. Observed not to modify anything under
  `CODEX_HOME` across repeated runs, but that is an observation, not a
  guarantee codex offers.

  Surface: `Doctor(ctx, opts...)`, `(*Client).Doctor`, `DoctorReport`,
  `DoctorCheck`, `DoctorInstallation`, `DoctorUpdates`, `DoctorSchemaVersion`,
  `DoctorCheckInstallation`, `DoctorCheckUpdates`, `ErrDoctorFailed`.

- `(*Client).binaryPath` semantics are now exercised: `WithBinaryPath` is
  honoured by both new entry points, and a non-local `Executor` falls back to
  `codex`.

### Notes for consumers

- **The binary on PATH is not the binary that runs.** For an npm install, PATH
  points at a JS wrapper (`…/@openai/codex/bin/codex.js`) that spawns a vendored
  musl binary four directories deeper. Both walk up to a `package.json` naming
  `@openai/codex` — the platform sub-package is an npm alias of the same name —
  so that shared ancestor is the primary classifier and both entry points
  classify identically. `InstallInfo.RealPath` is the wrapper;
  `DoctorReport.Installation.CurrentExecutable` is the vendored binary. They
  differ by design, not by disagreement.

- **`InstallInfo.VersionManager` is set even for npm installs.** A global npm
  install hosted by fnm or nvm only updates for the node version currently
  active.

- **`InstallNative` means codex's standalone-installer layout only**, not any
  standalone binary. A bare executable in an ordinary bin dir is
  `InstallUnknown` with no command: codex itself reports it as "other" and
  `codex update` refuses it outright. This deliberately differs from
  `claudecli-go`, where a bare executable *is* native.

- **`DoctorReport` details are display labels, not an API.** Each check's
  `details` is a `map[string]string` of human-readable label to human-readable
  value, so any key can be renamed by a codex release that never bumps
  `schemaVersion`. The typed projections degrade to zero values rather than
  guess; check `DoctorReport.SchemaSupported`, and read `DoctorReport.Checks`
  for the payload verbatim. `GeneratedAt` is kept as a string because codex
  0.148.0 emits `"1787550860s since unix epoch"`, not RFC 3339.

- **Windows classification is unverified on real hardware.** A `.cmd`/`.bat`/
  `.ps1` shim is not a symlink and cannot be resolved further, so unless a
  sibling npm layout confirms the install it is reported as `InstallUnknown`
  rather than guessed at.

- **The winget package id `OpenAI.Codex` does not come from codex.** OpenAI
  publishes no winget package and the codex binary contains no winget strings;
  the id comes from the winget community repository. Everything else
  (`brew upgrade --cask codex`, the npm/pnpm/bun commands) is codex's own
  vocabulary.

### Changed

- `SDKVersion` is `0.1.0` (was `0.0.2`). It is sent as
  `initialize.params.clientInfo.version`, so codex-side logs will show the new
  value.
- README documents both entry points and gains an `install.go` / `doctor.go`
  row in the architecture table.

[Unreleased]: https://github.com/allbin/codexcli-go/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/allbin/codexcli-go/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/allbin/codexcli-go/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/allbin/codexcli-go/releases/tag/v0.1.0
