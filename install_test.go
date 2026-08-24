package codexcli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeInstallEnv builds an installEnv backed by an in-memory file map, so
// classification can be exercised over synthetic path layouts without any
// codex CLI on the machine. evalSymlink resolves nothing by default; tests
// that need the CODEX_HOME standalone record override it.
func fakeInstallEnv(files map[string]string) installEnv {
	return installEnv{
		lookPath:    func(s string) (string, error) { return s, nil },
		evalSymlink: func(string) (string, error) { return "", os.ErrNotExist },
		readFile: func(name string) ([]byte, error) {
			// Callers build paths with path.Join, which already cleans them.
			if content, ok := files[name]; ok {
				return []byte(content), nil
			}
			return nil, os.ErrNotExist
		},
		runVersion: func(context.Context, string) (string, error) { return "0.148.0", nil },
		codexHome:  "/home/u/.codex",
	}
}

const cliPackageJSON = `{"name":"@openai/codex","version":"0.148.0"}`

// platformPackageJSON is what the vendored platform sub-package publishes.
// It is an npm alias of the same package, so `name` is identical and the
// walk-up finds it from the native binary buried under vendor/.
const platformPackageJSON = `{"name":"@openai/codex","version":"0.148.0-linux-x64"}`

// The two real paths a codex npm install presents, captured from a live fnm
// install of codex 0.148.0: the JS wrapper the PATH symlink points at, and the
// vendored musl binary that wrapper actually spawns.
const (
	fnmPkgRoot = "/home/u/.local/share/fnm/node-versions/v24.14.1/installation/lib/node_modules/@openai/codex"
	fnmWrapper = fnmPkgRoot + "/bin/codex.js"
	fnmVendor  = fnmPkgRoot + "/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex"
)

