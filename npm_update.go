package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
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

	// npm is `<prefix>/bin/npm`, the npm shipped beside the node that owns the
	// package. Never a PATH lookup.
	npm string

	// binDir is `<prefix>/bin`. It goes first on the child's PATH, because
	// npm is a `#!/usr/bin/env node` script and a service's PATH may reach a
	// different node, or none.
	binDir string

	// targets are the directories `npm install -g` writes: the package tree
	// under lib/node_modules and the bin link it rewrites.
	targets []string
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
// So nothing here is looked up on PATH. The npm is the one inside the prefix
// that owns the package root (`<prefix>/bin/npm` beside `<prefix>/bin/node`),
// and that npm is asked for its global prefix under the same environment the
// install will run with. Only an exact match, after resolving symlinks, is a
// proof. Anything unresolvable is not.
//
// Only npm on unix is attempted. pnpm and bun keep their own global stores,
// and a Windows prefix has neither the lib/ layout nor a verified shim path;
// they stay manual until the same proof is written and checked for them.
func proveNPMUpdate(ctx context.Context, info *InstallInfo, env installEnv) (*npmUpdatePlan, error) {
	if info.Method != InstallNPMGlobal || info.PackageManager != "npm" {
		return nil, unproven("%s install is not npm-global", info.Method)
	}
	if runtime.GOOS == "windows" {
		return nil, unproven("npm prefix layout is not verified on windows")
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
	npm := filepath.Join(binDir, "npm")
	for _, exe := range []string{npm, filepath.Join(binDir, "node")} {
		if err := isExecutableFile(exe); err != nil {
			return nil, unproven("%v", err)
		}
	}

	reported, err := env.npmPrefix(ctx, npm, binDir)
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

	return &npmUpdatePlan{
		prefix: prefix,
		npm:    npm,
		binDir: binDir,
		targets: []string{
			filepath.Join(filepath.FromSlash(prefix), "lib", "node_modules"),
			binDir,
		},
	}, nil
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

// runNPMPrefix runs `<npm> prefix -g` with binDir first on PATH and returns
// the trimmed output. npm reads no network for this; it resolves config only.
func runNPMPrefix(ctx context.Context, npm, binDir string, overrides map[string]string, workDir string) (string, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultInstallTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, npm, "prefix", "-g")
	cmd.Env = buildEnv(withPathPrefix(overrides, binDir))
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

// runNPMLatest runs `<npm> view @openai/codex@latest version` with binDir
// first on PATH, so the registry and auth npm is configured with answer.
func runNPMLatest(ctx context.Context, npm, binDir string, overrides map[string]string, workDir string) (string, error) {
	cmd := exec.CommandContext(ctx, npm, "view", CLIPackageName+"@latest", "version")
	cmd.Env = buildEnv(withPathPrefix(overrides, binDir))
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
// else this process's own.
func withPathPrefix(overrides map[string]string, dir string) map[string]string {
	merged := make(map[string]string, len(overrides)+1)
	for k, v := range overrides {
		merged[k] = v
	}
	base, ok := overrides["PATH"]
	if !ok {
		base = os.Getenv("PATH")
	}
	if base == "" {
		merged["PATH"] = dir
	} else {
		merged["PATH"] = dir + string(os.PathListSeparator) + base
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
