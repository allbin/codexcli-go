package schema

// Skill RPC method and notification names. Keep in sync with the bundle
// `go generate ./schema` writes to schema/v2_raw/: ClientRequest.json for
// the request methods and ServerNotification.json for skills/changed.
const (
	// MethodSkillsList is the `skills/list` request: discover skills visible
	// from one or more working directories.
	MethodSkillsList = "skills/list"
	// MethodSkillsConfigWrite is the `skills/config/write` request: toggle a
	// skill's enabled state by name or path selector.
	MethodSkillsConfigWrite = "skills/config/write"
	// MethodSkillsChanged is the `skills/changed` notification: watched local
	// skill files changed; treat as an invalidation signal and re-run
	// skills/list when fresh metadata is needed.
	MethodSkillsChanged = "skills/changed"
)

// SkillScope identifies where a skill is defined. Unknown values are
// passed through verbatim for forward compatibility.
type SkillScope string

const (
	SkillScopeUser   SkillScope = "user"
	SkillScopeRepo   SkillScope = "repo"
	SkillScopeSystem SkillScope = "system"
	SkillScopeAdmin  SkillScope = "admin"
)

// SkillsListParams is the `skills/list` request payload.
type SkillsListParams struct {
	// Cwds scopes discovery to specific working directories. When empty,
	// codex defaults to the current session working directory.
	Cwds []string `json:"cwds,omitempty"`
	// ForceReload bypasses the in-memory skills cache and re-scans disk.
	ForceReload bool `json:"forceReload,omitempty"`
}

// SkillsListResponse is the `skills/list` reply: one entry per requested cwd.
type SkillsListResponse struct {
	Data []SkillsListEntry `json:"data"`
}

// SkillsListEntry groups the skills discovered for a single cwd, plus any
// per-skill parse errors encountered while scanning that directory.
type SkillsListEntry struct {
	Cwd    string           `json:"cwd"`
	Skills []SkillMetadata  `json:"skills"`
	Errors []SkillErrorInfo `json:"errors"`
}

// SkillMetadata describes one discovered skill. The field set mirrors the
// codex app-server schema; unknown JSON keys are ignored and missing keys
// decode to zero values, so new codex releases can add fields without
// breaking callers.
type SkillMetadata struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Enabled     bool       `json:"enabled"`
	Path        string     `json:"path"`
	Scope       SkillScope `json:"scope"`
	// ShortDescription is the legacy short_description from SKILL.md. Prefer
	// Interface.ShortDescription when present.
	ShortDescription *string            `json:"shortDescription,omitempty"`
	Interface        *SkillInterface    `json:"interface,omitempty"`
	Dependencies     *SkillDependencies `json:"dependencies,omitempty"`
}

// SkillInterface carries the optional SKILL.json presentation block a UI
// uses to render the skill in a picker.
type SkillInterface struct {
	DisplayName      *string `json:"displayName,omitempty"`
	ShortDescription *string `json:"shortDescription,omitempty"`
	DefaultPrompt    *string `json:"defaultPrompt,omitempty"`
	BrandColor       *string `json:"brandColor,omitempty"`
	IconLarge        *string `json:"iconLarge,omitempty"`
	IconSmall        *string `json:"iconSmall,omitempty"`
}

// SkillDependencies lists the external tools a skill declares it needs.
type SkillDependencies struct {
	Tools []SkillToolDependency `json:"tools"`
}

// SkillToolDependency is one declared tool dependency (e.g. an MCP server
// or a shell command the skill expects to be available).
type SkillToolDependency struct {
	Type        string  `json:"type"`
	Value       string  `json:"value"`
	Command     *string `json:"command,omitempty"`
	URL         *string `json:"url,omitempty"`
	Transport   *string `json:"transport,omitempty"`
	Description *string `json:"description,omitempty"`
}

// SkillErrorInfo reports a skill that failed to parse during discovery.
type SkillErrorInfo struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// SkillsConfigWriteParams is the `skills/config/write` request payload. Set
// exactly one of Name or Path to select the skill, and Enabled to the
// desired state.
type SkillsConfigWriteParams struct {
	Enabled bool    `json:"enabled"`
	Name    *string `json:"name,omitempty"`
	Path    *string `json:"path,omitempty"`
}

// SkillsConfigWriteResponse is the `skills/config/write` reply. It reports
// the effective enabled state after the write, which may differ from the
// requested value if another config layer overrides it.
type SkillsConfigWriteResponse struct {
	EffectiveEnabled bool `json:"effectiveEnabled"`
}

// SkillsChangedNotification is the (empty) `skills/changed` payload.
type SkillsChangedNotification struct{}