func TestClassifyInstall(t *testing.T) {
	tests := []struct {
		name     string
		realPath string
		files    map[string]string
		want     classification
	}{
		{
			name:     "npm global with package metadata",
			realPath: "/usr/local/lib/node_modules/@openai/codex/bin/codex.js",
			files: map[string]string{
				"/usr/local/lib/node_modules/@openai/codex/package.json": cliPackageJSON,
			},
			want: classification{InstallNPMGlobal, "", "npm", CLIPackageName, InstallSourcePackageMetadata},
		},
		{
			name:     "npm global without readable metadata falls back to layout",
			realPath: "/usr/lib/node_modules/@openai/codex/bin/codex.js",
			want:     classification{InstallNPMGlobal, "", "npm", CLIPackageName, InstallSourcePathLayout},
		},
		{
			// The live layout: the PATH symlink resolves to the JS wrapper.
			name:     "live fnm layout, JS wrapper on PATH",
			realPath: fnmWrapper,
			files:    map[string]string{fnmPkgRoot + "/package.json": cliPackageJSON},
			want:     classification{InstallNPMGlobal, "fnm", "npm", CLIPackageName, InstallSourcePackageMetadata},
		},
		{
			// The same install seen from the binary that actually runs: four
			// levels deeper, under the platform sub-package's vendor tree.
			name:     "live fnm layout, vendored musl binary",
			realPath: fnmVendor,
			files: map[string]string{
				fnmPkgRoot + "/node_modules/@openai/codex-linux-x64/package.json": platformPackageJSON,
			},
			want: classification{InstallNPMGlobal, "fnm", "npm", CLIPackageName, InstallSourcePackageMetadata},
		},
		{
			// Neither package.json readable: the shared node_modules/@openai
			// segment still identifies it, one tier weaker.
			name:     "vendored musl binary with no readable metadata",
			realPath: fnmVendor,
			want:     classification{InstallNPMGlobal, "fnm", "npm", CLIPackageName, InstallSourcePathLayout},
		},
		{
			name:     "npm global hosted by nvm keeps npm method and names the manager",
			realPath: "/home/u/.nvm/versions/node/v20.11.0/lib/node_modules/@openai/codex/bin/codex.js",
			files: map[string]string{
				"/home/u/.nvm/versions/node/v20.11.0/lib/node_modules/@openai/codex/package.json": cliPackageJSON,
			},
			want: classification{InstallNPMGlobal, "nvm", "npm", CLIPackageName, InstallSourcePackageMetadata},
		},
		{
			name:     "fnm per-shell runtime dir is still fnm",
			realPath: "/run/user/1000/fnm_multishells/1120274_1787519337480/lib/node_modules/@openai/codex/bin/codex.js",
			want:     classification{InstallNPMGlobal, "fnm", "npm", CLIPackageName, InstallSourcePathLayout},
		},
		{
			name:     "volta tool image",
			realPath: "/home/u/.volta/tools/image/packages/@openai/codex/lib/node_modules/@openai/codex/bin/codex.js",
			want:     classification{InstallNPMGlobal, "volta", "npm", CLIPackageName, InstallSourcePathLayout},
		},
		{
			name:     "bun global prefix",
			realPath: "/home/u/.bun/install/global/node_modules/@openai/codex/bin/codex.js",
			files: map[string]string{
				"/home/u/.bun/install/global/node_modules/@openai/codex/package.json": cliPackageJSON,
			},
			want: classification{InstallNPMGlobal, "", "bun", CLIPackageName, InstallSourcePackageMetadata},
		},
		{
			// pnpm is identified the way codex's own shim identifies it: an
			// owning node_modules holding .modules.yaml next to the package.
			name:     "pnpm global prefix identified by .modules.yaml",
			realPath: "/home/u/.local/share/pnpm/global/5/node_modules/@openai/codex/bin/codex.js",
			files: map[string]string{
				"/home/u/.local/share/pnpm/global/5/node_modules/@openai/codex/package.json": cliPackageJSON,
				"/home/u/.local/share/pnpm/global/5/node_modules/.modules.yaml":              "hoistPattern:\n  - '*'\n",
			},
			want: classification{InstallNPMGlobal, "", "pnpm", CLIPackageName, InstallSourcePackageMetadata},
		},
		{
			name:     "pnpm store layout without readable metadata",
			realPath: "/home/u/.pnpm-store/node_modules/.pnpm/@openai+codex@0.148.0/node_modules/@openai/codex/bin/codex.js",
			want:     classification{InstallNPMGlobal, "", "pnpm", CLIPackageName, InstallSourcePathLayout},
		},
		{
			name:     "standalone installer release tree",
			realPath: "/home/u/.codex/packages/standalone/releases/0.148.0/bin/codex",
			want:     classification{InstallNative, "", "", "", InstallSourcePathLayout},
		},
		{
			name:     "standalone installer under a relocated CODEX_HOME",
			realPath: "/opt/cx/packages/standalone/releases/0.148.0/bin/codex",
			want:     classification{InstallNative, "", "", "", InstallSourcePathLayout},
		},
		{
			name:     "homebrew cask",
			realPath: "/opt/homebrew/Caskroom/codex/0.148.0/codex",
			want:     classification{InstallPackageManager, "", "homebrew", "codex", InstallSourcePathLayout},
		},
		{
			name:     "homebrew cask keeps a non-default cask name",
			realPath: "/opt/homebrew/Caskroom/codex@beta/0.149.0/codex",
			want:     classification{InstallPackageManager, "", "homebrew", "codex@beta", InstallSourcePathLayout},
		},
		{
			name:     "winget package",
			realPath: `C:\Users\u\AppData\Local\Microsoft\WinGet\Packages\OpenAI.Codex_x\codex.exe`,
			want:     classification{InstallPackageManager, "", "winget", "OpenAI.Codex", InstallSourcePathLayout},
		},
		{
			name:     "mise managed tool",
			realPath: "/home/u/.local/share/mise/installs/codex/0.148.0/bin/codex",
			want:     classification{InstallPackageManager, "mise", "mise", "codex", InstallSourcePathLayout},
		},
		{
			name:     "asdf managed tool",
			realPath: "/home/u/.asdf/installs/codex/0.148.0/bin/codex",
			want:     classification{InstallPackageManager, "asdf", "asdf", "codex", InstallSourcePathLayout},
		},
		{
			name:     "version manager root with no packaging evidence",
			realPath: "/home/u/.nvm/versions/node/v20.11.0/bin/codex",
			want:     classification{InstallVersionManager, "nvm", "", "", InstallSourcePathLayout},
		},
		{
			name:     "windows cmd shim beside an npm prefix",
			realPath: `C:\Users\u\AppData\Roaming\npm\codex.cmd`,
			files: map[string]string{
				"C:/Users/u/AppData/Roaming/npm/node_modules/@openai/codex/package.json": cliPackageJSON,
			},
			want: classification{InstallNPMGlobal, "", "npm", CLIPackageName, InstallSourcePackageMetadata},
		},
		{
			name:     "unresolvable windows shim is unknown, not guessed",
			realPath: `C:\Users\u\bin\codex.cmd`,
			files:    map[string]string{"C:/Users/u/bin/codex.cmd": "@echo off\r\n"},
			want:     classification{InstallUnknown, "", "", "", InstallSourceNone},
		},
		{
			name:     "unresolvable powershell shim is unknown",
			realPath: `C:\Users\u\bin\codex.ps1`,
			want:     classification{InstallUnknown, "", "", "", InstallSourceNone},
		},
		{
			// codex itself reports "other" and refuses `codex update` here.
			name:     "standalone binary in an ordinary bin dir is unknown",
			realPath: "/usr/local/bin/codex",
			want:     classification{InstallUnknown, "", "", "", InstallSourceNone},
		},
		{
			name:     "unreadable path with no markers is unknown",
			realPath: "/some/where/codex",
			want:     classification{InstallUnknown, "", "", "", InstallSourceNone},
		},
		{
			name:     "directory merely containing nvm in its name is not a version manager",
			realPath: "/home/u/nvm-notes/codex",
			want:     classification{InstallUnknown, "", "", "", InstallSourceNone},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyInstall(tt.realPath, fakeInstallEnv(tt.files))
			if got != tt.want {
				t.Errorf("classifyInstall(%q) = %+v, want %+v", tt.realPath, got, tt.want)
			}
		})
	}
}

