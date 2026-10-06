package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The Windows half of the npm update proof. It lives in a platform-neutral
// file, like shim.go, so every decision stays testable off Windows; only the
// runtime switch in proveNPMUpdate routes a real install here.
//
// Verified on Windows 11 with node 24.20.0, npm 11.6.0 and codex 0.159.0
// (2026-10-06), against the user's real install and a throwaway prefix:
//
//	%APPDATA%\npm\                       the global prefix, and npm's global bin dir
//	  codex, codex.cmd, codex.ps1        shims written by npm's cmd-shim
//	  node_modules\@openai\codex\        the package root
//	    bin\codex.js
//	    node_modules\@openai\codex-win32-x64\vendor\<triple>\bin\codex.exe
//
// There is no lib\ and no bin\: packages sit at `<prefix>\node_modules` and the
// shims in `<prefix>` itself, which is the directory on PATH. npm is not in
// the prefix at all. It ships beside node.exe (`C:\Program Files\nodejs\
// npm.cmd`), and only a user who upgraded npm globally also has an npm.cmd in
// the prefix — which the one beside node.exe then delegates to.

// npmShimDirRefs are the spellings npm's cmd-shim has used for the shim's own
// directory in the line that runs the package: `%dp0%` today, `%~dp0` in
// older releases.
var npmShimDirRefs = []string{`%dp0%`, `%~dp0`}

