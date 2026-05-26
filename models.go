package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrModelsCacheUnavailable indicates the codex models cache file does
// not exist on disk. Callers should fall back to their own defaults.
var ErrModelsCacheUnavailable = errors.New("codexcli: models cache file not found")

// ModelVisibility represents the codex CLI's hint for how a model should
// surface in a UI picker. Compare against the typed constants below;
// unknown values are passed through verbatim for forward compatibility.
type ModelVisibility string

const (
	VisibilityList ModelVisibility = "list"
	VisibilityHide ModelVisibility = "hide"
)

// ModelInfo describes one model entry from codex's on-disk models cache.
// The field set mirrors the codex CLI's schema as observed in codex 0.133.0.
// New fields can be added in future codex releases without breaking callers
// because every field is optional from the consumer's point of view —
// unknown JSON keys are ignored and missing keys decode to zero values.
//
// Callers driving a model picker should filter on Visibility (typically
// keeping only VisibilityList entries) and sort by Priority.
type ModelInfo struct {
	Slug                          string            `json:"slug,omitempty"`
	DisplayName                   string            `json:"display_name,omitempty"`
	Description                   string            `json:"description,omitempty"`
	DefaultReasoningLevel         string            `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels      []ReasoningLevel  `json:"supported_reasoning_levels,omitempty"`
	ShellType                     string            `json:"shell_type,omitempty"`
	Visibility                    ModelVisibility   `json:"visibility,omitempty"`
	SupportedInAPI                bool              `json:"supported_in_api,omitempty"`
	Priority                      int               `json:"priority,omitempty"`
	AdditionalSpeedTiers          []string          `json:"additional_speed_tiers,omitempty"`
	ServiceTiers                  []ServiceTier     `json:"service_tiers,omitempty"`
	AvailabilityNUX               *AvailabilityNUX  `json:"availability_nux,omitempty"`
	Upgrade                       *ModelUpgrade     `json:"upgrade,omitempty"`
	BaseInstructions              string            `json:"base_instructions,omitempty"`
	ModelMessages                 *ModelMessages    `json:"model_messages,omitempty"`
	SupportsReasoningSummaries    bool              `json:"supports_reasoning_summaries,omitempty"`
	DefaultReasoningSummary       string            `json:"default_reasoning_summary,omitempty"`
	SupportVerbosity              bool              `json:"support_verbosity,omitempty"`
	DefaultVerbosity              string            `json:"default_verbosity,omitempty"`
	ApplyPatchToolType            string            `json:"apply_patch_tool_type,omitempty"`
	WebSearchToolType             string            `json:"web_search_tool_type,omitempty"`
	TruncationPolicy              *TruncationPolicy `json:"truncation_policy,omitempty"`
	SupportsParallelToolCalls     bool              `json:"supports_parallel_tool_calls,omitempty"`
	SupportsImageDetailOriginal   bool              `json:"supports_image_detail_original,omitempty"`
	ContextWindow                 int               `json:"context_window,omitempty"`
	MaxContextWindow              int               `json:"max_context_window,omitempty"`
	EffectiveContextWindowPercent int               `json:"effective_context_window_percent,omitempty"`
	ExperimentalSupportedTools    []json.RawMessage `json:"experimental_supported_tools,omitempty"`
	InputModalities               []string          `json:"input_modalities,omitempty"`
	SupportsSearchTool            bool              `json:"supports_search_tool,omitempty"`
}

// ReasoningLevel is one entry of a model's supported reasoning effort list.
type ReasoningLevel struct {
	Effort      string `json:"effort,omitempty"`
	Description string `json:"description,omitempty"`
}

// ServiceTier describes one billing/throughput tier offered for a model.
type ServiceTier struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// AvailabilityNUX carries the new-user-experience message codex displays
// the first time a model becomes available.
type AvailabilityNUX struct {
	Message string `json:"message,omitempty"`
}

// ModelUpgrade points at a successor model when codex flags the current
// slug as superseded.
type ModelUpgrade struct {
	Model             string `json:"model,omitempty"`
	MigrationMarkdown string `json:"migration_markdown,omitempty"`
}

// ModelMessages bundles the system-prompt template and substitution
// variables codex CLI uses when starting a turn against this model.
type ModelMessages struct {
	InstructionsTemplate  string            `json:"instructions_template,omitempty"`
	InstructionsVariables map[string]string `json:"instructions_variables,omitempty"`
}

// TruncationPolicy controls how codex truncates long histories for the model.
type TruncationPolicy struct {
	Mode  string `json:"mode,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// modelsCacheFile mirrors the on-disk wrapper around the models array.
type modelsCacheFile struct {
	FetchedAt     string      `json:"fetched_at,omitempty"`
	ETag          string      `json:"etag,omitempty"`
	ClientVersion string      `json:"client_version,omitempty"`
	Models        []ModelInfo `json:"models"`
}

// ListModels returns the codex model registry as cached on disk. It does
// not open an app-server connection or spawn a process — it is a pure
// file read. The codex CLI itself refreshes this file on launch.
//
// Resolution order for the cache directory:
//  1. WithCodexHome on the Client (or per-call options)
//  2. $CODEX_HOME
//  3. $HOME/.codex (or platform equivalent via os.UserHomeDir)
//
// Returns ErrModelsCacheUnavailable if the file does not exist, so callers
// can fall back to their own defaults. Returns a parse error for malformed
// JSON.
//
// The ctx parameter is reserved for future remote/refresh modes; the
// current implementation does not consult it.
func (c *Client) ListModels(ctx context.Context, opts ...Option) ([]ModelInfo, error) {
	resolved := resolveOptions(c.defaults, opts)
	return readModelsCache(resolved.codexHome)
}

// ListModels is the package-level convenience that mirrors
// (*Client).ListModels without requiring a Client.
func ListModels(ctx context.Context, opts ...Option) ([]ModelInfo, error) {
	resolved := resolveOptions(nil, opts)
	return readModelsCache(resolved.codexHome)
}

func readModelsCache(codexHomeOverride string) ([]ModelInfo, error) {
	home, err := resolveCodexHome(codexHomeOverride)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, "models_cache.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrModelsCacheUnavailable
		}
		return nil, fmt.Errorf("codexcli: read models cache: %w", err)
	}
	var cache modelsCacheFile
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("codexcli: parse models cache: %w", err)
	}
	if cache.Models == nil {
		return []ModelInfo{}, nil
	}
	return cache.Models, nil
}

func resolveCodexHome(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("CODEX_HOME"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codexcli: locate codex home: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}
