package codexcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// npm on Windows installs the CLI behind a cmd.exe shim (`codex.cmd`), so
// exec.LookPath resolves to a batch file rather than the CLI itself. Running
// that shim has two costs: os/exec refuses to start .bat/.cmd files whose
// arguments cmd.exe cannot safely escape (the CVE-2024-24576 hardening — any
// `%`, `"` or newline in an arg fails at Start), and the shim puts a cmd.exe
// layer into the process tree. findShimEntryJS resolves the shim to the JS
// entry script it wraps so the executor can run node directly, exactly as
// the shim itself would.
//
// The logic lives in a platform-neutral file so it stays testable off
// Windows; only executor_windows.go calls it.

// findShimEntryJS reports the CLI entry script an npm shim wraps. It trusts
// the same layout evidence npmEvidence does — a package.json for
// CLIPackageName beside the shim's node_modules tree, or under the
// unix-style ../lib prefix — and only a confirmed CLI package yields a
// result: a foreign shim, or one whose package cannot be read, runs as-is.
func findShimEntryJS(shimPath string) (string, bool) {
	switch strings.ToLower(filepath.Ext(shimPath)) {
	case ".cmd", ".bat", ".ps1":
	default:
		return "", false
	}
	shimDir := filepath.Dir(shimPath)
	for _, rel := range []string{
		filepath.Join("node_modules", filepath.FromSlash(CLIPackageName)),
		filepath.Join("..", "lib", "node_modules", filepath.FromSlash(CLIPackageName)),
	} {
		pkgDir := filepath.Join(shimDir, rel)
		entry, ok := cliPackageEntry(filepath.Join(pkgDir, "package.json"))
		if !ok {
			continue
		}
		entryJS := filepath.Join(pkgDir, filepath.FromSlash(entry))
		if info, err := os.Stat(entryJS); err == nil && !info.IsDir() {
			return entryJS, true
		}
	}
	return "", false
}

// cliPackageEntry reads a package.json and, when it describes the CLI
// package, reports the bin entry script the shim would run. The bin field
// is either a bare string or a name→script map; "bin/codex.js" is the
// fallback when the field is missing or unreadable, matching the package's
// layout.
func cliPackageEntry(pkgFile string) (string, bool) {
	b, err := os.ReadFile(pkgFile)
	if err != nil {
		return "", false
	}
	var pkg struct {
		Name string          `json:"name"`
		Bin  json.RawMessage `json:"bin"`
	}
	if json.Unmarshal(b, &pkg) != nil || pkg.Name != CLIPackageName {
		return "", false
	}
	if len(pkg.Bin) > 0 {
		var s string
		if json.Unmarshal(pkg.Bin, &s) == nil && s != "" {
			return s, true
		}
		var m map[string]string
		if json.Unmarshal(pkg.Bin, &m) == nil {
			if s := m["codex"]; s != "" {
				return s, true
			}
		}
	}
	return "bin/codex.js", true
}
