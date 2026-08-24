package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DoctorSchemaVersion is the `schemaVersion` this package knows how to project
// into typed fields. codex 0.148.0 emits 1.
//
// Everything below the top level of a doctor payload is display-oriented: the
// per-check `details` are a map of human-readable label to human-readable
// value, so any key can be renamed by a codex release that never touches this
// number. The version is therefore a floor, not a guarantee — see
// DoctorReport.SchemaSupported.
const DoctorSchemaVersion = 1

// Check ids in DoctorReport.Checks that this package projects into typed
// fields. The full map is always available for the rest.
const (
	DoctorCheckInstallation = "installation"
	DoctorCheckUpdates      = "updates.status"
)

// defaultDoctorTimeout bounds `codex doctor --json` when the caller's context
// has no deadline. Observed wall clock on a healthy machine is ~1.2s, most of
// it network; the ceiling is generous because an unreachable provider is
// exactly the condition a caller runs this to diagnose.
const defaultDoctorTimeout = 60 * time.Second

// ErrDoctorFailed is returned when `codex doctor --json` could not be run or
// produced no parseable report. A report whose *checks* failed is not this
// error — that is a successful diagnosis of a broken machine, and comes back
// as a report with a non-"ok" OverallStatus.
var ErrDoctorFailed = errors.New("codexcli: codex doctor failed")

// DoctorReport is the machine-readable report `codex doctor --json` prints:
// codex's own account of its installation, config, auth and runtime health.
type DoctorReport struct {
	// SchemaVersion is the payload's declared version.
	SchemaVersion int `json:"schemaVersion"`

	// SchemaSupported reports whether SchemaVersion is one this package
	// projects. When false, Checks is still populated verbatim but Installation
	// and Updates are left zero rather than filled from labels that may have
	// moved. Check this before trusting the typed projections.
	SchemaSupported bool `json:"-"`

	// GeneratedAt is codex's timestamp, verbatim. It is not RFC 3339: codex
	// 0.148.0 emits strings like "1787550860s since unix epoch". It is kept as
	// a string precisely because there is no stable format to parse.
	GeneratedAt string `json:"generatedAt"`

	// OverallStatus is codex's roll-up ("ok", "warning", "issues", ...).
	OverallStatus string `json:"overallStatus"`

	// CodexVersion is the version of the binary that produced the report —
	// which is the binary that actually ran, not whatever else is on PATH.
	CodexVersion string `json:"codexVersion"`

	// Checks is every check keyed by id, verbatim. Ids beyond the two
	// projected below are not enumerated here because codex adds and renames
	// them freely; iterate the map to surface them all.
	Checks map[string]DoctorCheck `json:"checks"`

	// Installation and Updates are typed projections of the two checks a
	// consumer showing "which codex will I run, and how do I update it" needs.
	// Every field is best-effort: an absent check or a renamed label yields a
	// zero value, never a wrong one.
	Installation DoctorInstallation `json:"-"`
	Updates      DoctorUpdates      `json:"-"`
}

// DoctorCheck is one entry of the report's `checks` map.
type DoctorCheck struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Status   string `json:"status"`
	Summary  string `json:"summary"`

	// Details is a map of display label to display value — both sides are
	// human-facing strings chosen for rendering, not an API. Read it with
	// that in mind: treat every key as optional, and never parse a value
	// whose absence you cannot tolerate.
	Details map[string]string `json:"details"`

	// Remediation is codex's suggested fix, or "" when it has none (the
	// payload carries JSON null there).
	Remediation string `json:"remediation"`

	DurationMs int `json:"durationMs"`
}