// windowsShimPackage reports the prefix and package root a global npm `.cmd`
// shim runs. The shim is a text file, not a symlink, so the package it runs is
// read from it: npm writes `"%dp0%\node_modules\@openai\codex\bin\codex.js"`,
// relative to the shim's own directory, which is therefore the prefix. A shim
// that names anything else is not one this proof can speak for.
func windowsShimPackage(shim string, env installEnv) (prefix, pkgRoot string, err error) {
	if !strings.EqualFold(filepath.Ext(shim), ".cmd") {
		return "", "", unproven("%s is not an npm .cmd shim", shim)
	}
	b, err := env.readFile(shim)
	if err != nil {
		return "", "", unproven("read shim %s: %v", shim, err)
	}
	// The shim is batch: its separators are backslashes on whatever OS reads it.
	text := strings.ToLower(string(b))
	ref := `\node_modules\` + strings.ReplaceAll(CLIPackageName, "/", `\`) + `\`
	runsPackage := false
	for _, dir := range npmShimDirRefs {
		if strings.Contains(text, dir+ref) {
			runsPackage = true
			break
		}
	}
	if !runsPackage {
		return "", "", unproven("%s does not run %s relative to its own directory", shim, CLIPackageName)
	}

	prefix = filepath.Dir(shim)
	pkgRoot = windowsPackageRoot(prefix)
	if !isCLIPackageJSON(filepath.Join(pkgRoot, "package.json"), env) {
		return "", "", unproven("%s holds no %s package.json", pkgRoot, CLIPackageName)
	}
	return prefix, pkgRoot, nil
}

// windowsPackageRoot is where npm puts the CLI under a Windows global prefix.
func windowsPackageRoot(prefix string) string {
	return filepath.Join(prefix, "node_modules", filepath.FromSlash(CLIPackageName))
}

// proveNPMUpdateWindows is proveNPMUpdate for a Windows global prefix. The
// proof is the unix one with the Windows layout: the npm about to run must
// report a `prefix -g` that is the very directory holding the shim on PATH,
// and whose `node_modules\@openai\codex` is the very package root that shim
// runs. Both are compared after resolving symlinks and 8.3 names, as exact
// paths and as the same file system object. Anything unresolvable is not a
// proof.
//
// npm cannot be taken from the prefix, because on Windows it does not live
// there, so it is always found the way the unix fallback finds it: once, to an
// absolute path, on the PATH the install will run with (env.childPath; see
// windowsNPM). The unix fallback's npm-cli.js and shebang checks do not
// apply — npm.cmd is batch, not a symlink — and since the proof asks that npm,
// under that environment, where it writes, which npm was found matters only
// to whether the proof holds, never to whether a holding proof is true.
func proveNPMUpdateWindows(ctx context.Context, info *InstallInfo, env installEnv) (*npmUpdatePlan, error) {
	if env.npmPrefix == nil || env.evalSymlink == nil {
		return nil, unproven("no npm prefix probe available")
	}
	prefix, pkgRoot, err := windowsShimPackage(info.RealPath, env)
	if err != nil {
		return nil, err
	}

	npm, err := windowsNPM(prefix, env.childPath, env.childPathExt)
	if err != nil {
		return nil, unproven("find npm: %v", err)
	}
	if err := isNPMCmd(npm, env); err != nil {
		return nil, unproven("%v", err)
	}
	npmDir := filepath.Dir(npm)
	reported, err := env.npmPrefix(ctx, npm, npmDir)
	if err != nil {
		return nil, unproven("%s prefix -g: %v", npm, err)
	}
	if reported == "" {
		return nil, unproven("%s prefix -g reported nothing", npm)
	}
	if err := sameResolvedDir(env, prefix, reported); err != nil {
		return nil, unproven("%s writes its shims into %s, but PATH runs %s: %v", npm, reported, info.Path, err)
	}
	if err := sameResolvedDir(env, pkgRoot, windowsPackageRoot(reported)); err != nil {
		return nil, unproven("%s writes %s, but PATH runs %s: %v", npm, windowsPackageRoot(reported), pkgRoot, err)
	}

	var shims []string
	for _, name := range []string{"codex", "codex.cmd", "codex.ps1"} {
		shims = append(shims, filepath.Join(prefix, name))
	}
	return &npmUpdatePlan{
		prefix:  prefix,
		npm:     npm,
		pathDir: npmDir,
		// As for the unix PATH fallback: a prefix npm derives from its node
		// (nvm-windows sets it to the node directory, and switches node by
		// repointing a symlink) would follow a switch made between this proof
		// and the install. The reported prefix resolved to the shim's
		// directory above, so pinning it changes nothing else.
		pinPrefix: reported,
		targets:   []string{filepath.Join(prefix, "node_modules"), prefix},
		inUse:     append([]string{pkgRoot}, shims...),
		// npm renames the old package and shims aside before it downloads and
		// extracts the new ones, and rolls back only on its own errors or a
		// signal it can catch. Windows has no interrupt to give it, and a tree
		// kill in that window was observed to leave no codex.cmd on PATH and a
		// half-extracted package.
		finishOnCancel: true,
	}, nil
}

// sameResolvedDir requires a and b to resolve to one directory: equal paths
// after symlink resolution — which on Windows also expands 8.3 names and
// restores on-disk case — and the same file system object by volume and file
// index. Either test alone has a hole: equal strings say nothing about a
// junction EvalSymlinks did not see through, and a file index of 0 from a
// network share makes unrelated directories "the same".
func sameResolvedDir(env installEnv, a, b string) error {
	ra, err := env.evalSymlink(a)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", a, err)
	}
	rb, err := env.evalSymlink(b)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", b, err)
	}
	if ra != rb {
		return fmt.Errorf("%s is not %s", rb, ra)
	}
	fa, err := os.Stat(ra)
	if err != nil {
		return err
	}
	fb, err := os.Stat(rb)
	if err != nil {
		return err
	}
	if !fa.IsDir() || !os.SameFile(fa, fb) {
		return fmt.Errorf("%s is not the same directory as %s", rb, ra)
	}
	return nil
}

// windowsNPM resolves the npm an update will run, once, to an absolute path:
// the first `npm` with a runnable extension on pathList (the child's PATH, so
// what the user's own `npm` command is under the environment the install gets),
// else the npm.cmd a global npm upgrade leaves in the prefix.
//
// The lookup is done here rather than by exec at spawn time so `prefix -g`,
// `view` and `install -g` all run one file. Relative PATH entries are skipped,
// as exec.LookPath refuses them: a working directory is not a place to find the
// program that rewrites the install.
func windowsNPM(prefix, pathList, pathExt string) (string, error) {
	exts := windowsRunnableExts(pathExt)
	for _, dir := range filepath.SplitList(pathList) {
		dir = strings.Trim(dir, `"`)
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		for _, ext := range exts {
			if name := filepath.Join(dir, "npm"+ext); isRegularFile(name) {
				return name, nil
			}
		}
	}
	if name := filepath.Join(prefix, "npm.cmd"); isRegularFile(name) {
		return name, nil
	}
	return "", errors.New("npm is neither on PATH nor in the prefix")
}

