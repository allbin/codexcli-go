package schema

// Thread deletion. thread/delete is stable; codex announces it with
// thread/deleted.
const (
	// MethodThreadDelete is the `thread/delete` request.
	MethodThreadDelete = "thread/delete"
	// MethodThreadDeleted is the `thread/deleted` notification.
	MethodThreadDeleted = "thread/deleted"
)

// ThreadDeleteParams is the `thread/delete` payload. The response is an
// empty object.
type ThreadDeleteParams struct {
	ThreadID string `json:"threadId"`
}

// ThreadDeletedNotification is the `thread/deleted` payload.
type ThreadDeletedNotification struct {
	ThreadID string `json:"threadId"`
}