// DoctorInstallation projects the `installation` check.
type DoctorInstallation struct {
	// Status and Summary come from the check itself.
	Status  string
	Summary string

	// CurrentExecutable is the binary codex is actually running. For an npm
	// install this is the vendored native binary, several directories below
	// the JS wrapper that PATH points at — so it will not equal
	// InstallInfo.RealPath, and that is not a disagreement.
	CurrentExecutable string

	// InstallContext is codex's own one-line description of the install
	// ("npm (package ..., bin ..., resources ...)", "standalone (unix, ...)",
	// "other"). Display it; do not parse it.
	InstallContext string

	// ManagedBy is "npm", "bun" or "pnpm" when codex reports that package
	// manager as owning the install, else "". Derived from the mutually
	// exclusive "managed by npm" / "managed by bun" / "managed by pnpm"
	// booleans the JS shim sets via CODEX_MANAGED_BY_* env vars.
	ManagedBy string

	// ManagedPackageRoot is the node package directory owning the install, or
	// "" when nothing does.
	ManagedPackageRoot string

	// NPMUpdateTarget is the package directory `npm install -g` would write.
	// When it differs from ManagedPackageRoot, an npm update would install
	// into a prefix other than the one being run.
	NPMUpdateTarget string

	// PathEntries is every codex on PATH, in PATH order. More than one entry
	// is the state where a version number stops describing the binary that
	// runs: whichever copy PATH reaches first answers `codex --version`, and
	// updating a different one silently changes nothing the user can see.
	//
	// The list is enumerated from codex's "PATH codex #1", "#2", ... labels
	// rather than from its "PATH codex entries" count, so a renamed count
	// label cannot truncate it.
	PathEntries []string
}

// DoctorUpdates projects the `updates.status` check. This is the only part of
// either report that involves the network: codex queries the npm registry (or
// the Homebrew cask API, or the GitHub releases API) for the published
// version. Fields are "" when the check is absent or its labels moved.
type DoctorUpdates struct {
	Status  string
	Summary string

	// LatestVersion is the published version codex just fetched, and
	// LatestVersionStatus its verdict on it ("newer version is available",
	// "current version is not older").
	LatestVersion       string
	LatestVersionStatus string

	// UpdateAction is codex's own answer to "how would I update this",
	// expressed in its vocabulary: "npm install -g @openai/codex",
	// "standalone installer", "brew upgrade --cask codex", "manual or
	// unknown". Compare it against InstallInfo.UpdateCmd rather than
	// substituting it — DetectInstall reports a command the user can run,
	// where "standalone installer" is a description.
	UpdateAction string

	// NPMUpdateTarget is the package directory `npm install -g` would write.
	NPMUpdateTarget string

	// CachedLatestVersion and LastCheckedAt describe codex's on-disk version
	// cache ($CODEX_HOME/version.json), whose path is VersionCache. A cached
	// value older than LatestVersion means this run refreshed it.
	CachedLatestVersion string
	LastCheckedAt       string
	VersionCache        string
}

// Doctor runs `codex doctor --json` with the default client's binary and
// returns codex's own account of its health.
//
// # This touches the network
//
// Unlike [DetectInstall], which is offline and read-only, Doctor spawns the
// CLI and lets it do everything it does: it resolves DNS, opens a WebSocket to
// the model provider, and queries a package registry for the published
// version. On a healthy machine that is ~400ms of network inside ~1.2s of wall
// clock (measured against codex 0.148.0); on a machine with a black-holed
// proxy it is however long the timeouts take. Pick between the two
// deliberately — a UI that probes on every launch wants DetectInstall, and a
// "diagnose my install" button wants this.
//
// It was *observed* not to modify anything under CODEX_HOME across repeated
// runs — not even version.json, which it reads but does not refresh with the
// version it just fetched. That is an observation of one version's behaviour,
// not a contract codex offers. Do not rely on it being read-only.
//
// # Trusting the result
//
// Only the top level of the payload is structured. Each check's `details` is a
// map of display label to display value, so every key this package reads can
// be renamed by a codex release without any version bump. The typed
// projections therefore degrade to zero values rather than guess, and
// DoctorReport.SchemaSupported reports whether they were attempted at all.
// DoctorReport.Checks always holds the payload verbatim.
//
// A report whose checks failed is a success: it comes back with a non-"ok"
// OverallStatus and no error. ErrDoctorFailed means the command could not be
// run or printed nothing parseable.
func Doctor(ctx context.Context, opts ...Option) (*DoctorReport, error) {
	return defaultInstallClient.Doctor(ctx, opts...)
}

