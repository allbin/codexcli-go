package schema

// Thread naming. thread/name/set is stable; codex announces every change,
// including its own, with thread/name/updated.
const (
	// MethodThreadNameSet is the `thread/name/set` request.
	MethodThreadNameSet = "thread/name/set"
	// MethodThreadNameUpdated is the `thread/name/updated` notification.
	MethodThreadNameUpdated = "thread/name/updated"
)

// ThreadSetNameParams is the `thread/name/set` payload. Codex 0.159.3
// rejects an empty name. The response is an empty object.
type ThreadSetNameParams struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

// ThreadNameUpdatedNotification is the `thread/name/updated` payload.
// ThreadName is nil when the name was cleared.
type ThreadNameUpdatedNotification struct {
	ThreadID   string  `json:"threadId"`
	ThreadName *string `json:"threadName,omitempty"`
}
