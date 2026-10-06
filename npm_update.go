package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// npmPackageSuffix is where npm puts a global package under its prefix on
// unix: `<prefix>/lib/node_modules/@openai/codex`.
const npmPackageSuffix = "/lib/node_modules/" + CLIPackageName

// npmUpdatePlan is the proof that an npm-global install can be updated in
// place: the npm that writes the package tree the PATH entry executes, and the
// directories that write lands in.
type npmUpdatePlan struct {
	// prefix is the node prefix owning the package root, symlinks resolved.
	prefix string

	// npm is the absolute path every npm step runs: `<prefix>/bin/npm` when
	// the prefix has one, else the npm found once on the child's PATH — on
	// Windows always the latter (see windowsNPM), since npm does not live in
	// a Windows prefix. It is never looked up again between the proof and the
	// install.
	npm string

	// pathDir goes first on the child's PATH for every npm step, so npm's
	// `#!/usr/bin/env node` reaches the node the proof checked rather than
	// whatever a service's PATH would find, or nothing. It is `<prefix>/bin`
	// when the prefix has its own npm, else the directory of the node the
	// fallback npm runs under. On Windows it is npm.cmd's own directory, whose
	// node.exe that npm.cmd runs.
	pathDir string

	// pinPrefix, when set, is passed to `npm install -g` as --prefix: the
	// prefix `npm prefix -g` reported, so the install lands where the proof
	// looked even if something it derives from moves in between. Set for the
	// PATH fallback and on Windows; see proveNPMUpdate.
	pinPrefix string

	// targets are the directories `npm install -g` writes: the package tree
	// (`<prefix>/lib/node_modules`, or `<prefix>\node_modules` on Windows) and
	// the directory whose bin links or shims it rewrites. Both are under
	// prefix, never under the directory npm itself sits in.
	targets []string

	// inUse are the files and trees npm replaces that must not be held open by
	// another process when it starts — Windows only, nil on unix. See
	// firstFileInUse.
	inUse []string

	// finishOnCancel lets a started `npm install -g` run to completion when
	// the caller cancels — Windows only. See updaterRun.finishOnCancel.
	finishOnCancel bool
}

// errNPMUnproven is the reason an npm-global install stays manual. It never
// escapes this package: Update turns it into a ManualUpdateError, and
// detection into SelfManaged false.
var errNPMUnproven = errors.New("codexcli: npm update target not proven")

func unproven(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errNPMUnproven, fmt.Sprintf(format, args...))
}

// proveNPMUpdate decides whether `npm install -g @openai/codex@latest` can be
// run for this install without creating a second copy.
//
// The failure it guards against: npm writes wherever its own global prefix
// is, which is set by the node it runs under, by .npmrc, and by npm_config_*
// variables — none of which need agree with the tree the PATH entry resolves
// into. When they disagree the "update" installs a second copy, and which one
// answers `codex --version` from then on depends on PATH order.
//
// So the npm is chosen once, by absolute path, and asked for its global prefix
// under the same environment the install will run with. Only an exact match,
// after resolving symlinks, is a proof. Anything unresolvable is not.
//
// The npm is the one inside the prefix that owns the package root
// (`<prefix>/bin/npm` beside `<prefix>/bin/node`) whenever anything sits at
// that path. Only when nothing does — a user-level prefix set in .npmrc for a
// system node, the usual way to avoid sudo — is npm looked up on the child's
// PATH; see fallbackNPM for what that npm has to prove about itself.
//
// Only npm is attempted. pnpm and bun keep their own global stores; they stay
// manual until the same proof is written and checked for them. A Windows
// prefix has neither the lib/ layout nor npm inside it, and is proven by
// proveNPMUpdateWindows to the same standard.
func proveNPMUpdate(ctx context.Context, info *InstallInfo, env installEnv) (*npmUpdatePlan, error) {
	if info.Method != InstallNPMGlobal || info.PackageManager != "npm" {
		return nil, unproven("%s install is not npm-global", info.Method)
	}
	if env.os() == "windows" {
		return proveNPMUpdateWindows(ctx, info, env)
	}
	if env.npmPrefix == nil || env.evalSymlink == nil {
		return nil, unproven("no npm prefix probe available")
	}

	pkgRoot := npmPackageRoot(normalizeInstallPath(info.RealPath), env)
	if pkgRoot == "" {
		return nil, unproven("%s is not under <prefix>%s", info.RealPath, npmPackageSuffix)
	}
	prefix := strings.TrimSuffix(pkgRoot, npmPackageSuffix)
	binDir := filepath.Join(filepath.FromSlash(prefix), "bin")
	npm, pathDir := filepath.Join(binDir, "npm"), binDir
	fallback := false
	if _, err := os.Lstat(npm); errors.Is(err, os.ErrNotExist) {
		own := npm
		fallback = true
		if npm, pathDir, err = fallbackNPM(env); err != nil {
			return nil, unproven("no %s, and %v", own, err)
		}
	} else {
		for _, exe := range []string{npm, filepath.Join(binDir, "node")} {
			if err := isExecutableFile(exe); err != nil {
				return nil, unproven("%v", err)
			}
		}
	}

	reported, err := env.npmPrefix(ctx, npm, pathDir)
	if err != nil {
		return nil, unproven("%s prefix -g: %v", npm, err)
	}
	if reported == "" {
		return nil, unproven("%s prefix -g reported nothing", npm)
	}
	wantRoot, err := env.evalSymlink(filepath.FromSlash(pkgRoot))
	if err != nil {
		return nil, unproven("resolve package root %s: %v", pkgRoot, err)
	}
	gotRoot, err := env.evalSymlink(filepath.Join(reported, filepath.FromSlash(npmPackageSuffix)))
	if err != nil {
		return nil, unproven("%s prefix -g is %s, which holds no %s: %v", npm, reported, CLIPackageName, err)
	}
	if gotRoot != wantRoot {
		return nil, unproven("%s writes %s, but PATH runs %s", npm, gotRoot, wantRoot)
	}

	plan := &npmUpdatePlan{
		prefix:  prefix,
		npm:     npm,
		pathDir: pathDir,
		targets: []string{
			filepath.Join(filepath.FromSlash(prefix), "lib", "node_modules"),
			binDir,
		},
	}
	if fallback {
		// The PATH npm's global prefix may be derived rather than configured:
		// from the node it runs under when no .npmrc or npm_config_prefix
		// sets one. If that node moves between this proof and the install (a
		// version switch repointing the PATH directory it was found in), an
		// unpinned install follows it to another prefix. Passing the reported
		// prefix back as --prefix pins it, and changes nothing else: npm
		// derives globalconfig (`<prefix>/etc/npmrc`) from the prefix, so the
		// install reads the same global npmrc the proof's run did.
		// `<prefix>/bin/npm` needs no pin, because its node is fixed by path.
		plan.pinPrefix = reported
	}
	return plan, nil
}