func TestUpdateCommand(t *testing.T) {
	tests := []struct {
		name           string
		method         InstallMethod
		packageManager string
		packageName    string
		want           string
	}{
		{"standalone updates itself", InstallNative, "", "", "codex update"},
		{"npm global", InstallNPMGlobal, "npm", CLIPackageName, "npm install -g @openai/codex@latest"},
		{"npm global with no manager recorded defaults to npm", InstallNPMGlobal, "", "", "npm install -g @openai/codex@latest"},
		{"pnpm global", InstallNPMGlobal, "pnpm", CLIPackageName, "pnpm add -g @openai/codex@latest"},
		{"bun global", InstallNPMGlobal, "bun", CLIPackageName, "bun install -g @openai/codex@latest"},
		{"homebrew", InstallPackageManager, "homebrew", "codex", "brew upgrade --cask codex"},
		{"homebrew without a cask name", InstallPackageManager, "homebrew", "", "brew upgrade --cask codex"},
		{"winget", InstallPackageManager, "winget", "OpenAI.Codex", "winget upgrade OpenAI.Codex"},
		{"mise", InstallPackageManager, "mise", "codex", "mise upgrade codex"},
		{"asdf has no known command", InstallPackageManager, "asdf", "codex", ""},
		{"unrecognized package manager", InstallPackageManager, "pacman", "codex", ""},
		{"version manager", InstallVersionManager, "", "", ""},
		{"unknown", InstallUnknown, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := updateCommand(tt.method, tt.packageManager, tt.packageName); got != tt.want {
				t.Errorf("updateCommand(%q, %q, %q) = %q, want %q",
					tt.method, tt.packageManager, tt.packageName, got, tt.want)
			}
		})
	}
}