// Doctor runs `codex doctor --json` with this client's binary, working
// directory, environment and CODEX_HOME. See the package-level [Doctor] for
// the full contract, including that it touches the network.
func (c *Client) Doctor(ctx context.Context, opts ...Option) (*DoctorReport, error) {
	resolved := resolveOptions(c.defaults, opts)

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultDoctorTimeout)
		defer cancel()
	}

	env := resolved.env
	if resolved.codexHome != "" {
		env = withCodexHome(env, resolved.codexHome)
	}

	cmd := exec.CommandContext(ctx, c.binaryPath(), "doctor", "--json")
	cmd.Env = buildEnv(env)
	if resolved.workDir != "" {
		cmd.Dir = resolved.workDir
	}
	hideConsoleWindow(cmd)

	// A non-zero exit is expected when checks fail, and the report is still on
	// stdout. Only treat it as a failure if nothing parseable came back.
	out, runErr := cmd.Output()
	report, parseErr := parseDoctorReport(out)
	if parseErr != nil {
		return nil, errors.Join(doctorRunError(runErr), parseErr)
	}

	c.log().Debug("codex doctor",
		"schemaVersion", report.SchemaVersion, "supported", report.SchemaSupported,
		"overallStatus", report.OverallStatus, "codexVersion", report.CodexVersion,
		"checks", len(report.Checks), "pathEntries", len(report.Installation.PathEntries),
		"updateAction", report.Updates.UpdateAction)
	return report, nil
}

// withCodexHome returns overrides with CODEX_HOME set, without mutating the
// caller's map.
func withCodexHome(overrides map[string]string, home string) map[string]string {
	merged := make(map[string]string, len(overrides)+1)
	for k, v := range overrides {
		merged[k] = v
	}
	merged["CODEX_HOME"] = home
	return merged
}

// doctorRunError wraps the exec failure, keeping any captured stderr, or
// returns nil when the command itself ran fine.
func doctorRunError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		stderr := strings.TrimSpace(string(ee.Stderr))
		if len(stderr) > 512 {
			stderr = stderr[:512] + "..."
		}
		return fmt.Errorf("%w: %w: %s", ErrDoctorFailed, err, stderr)
	}
	return fmt.Errorf("%w: %w", ErrDoctorFailed, err)
}

func parseDoctorReport(out []byte) (*DoctorReport, error) {
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, fmt.Errorf("%w: no report on stdout", ErrDoctorFailed)
	}
	var report DoctorReport
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("%w: parse report: %w", ErrDoctorFailed, err)
	}

	report.SchemaSupported = report.SchemaVersion == DoctorSchemaVersion
	if report.SchemaSupported {
		report.Installation = projectInstallation(report.Checks[DoctorCheckInstallation])
		report.Updates = projectUpdates(report.Checks[DoctorCheckUpdates])
	}
	return &report, nil
}

func projectInstallation(check DoctorCheck) DoctorInstallation {
	d := check.Details
	return DoctorInstallation{
		Status:             check.Status,
		Summary:            check.Summary,
		CurrentExecutable:  d["current executable"],
		InstallContext:     d["install context"],
		ManagedBy:          managedBy(d),
		ManagedPackageRoot: d["managed package root"],
		NPMUpdateTarget:    d["npm update target"],
		PathEntries:        pathEntries(d),
	}
}

func projectUpdates(check DoctorCheck) DoctorUpdates {
	d := check.Details
	return DoctorUpdates{
		Status:              check.Status,
		Summary:             check.Summary,
		LatestVersion:       d["latest version"],
		LatestVersionStatus: d["latest version status"],
		UpdateAction:        d["update action"],
		NPMUpdateTarget:     d["npm update target"],
		CachedLatestVersion: d["cached latest version"],
		LastCheckedAt:       d["last checked at"],
		VersionCache:        d["version cache"],
	}
}

// managedBy reads the mutually exclusive "managed by <pm>" booleans. codex
// prints them as the strings "true"/"false"; anything else counts as false.
func managedBy(details map[string]string) string {
	for _, pm := range []string{"npm", "bun", "pnpm"} {
		if strings.EqualFold(strings.TrimSpace(details["managed by "+pm]), "true") {
			return pm
		}
	}
	return ""
}

// maxDoctorPathEntries bounds the "PATH codex #N" walk. A machine with more
// than this many copies of codex on PATH has a problem the list length is not
// going to clarify.
const maxDoctorPathEntries = 64

// pathEntries collects "PATH codex #1", "#2", ... in order, stopping at the
// first gap. The sibling "PATH codex entries" count is deliberately not
// consulted: enumerating cannot report more copies than codex listed, whereas
// trusting a renamed or stale count could report fewer.
func pathEntries(details map[string]string) []string {
	var entries []string
	for i := 1; i <= maxDoctorPathEntries; i++ {
		v, ok := details["PATH codex #"+strconv.Itoa(i)]
		if !ok || v == "" {
			break
		}
		entries = append(entries, v)
	}
	return entries
}
