# Changelog

All notable changes to `codexcli-go` are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Install the latest release with:

```
go get github.com/allbin/codexcli-go@latest
```

or pin a specific version (e.g. `@v0.1.0`).

## [Unreleased]

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

[Unreleased]: https://github.com/allbin/codexcli-go/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/allbin/codexcli-go/releases/tag/v0.1.0
