package codexcli

import (
	"encoding/json"
	"testing"
)

// helper assertions for fixture-based tests. Kept in a _test.go file so
// they don't bleed into the public API.

// expectRequest reads the next frame and asserts it is a request with
// the given method. Returns the request id and raw params for the test
// to inspect or respond to.
func expectRequest(t *testing.T, e *BidiFixtureExecutor, method string) (json.RawMessage, json.RawMessage) {
	t.Helper()
	f, err := e.ReadFrame()
	if err != nil {
		t.Fatalf("read %s: %v", method, err)
	}
	if !f.IsRequest() {
		t.Fatalf("expected request %q, got method=%q id=%s", method, f.Method, string(f.ID))
	}
	if f.Method != method {
		t.Fatalf("expected method %q, got %q (id=%s)", method, f.Method, string(f.ID))
	}
	return f.ID, f.Params
}

// expectNotification reads the next frame and asserts it is a
// notification with the given method.
func expectNotification(t *testing.T, e *BidiFixtureExecutor, method string) json.RawMessage {
	t.Helper()
	f, err := e.ReadFrame()
	if err != nil {
		t.Fatalf("read %s notification: %v", method, err)
	}
	if !f.IsNotification() {
		t.Fatalf("expected notification %q, got method=%q id=%s", method, f.Method, string(f.ID))
	}
	if f.Method != method {
		t.Fatalf("expected notification method %q, got %q", method, f.Method)
	}
	return f.Params
}

// expectResponse reads the next frame and asserts it is a response with
// matching id.
func expectResponse(t *testing.T, e *BidiFixtureExecutor, wantID json.RawMessage) rpcFrame {
	t.Helper()
	f, err := e.ReadFrame()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !f.IsResponse() {
		t.Fatalf("expected response, got method=%q id=%s", f.Method, string(f.ID))
	}
	if string(f.ID) != string(wantID) {
		t.Fatalf("response id = %s, want %s", string(f.ID), string(wantID))
	}
	return f
}