// isNPMCmd requires the npm found to be npm's own batch launcher — the
// Windows counterpart of the unix fallback's npm-cli.js check — and not a
// dispatcher that happens to be called npm. Volta's npm.exe is the case this
// excludes: it answers `prefix -g` from npm's config but intercepts
// `install -g` into its own image directory, so the proof would hold and the
// install would still make a second copy. The launcher must be a .cmd that
// runs `node_modules\npm\bin\npm-cli.js` relative to itself, beside a
// package.json naming npm. Both forms seen on the reference machine pass: the
// one node's installer puts beside node.exe (which may hand over to a
// globally upgraded npm's npm-cli.js in the prefix) and the one cmd-shim
// writes into the prefix for such an upgrade.
func isNPMCmd(npm string, env installEnv) error {
	if !strings.EqualFold(filepath.Ext(npm), ".cmd") {
		return fmt.Errorf("%s is not npm's npm.cmd", npm)
	}
	b, err := env.readFile(npm)
	if err != nil {
		return err
	}
	text := strings.ToLower(string(b))
	if !strings.Contains(text, `%~dp0\node_modules\npm\bin\npm-cli.js`) &&
		!strings.Contains(text, `%dp0%\node_modules\npm\bin\npm-cli.js`) {
		return fmt.Errorf("%s does not run npm's bin\\npm-cli.js beside it", npm)
	}
	if !isPackageNamed(filepath.Join(filepath.Dir(npm), "node_modules", "npm", "package.json"), "npm", env) {
		return fmt.Errorf("%s has no npm package beside it", npm)
	}
	return nil
}

// windowsRunnableExts is PATHEXT in order, kept to the extensions
// CreateProcess can start: an npm.js or npm.vbs on PATH is not an npm this
// package will run.
func windowsRunnableExts(pathExt string) []string {
	if pathExt == "" {
		pathExt = ".COM;.EXE;.BAT;.CMD"
	}
	var exts []string
	for _, ext := range strings.Split(pathExt, ";") {
		switch strings.ToLower(strings.TrimSpace(ext)) {
		case ".com", ".exe", ".bat", ".cmd":
			exts = append(exts, strings.ToLower(strings.TrimSpace(ext)))
		}
	}
	return exts
}

func isRegularFile(name string) bool {
	fi, err := os.Stat(name)
	return err == nil && fi.Mode().IsRegular()
}

// envValue reads key from the subprocess overrides, falling back to this
// process's environment. Windows variable names are case-insensitive, and a
// caller's WithEnv may well spell PATH as "Path".
func envValue(overrides map[string]string, key string) string {
	if k, ok := envKey(overrides, key); ok {
		return overrides[k]
	}
	return os.Getenv(key)
}

// envKey finds key among overrides, case-insensitively on Windows.
func envKey(overrides map[string]string, key string) (string, bool) {
	if _, ok := overrides[key]; ok {
		return key, true
	}
	if runtime.GOOS == "windows" {
		for k := range overrides {
			if strings.EqualFold(k, key) {
				return k, true
			}
		}
	}
	return "", false
}
