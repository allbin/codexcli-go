package codexcli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"strings"
)

// Process represents a running codex app-server subprocess.
type Process struct {
	Stdout io.ReadCloser
	Stderr io.ReadCloser
	Stdin  io.WriteCloser
	Wait   func() error
}

// StartConfig holds parameters for starting a codex subprocess.
type StartConfig struct {
	Args    []string
	Env     map[string]string
	WorkDir string
}

// Executor controls how the codex CLI is spawned. Implement this
// interface to customize execution (e.g. Docker, SSH, remote socket).
type Executor interface {
	Start(ctx context.Context, cfg *StartConfig) (*Process, error)
}

// LocalExecutor spawns codex as a local subprocess.
type LocalExecutor struct {
	// BinaryPath overrides the CLI binary. Defaults to "codex".
	BinaryPath string
}

// NewLocalExecutor returns an executor that runs codex locally.
func NewLocalExecutor() *LocalExecutor { return &LocalExecutor{} }

// Start launches `codex app-server --listen stdio://` (cfg.Args is
// prepended after `app-server`). The transport defaults to stdio so
// existing callers don't have to know about app-server's flag surface.
func (e *LocalExecutor) Start(ctx context.Context, cfg *StartConfig) (*Process, error) {
	binary := e.BinaryPath
	if binary == "" {
		binary = "codex"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("codex binary not found: %w", err)
	}

	args := append([]string{"app-server"}, cfg.Args...)
	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Env = buildEnv(cfg.Env)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	hideConsoleWindow(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	return &Process{
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  stdin,
		Wait:   cmd.Wait,
	}, nil
}

// buildEnv merges os.Environ with overrides. Codex picks up CODEX_HOME
// from the environment, so callers that need a sandboxed home set
// CODEX_HOME via WithEnv.
func buildEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if _, ok := overrides[key]; ok {
			continue
		}
		env = append(env, e)
	}
	merged := maps.Clone(overrides)
	for k, v := range merged {
		env = append(env, k+"="+v)
	}
	return env
}
