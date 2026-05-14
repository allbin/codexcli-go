package codexcli

import (
	"github.com/allbin/codexcli-go/schema"
)

// Option configures a Run/Connect call or, when passed to New,
// supplies defaults. Options applied at call time replace (not merge
// with) client-level defaults.
type Option func(*options)

type options struct {
	// client-only
	binaryPath string
	clientInfo schema.ClientInfo
	caps       *schema.InitializeCapabilities

	// thread bootstrapping
	cwd           string
	model         string
	modelProvider string
	approval      *schema.AskForApproval
	sandbox       *schema.SandboxMode
	threadExtra   map[string]any
	ephemeral     *bool

	// turn defaults
	effort    string
	turnExtra map[string]any

	// execution
	workDir string
	env     map[string]string

	// observability
	stderrCallback func(string)

	// notification opt-outs forwarded to the server
	optOutMethods []string
	experimental  bool

	// approval routing
	approvalFunc ApprovalFunc
}

// ClientOption configures the Client itself (not individual Run calls).
type ClientOption func(*Client)

// WithBinaryPath sets the codex CLI binary path. Only effective when
// passed to New (ignored at call time). Defaults to "codex".
func WithBinaryPath(path string) Option {
	return func(o *options) { o.binaryPath = path }
}

// WithClientInfo overrides the clientInfo sent during `initialize`.
// Defaults to {name: DefaultClientName, version: SDKVersion}. Enterprise
// integrations should set a stable identifier — codex app-server uses
// the name for OpenAI Compliance Logs routing.
func WithClientInfo(info schema.ClientInfo) Option {
	return func(o *options) { o.clientInfo = info }
}

// WithExperimentalAPI opts into the experimental protocol surface
// (dynamic tools, environments, additional notification shapes).
func WithExperimentalAPI() Option {
	return func(o *options) { o.experimental = true }
}

// WithOptOutNotifications suppresses the named server-to-client
// notification methods for this connection. Exact-match; unknown method
// names are ignored by the server.
func WithOptOutNotifications(methods ...string) Option {
	return func(o *options) { o.optOutMethods = append(o.optOutMethods, methods...) }
}

// WithCwd sets the cwd passed to thread/start. When set and the
// resolved sandbox is workspace-write or full access, codex app-server
// marks the directory as trusted in config.toml.
func WithCwd(cwd string) Option {
	return func(o *options) { o.cwd = cwd }
}

// WithModel overrides the model on thread/start and turn/start.
func WithModel(model string) Option {
	return func(o *options) { o.model = model }
}

// WithModelProvider overrides the model provider on thread/start.
func WithModelProvider(provider string) Option {
	return func(o *options) { o.modelProvider = provider }
}

// WithApprovalPolicy sets the legacy thread-level approval policy. Use
// the raw JSON form for the granular variant.
func WithApprovalPolicy(policy schema.AskForApproval) Option {
	return func(o *options) { o.approval = &policy }
}

// WithSandbox sets the legacy thread-level sandbox shorthand.
func WithSandbox(mode schema.SandboxMode) Option {
	return func(o *options) { o.sandbox = &mode }
}

// WithEphemeralThread marks the thread as in-memory only — no rollout
// file is materialized on disk.
func WithEphemeralThread() Option {
	t := true
	return func(o *options) { o.ephemeral = &t }
}

// WithThreadExtra splices arbitrary keys into the thread/start params.
// Use this as the forward-compat escape hatch for fields not yet typed
// on ThreadStartParams; the keys are forwarded to the server verbatim.
func WithThreadExtra(extra map[string]any) Option {
	return func(o *options) {
		if o.threadExtra == nil {
			o.threadExtra = map[string]any{}
		}
		for k, v := range extra {
			o.threadExtra[k] = v
		}
	}
}

// WithTurnExtra splices arbitrary keys into the turn/start params.
func WithTurnExtra(extra map[string]any) Option {
	return func(o *options) {
		if o.turnExtra == nil {
			o.turnExtra = map[string]any{}
		}
		for k, v := range extra {
			o.turnExtra[k] = v
		}
	}
}

// WithEffort overrides the reasoning effort on turn/start. Values map
// to schema.ReasoningEffort ("none", "minimal", "low", "medium", "high",
// "xhigh").
func WithEffort(effort string) Option {
	return func(o *options) { o.effort = effort }
}

// WithWorkDir sets the codex subprocess working directory. Note this is
// separate from WithCwd which controls the thread cwd field — codex
// resolves rollouts and config relative to the subprocess cwd, while
// the thread cwd controls sandbox roots.
func WithWorkDir(dir string) Option {
	return func(o *options) { o.workDir = dir }
}

// WithEnv merges environment variables into the subprocess env. Use
// CODEX_HOME here to pin a sandboxed home for testing.
func WithEnv(env map[string]string) Option {
	return func(o *options) {
		if o.env == nil {
			o.env = map[string]string{}
		}
		for k, v := range env {
			o.env[k] = v
		}
	}
}

// WithStderrCallback registers a per-line callback for the subprocess
// stderr (codex logs at LOG_FORMAT=json or RUST_LOG-controlled text).
func WithStderrCallback(fn func(string)) Option {
	return func(o *options) { o.stderrCallback = fn }
}

// WithApprovalHandler registers a callback that responds to server-initiated
// approval requests. If unset, every approval auto-declines so codex falls
// back to skipping the proposed action — the safe default for headless use.
//
// The callback receives a typed ApprovalRequest; type-switch to handle the
// specific kind (command execution, file change, permissions, legacy). Return
// any ApprovalDecision (Accept, Decline, Cancel, PermissionGrant, etc.).
//
// Errors returned from the callback are surfaced as a JSON-RPC error
// response back to the server, which the agent typically renders as a
// tool failure. Returning a nil decision is treated as Decline.
func WithApprovalHandler(fn ApprovalFunc) Option {
	return func(o *options) { o.approvalFunc = fn }
}

func resolveOptions(defaults []Option, overrides []Option) *options {
	opts := &options{
		clientInfo: schema.ClientInfo{Name: DefaultClientName, Version: SDKVersion},
	}
	for _, o := range defaults {
		o(opts)
	}
	for _, o := range overrides {
		o(opts)
	}
	if opts.experimental || len(opts.optOutMethods) > 0 {
		opts.caps = &schema.InitializeCapabilities{
			ExperimentalApi:           opts.experimental,
			OptOutNotificationMethods: opts.optOutMethods,
		}
	}
	return opts
}

func (o *options) buildThreadStartParams() schema.ThreadStartParams {
	p := schema.ThreadStartParams{
		ApprovalPolicy: o.approval,
		Sandbox:        o.sandbox,
		Ephemeral:      o.ephemeral,
		Extra:          o.threadExtra,
	}
	if o.cwd != "" {
		v := o.cwd
		p.Cwd = &v
	}
	if o.model != "" {
		v := o.model
		p.Model = &v
	}
	if o.modelProvider != "" {
		v := o.modelProvider
		p.ModelProvider = &v
	}
	return p
}

func (o *options) buildTurnStartParams(threadID, prompt string) schema.TurnStartParams {
	p := schema.TurnStartParams{
		ThreadId: threadID,
		Input:    []schema.UserInput{schema.TextInput(prompt)},
		Extra:    o.turnExtra,
	}
	if o.model != "" {
		v := o.model
		p.Model = &v
	}
	if o.effort != "" {
		v := o.effort
		p.Effort = &v
	}
	if o.cwd != "" {
		v := o.cwd
		p.Cwd = &v
	}
	if o.approval != nil {
		p.ApprovalPolicy = o.approval
	}
	return p
}