// fallbackNPM finds the npm to prove when the owning prefix has none: the
// first `npm` on the PATH the child will run with ([WithEnv]'s PATH if set,
// else this process's), resolved once to an absolute path. The caller runs
// that path for every step, so no second lookup can reach a different npm.
//
// Found is not enough. The npm has to be npm itself — resolving, symlinks
// followed, to `bin/npm-cli.js` inside a package named npm — and not a
// dispatcher that happens to be called npm. Volta's npm shim is the case this
// excludes: it passes `prefix -g` through to npm, which answers from .npmrc,
// but intercepts `install -g` and writes into Volta's own image directory, so
// the proof would hold and the install would still make a second copy. asdf
// and mise shims are refused by the same rule; they pick a node per directory
// and stay manual.
//
// The node is the one npm-cli.js's shebang names: for `#!/usr/bin/env node`,
// the first `node` on that same PATH; for an absolute interpreter, that file.
// Its directory is returned to go first on the child's PATH. For the env
// shebang that is the node already found first, but a lookup here skips
// relative and empty PATH entries, as exec.LookPath does, while `env` would
// not — putting the checked node's directory first is what makes the shebang
// reach the node that was checked. Whether npm actually runs under it is
// settled by the `prefix -g` run that follows, which must succeed.
func fallbackNPM(env installEnv) (npm, nodeDir string, err error) {
	if env.readFile == nil {
		return "", "", errors.New("no npm lookup available")
	}
	npm, err = lookPathIn("npm", env.childPath)
	if err != nil {
		return "", "", fmt.Errorf("no npm on PATH: %w", err)
	}
	cli, err := env.evalSymlink(npm)
	if err != nil {
		return npm, "", fmt.Errorf("resolve %s: %w", npm, err)
	}
	pkgDir := filepath.Dir(filepath.Dir(cli))
	if filepath.Base(cli) != "npm-cli.js" || filepath.Base(filepath.Dir(cli)) != "bin" ||
		!isPackageNamed(filepath.Join(pkgDir, "package.json"), "npm", env) {
		return npm, "", fmt.Errorf("%s resolves to %s, which is not npm's own bin/npm-cli.js", npm, cli)
	}
	b, err := env.readFile(cli)
	if err != nil {
		return npm, "", err
	}
	node, err := shebangNode(b, env.childPath)
	if err != nil {
		return npm, "", fmt.Errorf("%s: %w", cli, err)
	}
	if err := isExecutableFile(node); err != nil {
		return npm, "", err
	}
	return npm, filepath.Dir(node), nil
}