// TestUpdateCommandEmptyWhenUnknown locks the rule that makes an unknown
// answer safe: no method without a verified command may emit one.
func TestUpdateCommandEmptyWhenUnknown(t *testing.T) {
	for _, m := range []InstallMethod{InstallUnknown, InstallVersionManager, ""} {
		if got := updateCommand(m, "", ""); got != "" {
			t.Errorf("updateCommand(%q) = %q, want empty — a guessed command half-installs a second copy", m, got)
		}
	}
}

func TestDetectInstall_NotFound(t *testing.T) {
	env := fakeInstallEnv(nil)
	env.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }

	info, err := detectInstall(context.Background(), "codex", env)
	if info != nil {
		t.Errorf("expected nil info, got %+v", info)
	}
	if !errors.Is(err, ErrCLINotFound) {
		t.Fatalf("expected ErrCLINotFound, got %v", err)
	}
	var nf *CLINotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected *CLINotFoundError, got %T", err)
	}
	if nf.Binary != "codex" {
		t.Errorf("Binary = %q, want %q", nf.Binary, "codex")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Error("expected the underlying lookup error to be unwrappable")
	}
}

// TestDetectInstall_StandaloneRecord covers codex's only on-disk record of a
// self-managed install: the `packages/standalone/current` symlink. It resolves
// an otherwise-unclassifiable binary, and — more importantly — flags the case
// where a second copy shadows the standalone one on PATH.
func TestDetectInstall_StandaloneRecord(t *testing.T) {
	const currentRelease = "/home/u/.codex/packages/standalone/releases/0.148.0"

	tests := []struct {
		name         string
		realPath     string
		files        map[string]string
		current      string // what packages/standalone/current resolves to, "" for absent
		wantMethod   InstallMethod
		wantSource   InstallSource
		wantConfig   string
		wantMismatch bool
		wantUpdate   string
	}{
		{
			// The release directory is itself a symlink onto another volume,
			// so the resolved path lands outside the recognizable tree. The
			// record is the only thing left that identifies it.
			name:       "record resolves a binary the path rules did not recognize",
			realPath:   "/mnt/tools/codex-releases/0.148.0/bin/codex",
			current:    "/mnt/tools/codex-releases/0.148.0",
			wantMethod: InstallNative,
			wantSource: InstallSourceConfig,
			wantConfig: "standalone",
			wantUpdate: "codex update",
		},
		{
			name:       "path layout already recognized it, so the record only corroborates",
			realPath:   currentRelease + "/bin/codex",
			current:    currentRelease,
			wantMethod: InstallNative,
			wantSource: InstallSourcePathLayout,
			wantConfig: "standalone",
			wantUpdate: "codex update",
		},
		{
			// The destructive case: `npm install -g` here updates the npm copy
			// and leaves the standalone one — which may be what PATH reaches
			// in another shell — untouched.
			name:     "npm copy shadows a standalone install",
			realPath: fnmWrapper,
			files: map[string]string{
				fnmPkgRoot + "/package.json": cliPackageJSON,
			},
			current:      currentRelease,
			wantMethod:   InstallNPMGlobal,
			wantSource:   InstallSourcePackageMetadata,
			wantConfig:   "standalone",
			wantMismatch: true,
			wantUpdate:   "npm install -g @openai/codex@latest",
		},
		{
			name:         "a stale symlink to a superseded release is a mismatch too",
			realPath:     "/home/u/.codex/packages/standalone/releases/0.140.0/bin/codex",
			current:      currentRelease,
			wantMethod:   InstallNative,
			wantSource:   InstallSourcePathLayout,
			wantConfig:   "standalone",
			wantMismatch: true,
			wantUpdate:   "codex update",
		},
		{
			name:       "no standalone install: nothing is recorded and nothing is inferred",
			realPath:   "/usr/local/bin/codex",
			wantMethod: InstallUnknown,
			wantSource: InstallSourceNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := fakeInstallEnv(tt.files)
			env.lookPath = func(string) (string, error) { return tt.realPath, nil }
			env.evalSymlink = func(p string) (string, error) {
				if filepath.ToSlash(p) == "/home/u/.codex/packages/standalone/current" {
					if tt.current == "" {
						return "", os.ErrNotExist
					}
					return tt.current, nil
				}
				return "", os.ErrNotExist
			}

			info, err := detectInstall(context.Background(), "codex", env)
			if err != nil {
				t.Fatalf("detectInstall: %v", err)
			}
			if info.Method != tt.wantMethod {
				t.Errorf("Method = %q, want %q", info.Method, tt.wantMethod)
			}
			if info.Source != tt.wantSource {
				t.Errorf("Source = %q, want %q", info.Source, tt.wantSource)
			}
			if info.ConfigMethod != tt.wantConfig {
				t.Errorf("ConfigMethod = %q, want %q", info.ConfigMethod, tt.wantConfig)
			}
			if info.ConfigMismatch != tt.wantMismatch {
				t.Errorf("ConfigMismatch = %v, want %v", info.ConfigMismatch, tt.wantMismatch)
			}
			if info.UpdateCmd != tt.wantUpdate {
				t.Errorf("UpdateCmd = %q, want %q", info.UpdateCmd, tt.wantUpdate)
			}
			if info.Version != "0.148.0" {
				t.Errorf("Version = %q, want %q", info.Version, "0.148.0")
			}
		})
	}
}

