package schema

import (
	"encoding/json"
	"sort"
)

// MethodMcpServerStatusList is the `mcpServerStatus/list` request: the MCP
// servers codex has configured, with their tools, resources and
// connection state. Keep in sync with ListMcpServerStatusParams.json and
// ListMcpServerStatusResponse.json in the generated bundle.
const MethodMcpServerStatusList = "mcpServerStatus/list"

// McpServerStatusDetail controls how much inventory mcpServerStatus/list
// fetches per server. Codex defaults to McpServerStatusDetailFull.
type McpServerStatusDetail string

const (
	McpServerStatusDetailFull             McpServerStatusDetail = "full"
	McpServerStatusDetailToolsAndAuthOnly McpServerStatusDetail = "toolsAndAuthOnly"
)

// ListMcpServerStatusParams is the `mcpServerStatus/list` request payload.
type ListMcpServerStatusParams struct {
	// ThreadId scopes the listing to one thread's MCP runtime, which is
	// what reflects a per-thread config overlay. Without it codex lists
	// the servers from the process-level config.
	ThreadId *string `json:"threadId,omitempty"`
	// Cursor is the opaque NextCursor of a previous page.
	Cursor *string `json:"cursor,omitempty"`
	// Limit is the page size; codex picks one when unset.
	Limit  *uint32                `json:"limit,omitempty"`
	Detail *McpServerStatusDetail `json:"detail,omitempty"`
}

// ListMcpServerStatusResponse is the `mcpServerStatus/list` reply.
type ListMcpServerStatusResponse struct {
	Data []McpServerStatus `json:"data"`
	// NextCursor is set when more pages follow.
	NextCursor *string `json:"nextCursor,omitempty"`
}

// McpAuthStatus is how codex authenticates to an MCP server. Unknown
// values are passed through verbatim.
type McpAuthStatus string

const (
	McpAuthStatusUnknown     McpAuthStatus = "unknown"
	McpAuthStatusUnsupported McpAuthStatus = "unsupported"
	McpAuthStatusNotLoggedIn McpAuthStatus = "notLoggedIn"
	McpAuthStatusBearerToken McpAuthStatus = "bearerToken"
	McpAuthStatusOAuth       McpAuthStatus = "oAuth"
)

// McpServerConnectionStatus is the thread runtime's connection state for
// an MCP server. Unknown values are passed through verbatim.
type McpServerConnectionStatus string

const (
	McpServerConnectionNotStarted             McpServerConnectionStatus = "notStarted"
	McpServerConnectionStarting               McpServerConnectionStatus = "starting"
	McpServerConnectionConnected              McpServerConnectionStatus = "connected"
	McpServerConnectionAuthenticationRequired McpServerConnectionStatus = "authenticationRequired"
	McpServerConnectionFailed                 McpServerConnectionStatus = "failed"
	McpServerConnectionCancelled              McpServerConnectionStatus = "cancelled"
	McpServerConnectionDisabled               McpServerConnectionStatus = "disabled"
)

// McpServerStatus is one entry of the `mcpServerStatus/list` reply.
type McpServerStatus struct {
	// Name is the server's key under mcp_servers.
	Name string `json:"name"`
	// Tools maps each tool codex discovered on the server by name.
	Tools      map[string]McpTool `json:"tools"`
	AuthStatus McpAuthStatus      `json:"authStatus"`
	// RuntimeStatus is the thread runtime's connection state; nil when
	// codex has none, or the configuration changed since it connected.
	RuntimeStatus *McpServerConnectionStatus `json:"runtimeStatus,omitempty"`
	// ToolsError is set when tool discovery failed and no catalog was
	// returned.
	ToolsError *string        `json:"toolsError,omitempty"`
	ServerInfo *McpServerInfo `json:"serverInfo,omitempty"`
	PluginId   *string        `json:"pluginId,omitempty"`

	// Resources, ResourceTemplates and ServerCapabilities follow the MCP
	// specification's shapes and stay raw.
	Resources          json.RawMessage `json:"resources,omitempty"`
	ResourceTemplates  json.RawMessage `json:"resourceTemplates,omitempty"`
	ServerCapabilities json.RawMessage `json:"serverCapabilities,omitempty"`

	// Raw preserves the full entry for fields not yet typed here.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps both the typed projection and the raw bytes.
func (s *McpServerStatus) UnmarshalJSON(data []byte) error {
	type alias McpServerStatus
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = McpServerStatus(a)
	s.Raw = append(s.Raw[:0], data...)
	return nil
}

// ToolNames returns the names of the server's tools, sorted.
func (s McpServerStatus) ToolNames() []string {
	names := make([]string, 0, len(s.Tools))
	for name := range s.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// McpTool is an MCP tool definition as the server advertised it.
type McpTool struct {
	Name        string  `json:"name"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`

	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Meta         json.RawMessage `json:"_meta,omitempty"`
}

// McpServerInfo is the presentation metadata an initialized MCP server
// advertises.
type McpServerInfo struct {
	Name        string  `json:"name"`
	Version     string  `json:"version"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	WebsiteUrl  *string `json:"websiteUrl,omitempty"`
}
