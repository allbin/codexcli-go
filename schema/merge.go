package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// mergeMarshal serializes typed plus extra fields into a single JSON
// object. Used by ThreadStartParams and TurnStartParams to expose any
// not-yet-modeled protocol surface without losing type safety on the
// fields we do know about. Extra keys overwrite typed keys, so a caller
// who knows the upstream protocol best can always win.
func mergeMarshal(typed any, extra map[string]any) ([]byte, error) {
	typedBytes, err := json.Marshal(typed)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return typedBytes, nil
	}
	var typedMap map[string]json.RawMessage
	if err := json.Unmarshal(typedBytes, &typedMap); err != nil {
		return nil, fmt.Errorf("merge: typed payload is not an object: %w", err)
	}
	for k, v := range extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("merge: encoding extra key %q: %w", k, err)
		}
		typedMap[k] = raw
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(typedMap); err != nil {
		return nil, err
	}
	// json.Encoder appends a trailing newline; strip it for tidy framing.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