// TestDetectInstall_VersionProbeFailureIsNotFatal locks that a binary we cannot
// run still yields a usable classification.
func TestDetectInstall_VersionProbeFailureIsNotFatal(t *testing.T) {
	env := fakeInstallEnv(map[string]string{
		fnmPkgRoot + "/package.json": cliPackageJSON,
	})
	env.lookPath = func(string) (string, error) { return fnmWrapper, nil }
	env.runVersion = func(context.Context, string) (string, error) {
		return "", errors.New("exec format error")
	}

	info, err := detectInstall(context.Background(), "codex", env)
	if err != nil {
		t.Fatalf("detectInstall: %v", err)
	}
	if info.Version != "" {
		t.Errorf("Version = %q, want empty", info.Version)
	}
	if info.Method != InstallNPMGlobal {
		t.Errorf("Method = %q, want %q", info.Method, InstallNPMGlobal)
	}
	if info.UpdateCmd != "npm install -g @openai/codex@latest" {
		t.Errorf("UpdateCmd = %q", info.UpdateCmd)
	}
}

// TestDetectInstall_SymlinkChain walks a real symlink chain over the layout
// captured from the dev box: ~/.local/bin/codex -> the JS wrapper inside an
// fnm-hosted npm prefix. Only the resolved path reveals what the shim is.
func TestDetectInstall_SymlinkChain(t *testing.T) {
	root := t.TempDir()

	pkgDir := filepath.Join(root, ".local", "share", "fnm", "node-versions", "v24.14.1",
		"installation", "lib", "node_modules", "@openai", "codex")
	mkdirAll(t, filepath.Join(pkgDir, "bin"))
	wrapper := filepath.Join(pkgDir, "bin", "codex.js")
	writeInstallFile(t, wrapper, "#!/usr/bin/env node\n")
	writeInstallFile(t, filepath.Join(pkgDir, "package.json"), cliPackageJSON)

	// The vendored binary the wrapper actually spawns, for completeness: it
	// must classify identically from its own platform sub-package metadata.
	vendorPkg := filepath.Join(pkgDir, "node_modules", "@openai", "codex-linux-x64")
	mkdirAll(t, filepath.Join(vendorPkg, "vendor", "x86_64-unknown-linux-musl", "bin"))
	writeInstallFile(t, filepath.Join(vendorPkg, "package.json"), platformPackageJSON)
	vendorBin := filepath.Join(vendorPkg, "vendor", "x86_64-unknown-linux-musl", "bin", "codex")
	writeInstallFile(t, vendorBin, "\x7fELFnot really\n")

	pathDir := filepath.Join(root, ".local", "bin")
	mkdirAll(t, pathDir)
	onPath := filepath.Join(pathDir, "codex")
	symlinkOrSkip(t, wrapper, onPath)

	env := osInstallEnv(filepath.Join(root, ".codex"))
	env.lookPath = func(string) (string, error) { return onPath, nil }
	env.runVersion = func(context.Context, string) (string, error) { return "0.148.0", nil }

	info, err := detectInstall(context.Background(), "codex", env)
	if err != nil {
		t.Fatalf("detectInstall: %v", err)
	}
	if info.Path != onPath {
		t.Errorf("Path = %q, want %q", info.Path, onPath)
	}
	// EvalSymlinks also resolves the temp dir itself (macOS /var -> /private/var),
	// so compare on the tail that identifies the package.
	if !strings.HasSuffix(normalizeInstallPath(info.RealPath), "/node_modules/@openai/codex/bin/codex.js") {
		t.Errorf("RealPath = %q, want it resolved into the npm package", info.RealPath)
	}
	if info.RealPath == info.Path {
		t.Error("RealPath must differ from Path — the shim was not resolved")
	}
	if info.Method != InstallNPMGlobal {
		t.Errorf("Method = %q, want %q", info.Method, InstallNPMGlobal)
	}
	if info.Source != InstallSourcePackageMetadata {
		t.Errorf("Source = %q, want %q", info.Source, InstallSourcePackageMetadata)
	}
	if info.VersionManager != "fnm" {
		t.Errorf("VersionManager = %q, want %q", info.VersionManager, "fnm")
	}
	if info.PackageManager != "npm" {
		t.Errorf("PackageManager = %q, want %q", info.PackageManager, "npm")
	}
	if info.UpdateCmd != "npm install -g @openai/codex@latest" {
		t.Errorf("UpdateCmd = %q", info.UpdateCmd)
	}

	// Same install, entered from the binary codex reports as its "current
	// executable": the classification must not change.
	vendorEnv := env
	vendorEnv.lookPath = func(string) (string, error) { return vendorBin, nil }
	vendorInfo, err := detectInstall(context.Background(), "codex", vendorEnv)
	if err != nil {
		t.Fatalf("detectInstall (vendored): %v", err)
	}
	if vendorInfo.Method != InstallNPMGlobal || vendorInfo.Source != InstallSourcePackageMetadata {
		t.Errorf("vendored binary classified as %q/%q, want %q/%q",
			vendorInfo.Method, vendorInfo.Source, InstallNPMGlobal, InstallSourcePackageMetadata)
	}
	if vendorInfo.UpdateCmd != info.UpdateCmd {
		t.Errorf("vendored UpdateCmd = %q, want %q — same install, same command",
			vendorInfo.UpdateCmd, info.UpdateCmd)
	}
}

