package codexcli

import (
	"context"

	"github.com/allbin/codexcli-go/schema"
)

// ListSkills queries the running app-server for the skills visible from the
// given working directories via the `skills/list` RPC.
//
// Unlike claudecli-go — where Claude Code pushes a flat list of skill names
// on the init event — codex does not advertise skills at thread start. This
// is the only way to discover them, so it is a deliberate pull: call it when
// you actually need the list (e.g. to populate a picker), not on every
// connect.
//
// Pass nil cwds to use the session's current working directory. Set
// forceReload to bypass codex's in-memory cache and re-scan disk; prefer the
// cached path and rely on SkillsChangedEvent to know when a reload is worth
// it.
//
// The reply groups skills per cwd (schema.SkillsListEntry) and includes any
// per-skill parse errors codex hit while scanning. To invoke a discovered
// skill in a turn, build the input with SkillInput(meta) or
// schema.SkillInput(name, path).
func (c *Conn) ListSkills(ctx context.Context, cwds []string, forceReload bool) ([]schema.SkillsListEntry, error) {
	if err := c.checkExited(); err != nil {
		return nil, err
	}
	params := schema.SkillsListParams{Cwds: cwds, ForceReload: forceReload}
	var resp schema.SkillsListResponse
	if err := c.rpc.Request(ctx, schema.MethodSkillsList, params, &resp); err != nil {
		return nil, c.promoteRPCError(schema.MethodSkillsList, err)
	}
	return resp.Data, nil
}

// SetSkillEnabledByName toggles a skill's enabled state by name via the
// `skills/config/write` RPC and returns the effective enabled state after
// the write (which can differ from the requested value if another config
// layer overrides it).
func (c *Conn) SetSkillEnabledByName(ctx context.Context, name string, enabled bool) (bool, error) {
	return c.writeSkillsConfig(ctx, schema.SkillsConfigWriteParams{Enabled: enabled, Name: &name})
}

// SetSkillEnabledByPath toggles a skill's enabled state by absolute path via
// the `skills/config/write` RPC. Use this to disambiguate when the same
// skill name is visible from multiple scopes or cwds. Returns the effective
// enabled state after the write.
func (c *Conn) SetSkillEnabledByPath(ctx context.Context, path string, enabled bool) (bool, error) {
	return c.writeSkillsConfig(ctx, schema.SkillsConfigWriteParams{Enabled: enabled, Path: &path})
}

func (c *Conn) writeSkillsConfig(ctx context.Context, params schema.SkillsConfigWriteParams) (bool, error) {
	if err := c.checkExited(); err != nil {
		return false, err
	}
	var resp schema.SkillsConfigWriteResponse
	if err := c.rpc.Request(ctx, schema.MethodSkillsConfigWrite, params, &resp); err != nil {
		return false, c.promoteRPCError(schema.MethodSkillsConfigWrite, err)
	}
	return resp.EffectiveEnabled, nil
}

// SkillInput is the discovery-aware convenience for building a "skill" turn
// input from a SkillMetadata returned by Conn.ListSkills. It pulls the
// name/path pair straight from the metadata, avoiding the easy mistake of
// pairing a skill name with the wrong path when the same name is visible
// from multiple working directories.
//
// For the low-level primitive, use schema.SkillInput(name, path).
func SkillInput(meta schema.SkillMetadata) schema.UserInput {
	return schema.SkillInput(meta.Name, meta.Path)
}
