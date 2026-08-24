package codexcli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// doctorFixture is a live `codex doctor --json` capture from codex 0.148.0,
// with home paths rewritten. It is the ground truth for the label names the
// typed projections read.
func doctorFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "doctor_0148.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const (
	fixtureNPMRoot = "/home/u/.local/share/fnm/node-versions/v24.14.1/installation/lib/node_modules/@openai/codex"
	fixtureExe     = fixtureNPMRoot + "/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex"
)

func TestParseDoctorReport_LiveFixture(t *testing.T) {
	report, err := parseDoctorReport(doctorFixture(t))
	if err != nil {
		t.Fatalf("parseDoctorReport: %v", err)
	}

	if report.SchemaVersion != DoctorSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", report.SchemaVersion, DoctorSchemaVersion)
	}
	if !report.SchemaSupported {
		t.Error("SchemaSupported = false for the version this package targets")
	}
	if report.OverallStatus != "ok" {
		t.Errorf("OverallStatus = %q, want %q", report.OverallStatus, "ok")
	}
	if report.CodexVersion != "0.148.0" {
		t.Errorf("CodexVersion = %q, want %q", report.CodexVersion, "0.148.0")
	}
	// codex does not emit RFC 3339 here; the field is kept verbatim precisely
	// because there is no stable format to parse.
	if report.GeneratedAt != "1787550860s since unix epoch" {
		t.Errorf("GeneratedAt = %q, want it verbatim", report.GeneratedAt)
	}
	if len(report.Checks) < 10 {
		t.Errorf("Checks has %d entries, want the whole map", len(report.Checks))
	}
	// A null remediation must land as "", not break the decode.
	if got := report.Checks[DoctorCheckInstallation].Remediation; got != "" {
		t.Errorf("Remediation = %q, want empty for a null payload value", got)
	}
	if got := report.Checks["git.environment"].DurationMs; got != 40 {
		t.Errorf("git.environment DurationMs = %d, want 40", got)
	}

	wantInstall := DoctorInstallation{
		Status:             "ok",
		Summary:            "installation looks consistent",
		CurrentExecutable:  fixtureExe,
		ManagedBy:          "npm",
		ManagedPackageRoot: fixtureNPMRoot,
		NPMUpdateTarget:    fixtureNPMRoot,
		PathEntries: []string{
			"/run/user/1001/fnm_multishells/1120274_1787519337480/bin/codex",
			"/home/u/.local/bin/codex",
		},
	}
	got := report.Installation
	got.InstallContext = "" // long, free-form, and display-only
	if !reflect.DeepEqual(got, wantInstall) {
		t.Errorf("Installation = %+v\nwant %+v", got, wantInstall)
	}
	if report.Installation.InstallContext == "" {
		t.Error("InstallContext empty, want codex's own description of the install")
	}

	wantUpdates := DoctorUpdates{
		Status:              "ok",
		Summary:             "update configuration is locally consistent",
		LatestVersion:       "0.149.1",
		LatestVersionStatus: "newer version is available",
		UpdateAction:        "npm install -g @openai/codex",
		NPMUpdateTarget:     fixtureNPMRoot,
		CachedLatestVersion: "0.148.0",
		LastCheckedAt:       "2026-08-19T07:22:26.587466717Z",
		VersionCache:        "/home/u/.codex/version.json",
	}
	if !reflect.DeepEqual(report.Updates, wantUpdates) {
		t.Errorf("Updates = %+v\nwant %+v", report.Updates, wantUpdates)
	}
}

// TestParseDoctorReport_MultipleCopiesOnPath is the state a consumer most
// needs surfaced: two codex binaries on PATH, so the version one of them
// reports need not describe the one the next session runs.
func TestParseDoctorReport_MultipleCopiesOnPath(t *testing.T) {
	report, err := parseDoctorReport(doctorFixture(t))
	if err != nil {
		t.Fatalf("parseDoctorReport: %v", err)
	}
	if len(report.Installation.PathEntries) != 2 {
		t.Fatalf("PathEntries = %v, want both copies", report.Installation.PathEntries)
	}
	// The declared count must agree with what we enumerated — if it ever
	// stops agreeing, codex listed more copies than it labelled.
	if got := report.Checks[DoctorCheckInstallation].Details["PATH codex entries"]; got != "2" {
		t.Errorf("PATH codex entries = %q, want %q", got, "2")
	}
}

// TestParseDoctorReport_UnsupportedSchema locks the degrade rule: an unknown
// schemaVersion leaves the typed projections zero rather than filling them
// from labels that may have moved, while the raw checks stay readable.
func TestParseDoctorReport_UnsupportedSchema(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal(doctorFixture(t), &payload); err != nil {
		t.Fatal(err)
	}
	payload["schemaVersion"] = DoctorSchemaVersion + 1
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	report, err := parseDoctorReport(raw)
	if err != nil {
		t.Fatalf("parseDoctorReport: %v", err)
	}
	if report.SchemaSupported {
		t.Error("SchemaSupported = true for an unknown schemaVersion")
	}
	if !reflect.DeepEqual(report.Installation, DoctorInstallation{}) {
		t.Errorf("Installation = %+v, want zero — labels may have moved", report.Installation)
	}
	if report.Updates != (DoctorUpdates{}) {
		t.Errorf("Updates = %+v, want zero", report.Updates)
	}
	if _, ok := report.Checks[DoctorCheckInstallation]; !ok {
		t.Error("Checks dropped; the raw payload must stay readable at any schema version")
	}
}