// TestDetectInstall_SymlinkChainToStandalone covers the other real chain, the
// one codex's install.sh builds: ~/.local/bin/codex -> CODEX_HOME's
// packages/standalone/current -> the versioned release directory.
func TestDetectInstall_SymlinkChainToStandalone(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")

	release := filepath.Join(codexHome, "packages", "standalone", "releases", "0.148.0")
	mkdirAll(t, filepath.Join(release, "bin"))
	binary := filepath.Join(release, "bin", "codex")
	writeInstallFile(t, binary, "\x7fELFnot really\n")

	current := filepath.Join(codexHome, "packages", "standalone", "current")
	symlinkOrSkip(t, release, current)

	pathDir := filepath.Join(root, ".local", "bin")
	mkdirAll(t, pathDir)
	onPath := filepath.Join(pathDir, "codex")
	symlinkOrSkip(t, filepath.Join(current, "bin", "codex"), onPath)

	env := osInstallEnv(codexHome)
	env.lookPath = func(string) (string, error) { return onPath, nil }
	env.runVersion = func(context.Context, string) (string, error) { return "0.148.0", nil }

	info, err := detectInstall(context.Background(), "codex", env)
	if err != nil {
		t.Fatalf("detectInstall: %v", err)
	}
	if info.Method != InstallNative {
		t.Errorf("Method = %q, want %q", info.Method, InstallNative)
	}
	if info.UpdateCmd != "codex update" {
		t.Errorf("UpdateCmd = %q, want %q", info.UpdateCmd, "codex update")
	}
	if info.ConfigMethod != "standalone" {
		t.Errorf("ConfigMethod = %q, want %q", info.ConfigMethod, "standalone")
	}
	if info.ConfigMismatch {
		t.Error("ConfigMismatch set, but PATH reaches exactly the recorded standalone install")
	}
	if info.VersionManager != "" {
		t.Errorf("VersionManager = %q, want empty", info.VersionManager)
	}
}