// shebangNode returns the node a script's `#!` line runs it under:
// `#!/usr/bin/env node` looks node up on pathList, `#!/abs/node` names it.
// Any other interpreter line is not one this proof understands.
func shebangNode(script []byte, pathList string) (string, error) {
	line, _, _ := strings.Cut(string(script), "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "#!")
	if !ok {
		return "", errors.New("no #! line")
	}
	f := strings.Fields(rest)
	switch {
	case len(f) == 2 && filepath.Base(f[0]) == "env" && f[1] == "node":
		node, err := lookPathIn("node", pathList)
		if err != nil {
			return "", fmt.Errorf("no node on PATH for %q: %w", line, err)
		}
		return node, nil
	case len(f) == 1 && filepath.IsAbs(f[0]) && strings.HasPrefix(filepath.Base(f[0]), "node"):
		return f[0], nil
	}
	return "", fmt.Errorf("interpreter %q is not node", line)
}

// lookPathIn is exec.LookPath over an explicit PATH value rather than this
// process's. Relative and empty entries are skipped, the way exec.LookPath
// refuses them with ErrDot: they name whatever directory the child happens
// to start in.
func lookPathIn(name, pathList string) (string, error) {
	for _, dir := range filepath.SplitList(pathList) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if isExecutableFile(p) == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
}

// npmPackageRoot walks up from the resolved binary to the
// `<prefix>/lib/node_modules/@openai/codex` directory whose package.json names
// the CLI. A nested match (the vendored platform package is an npm alias of
// the same name) does not end in the suffix and is walked past.
func npmPackageRoot(p string, env installEnv) string {
	dir := path.Dir(p)
	for i := 0; i < maxPackageWalkUp; i++ {
		if strings.HasSuffix(dir, npmPackageSuffix) && dir != npmPackageSuffix &&
			isCLIPackageJSON(path.Join(dir, "package.json"), env) {
			return dir
		}
		parent := path.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func isExecutableFile(name string) error {
	fi, err := os.Stat(name)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", name)
	}
	return nil
}

// runNPMPrefix runs `<npm> prefix -g` with pathDir first on PATH and returns
// the trimmed output. npm reads no network for this; it resolves config only.
func runNPMPrefix(ctx context.Context, npm, pathDir string, overrides map[string]string, workDir string) (string, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultInstallTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, npm, "prefix", "-g")
	cmd.Env = buildEnv(withPathPrefix(overrides, pathDir))
	if workDir != "" {
		cmd.Dir = workDir
	}
	hideConsoleWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runNPMLatest runs `<npm> view @openai/codex@latest version` with pathDir
// first on PATH, so the registry and auth npm is configured with answer.
func runNPMLatest(ctx context.Context, npm, pathDir string, overrides map[string]string, workDir string) (string, error) {
	cmd := exec.CommandContext(ctx, npm, "view", CLIPackageName+"@latest", "version")
	cmd.Env = buildEnv(withPathPrefix(overrides, pathDir))
	if workDir != "" {
		cmd.Dir = workDir
	}
	hideConsoleWindow(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if tail := lastOutputLine(stderr.String()); tail != "" {
			return "", fmt.Errorf("%w: %s", err, tail)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// withPathPrefix returns overrides with dir placed first on PATH, without
// mutating the caller's map. The base PATH is the override if one is set,
// else this process's own. On Windows an override spelled "Path" is the one
// replaced, so the child does not get two PATHs.
func withPathPrefix(overrides map[string]string, dir string) map[string]string {
	merged := make(map[string]string, len(overrides)+1)
	for k, v := range overrides {
		merged[k] = v
	}
	key, ok := envKey(overrides, "PATH")
	if !ok {
		key = "PATH"
	}
	base := envValue(overrides, "PATH")
	if base == "" {
		merged[key] = dir
	} else {
		merged[key] = dir + string(os.PathListSeparator) + base
	}
	return merged
}

// installSelfManaged reports whether Update would act for this install: the
// standalone layout codex's own updater owns, or an npm-global install whose
// update target is proven and writable. It is the single verdict behind both
// InstallInfo.SelfManaged and Update, so the two cannot disagree.
//
// The standalone case is answered on method alone, as it always was; its
// writability is left to Update's preflight, which reports it as
// [ErrUpdateNotWritable].
func installSelfManaged(ctx context.Context, info *InstallInfo, env installEnv) bool {
	switch info.Method {
	case InstallNative:
		return true
	case InstallNPMGlobal:
		plan, err := proveNPMUpdate(ctx, info, env)
		if err != nil {
			return false
		}
		for _, dir := range plan.targets {
			if probeWritable(env, dir) != nil {
				return false
			}
		}
		return true
	}
	return false
}
