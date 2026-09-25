package codexcli

import (
	"context"

	"github.com/allbin/codexcli-go/schema"
)

// ListMcpServerStatus queries the running app-server for the MCP servers
// visible to a thread via the `mcpServerStatus/list` RPC, following
// pagination until the server reports no more pages.
//
// Pass the thread's ID to see what that thread's runtime sees, including
// servers added through WithThreadConfig. An empty threadID lists the
// servers from the process-level config only.
//
// Codex connects to MCP servers in the background after thread/start.
// Against codex 0.156.1 the first call after NewThread already listed a
// local server's tools, in about 0.6s; a slow server may still report a
// RuntimeStatus of starting or no tools. McpServerStatusEvent reports
// when each server is ready.
func (c *Conn) ListMcpServerStatus(ctx context.Context, threadID string) ([]schema.McpServerStatus, error) {
	var all []schema.McpServerStatus
	params := schema.ListMcpServerStatusParams{}
	if threadID != "" {
		params.ThreadId = &threadID
	}
	for {
		resp, err := c.ListMcpServerStatusPage(ctx, params)
		if err != nil {
			return nil, err
		}
		all = append(all, resp.Data...)
		if resp.NextCursor == nil || *resp.NextCursor == "" {
			return all, nil
		}
		params.Cursor = resp.NextCursor
	}
}

// ListMcpServerStatusPage issues a single `mcpServerStatus/list` request
// and returns the reply verbatim, including NextCursor. Use it to page
// manually or to ask for less detail (schema.ListMcpServerStatusParams.Detail).
func (c *Conn) ListMcpServerStatusPage(ctx context.Context, params schema.ListMcpServerStatusParams) (*schema.ListMcpServerStatusResponse, error) {
	if err := c.checkExited(); err != nil {
		return nil, err
	}
	var resp schema.ListMcpServerStatusResponse
	if err := c.rpc.Request(ctx, schema.MethodMcpServerStatusList, params, &resp); err != nil {
		return nil, c.promoteRPCError(schema.MethodMcpServerStatusList, err)
	}
	return &resp, nil
}