// TestDetectInstall_MultipleCopiesOnPath is the state the whole feature exists
// to catch: a standalone install under CODEX_HOME plus an npm copy that PATH
// reaches first. The reported command must update the copy that actually runs,
// and the caller must be told the other copy exists.
func TestDetectInstall_MultipleCopiesOnPath(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")

	release := filepath.Join(codexHome, "packages", "standalone", "releases", "0.140.0")
	mkdirAll(t, filepath.Join(release, "bin"))
	writeInstallFile(t, filepath.Join(release, "bin", "codex"), "\x7fELFold\n")
	symlinkOrSkip(t, release, filepath.Join(codexHome, "packages", "standalone", "current"))

	pkgDir := filepath.Join(root, "npm", "lib", "node_modules", "@openai", "codex")
	mkdirAll(t, filepath.Join(pkgDir, "bin"))
	wrapper := filepath.Join(pkgDir, "bin", "codex.js")
	writeInstallFile(t, wrapper, "#!/usr/bin/env node\n")
	writeInstallFile(t, filepath.Join(pkgDir, "package.json"), cliPackageJSON)

	pathDir := filepath.Join(root, "bin")
	mkdirAll(t, pathDir)
	onPath := filepath.Join(pathDir, "codex")
	symlinkOrSkip(t, wrapper, onPath)

	env := osInstallEnv(codexHome)
	env.lookPath = func(string) (string, error) { return onPath, nil }
	env.runVersion = func(context.Context, string) (string, error) { return "0.148.0", nil }

	info, err := detectInstall(context.Background(), "codex", env)
	if err != nil {
		t.Fatalf("detectInstall: %v", err)
	}
	if info.Method != InstallNPMGlobal {
		t.Errorf("Method = %q, want %q — PATH reaches the npm copy", info.Method, InstallNPMGlobal)
	}
	if info.UpdateCmd != "npm install -g @openai/codex@latest" {
		t.Errorf("UpdateCmd = %q, want the npm command", info.UpdateCmd)
	}
	if !info.ConfigMismatch {
		t.Error("ConfigMismatch not set, but a standalone install exists that PATH does not reach")
	}
	if info.ConfigMethod != "standalone" {
		t.Errorf("ConfigMethod = %q, want %q", info.ConfigMethod, "standalone")
	}
}

func TestParseVersionOutput(t *testing.T) {
	tests := []struct{ in, want string }{
		{"codex-cli 0.148.0\n", "0.148.0"}, // what codex 0.148.0 prints
		{"codex-cli 0.149.0-alpha.1\n", "0.149.0-alpha.1"},
		{"0.148.0\n", "0.148.0"},
		{"v0.148.0\n", "0.148.0"},
		{"  codex-cli   0.148.0  ", "0.148.0"},
		{"", ""},
		{"\n", ""},
		{"error: something went wrong", ""},
	}
	for _, tt := range tests {
		if got := parseVersionOutput(tt.in); got != tt.want {
			t.Errorf("parseVersionOutput(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestClientBinaryPath(t *testing.T) {
	if got := New().binaryPath(); got != "codex" {
		t.Errorf("default binaryPath = %q, want %q", got, "codex")
	}
	if got := New(WithBinaryPath("/opt/codex/bin/codex")).binaryPath(); got != "/opt/codex/bin/codex" {
		t.Errorf("overridden binaryPath = %q", got)
	}
	// A non-local executor cannot be probed from this process.
	if got := NewWithExecutor(nil).binaryPath(); got != "codex" {
		t.Errorf("non-local executor binaryPath = %q, want %q", got, "codex")
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeInstallFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// symlinkOrSkip creates a symlink, skipping the test where the platform does
// not permit it (unprivileged Windows).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
}