// TestParseDoctorReport_RenamedLabels covers the failure mode the stringly
// typed details map makes possible: codex renames a display label without
// touching schemaVersion. Every affected field must come back empty rather
// than carrying a stale or wrong value.
func TestParseDoctorReport_RenamedLabels(t *testing.T) {
	raw := []byte(`{
	  "schemaVersion": 1,
	  "generatedAt": "whenever",
	  "overallStatus": "ok",
	  "codexVersion": "0.999.0",
	  "checks": {
	    "installation": {
	      "id": "installation", "category": "install", "status": "ok",
	      "summary": "installation looks consistent",
	      "details": {
	        "running executable": "/opt/codex/bin/codex",
	        "managed by cargo": "true",
	        "PATH codex #2": "/usr/bin/codex"
	      },
	      "remediation": null, "durationMs": 1
	    },
	    "updates.status": {
	      "id": "updates.status", "category": "updates", "status": "warning",
	      "summary": "could not reach the registry",
	      "details": {"newest version": "1.0.0"},
	      "remediation": "check your proxy", "durationMs": 2
	    }
	  }
	}`)

	report, err := parseDoctorReport(raw)
	if err != nil {
		t.Fatalf("parseDoctorReport: %v", err)
	}
	// Status and Summary come from the check itself, so they survive.
	if report.Installation.Status != "ok" || report.Updates.Status != "warning" {
		t.Errorf("check-level fields lost: %+v / %+v", report.Installation, report.Updates)
	}
	if report.Updates.Summary != "could not reach the registry" {
		t.Errorf("Updates.Summary = %q", report.Updates.Summary)
	}
	if report.Checks[DoctorCheckUpdates].Remediation != "check your proxy" {
		t.Error("Remediation lost")
	}

	// Everything read out of the details map must be empty.
	if report.Installation.CurrentExecutable != "" {
		t.Errorf("CurrentExecutable = %q, want empty on a renamed label",
			report.Installation.CurrentExecutable)
	}
	if report.Installation.ManagedBy != "" {
		t.Errorf("ManagedBy = %q, want empty for a package manager we do not know",
			report.Installation.ManagedBy)
	}
	if report.Updates.LatestVersion != "" {
		t.Errorf("LatestVersion = %q, want empty on a renamed label", report.Updates.LatestVersion)
	}
	// #2 with no #1 is a gap at the head: enumerate nothing rather than
	// silently renumber a partial list.
	if len(report.Installation.PathEntries) != 0 {
		t.Errorf("PathEntries = %v, want empty when the list does not start at #1",
			report.Installation.PathEntries)
	}
}

func TestParseDoctorReport_Failures(t *testing.T) {
	tests := []struct {
		name string
		out  string
	}{
		{"empty stdout", ""},
		{"whitespace only", "  \n\t "},
		{"not json", "error: codex doctor is not available in this build\n"},
		{"truncated json", `{"schemaVersion": 1, "checks": {`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := parseDoctorReport([]byte(tt.out))
			if report != nil {
				t.Errorf("expected nil report, got %+v", report)
			}
			if !errors.Is(err, ErrDoctorFailed) {
				t.Fatalf("expected ErrDoctorFailed, got %v", err)
			}
		})
	}
}

func TestManagedBy(t *testing.T) {
	tests := []struct {
		name    string
		details map[string]string
		want    string
	}{
		{"npm", map[string]string{"managed by npm": "true", "managed by bun": "false"}, "npm"},
		{"bun", map[string]string{"managed by npm": "false", "managed by bun": "true"}, "bun"},
		{"pnpm", map[string]string{"managed by pnpm": "true"}, "pnpm"},
		{"none", map[string]string{"managed by npm": "false"}, ""},
		{"absent", map[string]string{}, ""},
		{"nil map", nil, ""},
		{"non-boolean value", map[string]string{"managed by npm": "probably"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := managedBy(tt.details); got != tt.want {
				t.Errorf("managedBy(%v) = %q, want %q", tt.details, got, tt.want)
			}
		})
	}
}

func TestPathEntries(t *testing.T) {
	tests := []struct {
		name    string
		details map[string]string
		want    []string
	}{
		{"none", map[string]string{}, nil},
		{
			name:    "single copy",
			details: map[string]string{"PATH codex #1": "/a/codex", "PATH codex entries": "1"},
			want:    []string{"/a/codex"},
		},
		{
			name: "stops at the first gap rather than skipping it",
			details: map[string]string{
				"PATH codex #1": "/a/codex",
				"PATH codex #2": "/b/codex",
				"PATH codex #4": "/d/codex",
			},
			want: []string{"/a/codex", "/b/codex"},
		},
		{
			name:    "an empty value is a gap",
			details: map[string]string{"PATH codex #1": "/a/codex", "PATH codex #2": ""},
			want:    []string{"/a/codex"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathEntries(tt.details); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("pathEntries(%v) = %v, want %v", tt.details, got, tt.want)
			}
		})
	}
}

func TestWithCodexHome(t *testing.T) {
	base := map[string]string{"FOO": "bar"}
	got := withCodexHome(base, "/opt/cx")
	if got["CODEX_HOME"] != "/opt/cx" || got["FOO"] != "bar" {
		t.Errorf("withCodexHome = %v", got)
	}
	if _, ok := base["CODEX_HOME"]; ok {
		t.Error("withCodexHome mutated the caller's map")
	}
	if got := withCodexHome(nil, "/opt/cx"); got["CODEX_HOME"] != "/opt/cx" {
		t.Errorf("withCodexHome(nil) = %v", got)
	}
}
