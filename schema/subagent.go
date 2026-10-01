package schema

import "encoding/json"

// SubAgentActivity kinds. Codex 0.159.3 lists started, interacted,
// interrupted and completed.
const (
	SubAgentActivityStarted     = "started"
	SubAgentActivityInteracted  = "interacted"
	SubAgentActivityInterrupted = "interrupted"
	SubAgentActivityCompleted   = "completed"
)

// SubAgentActivity is the payload of a "subAgentActivity" thread item: a
// parent thread's record of a subagent it spawned or touched.
type SubAgentActivity struct {
	// Kind is one of the SubAgentActivity* constants.
	Kind string `json:"kind"`
	// AgentThreadID is the subagent's own thread id.
	AgentThreadID string `json:"agentThreadId"`
	// AgentPath is hierarchical: "/root/alpha" is a child of the root,
	// "/root/alpha/beta" a grandchild.
	AgentPath string `json:"agentPath"`
}

// SubAgentActivity returns the subAgentActivity payload, or nil if the item
// is another type. The item's ID is the spawning tool call's id for kind
// "started" ("call_…"); codex synthesises it for "completed"
// ("subagent-completed-<child turn id>").
func (t *ThreadItem) SubAgentActivity() *SubAgentActivity {
	if t.Type != ItemTypeSubAgentActivity || len(t.Raw) == 0 {
		return nil
	}
	var a SubAgentActivity
	if json.Unmarshal(t.Raw, &a) != nil {
		return nil
	}
	return &a
}
