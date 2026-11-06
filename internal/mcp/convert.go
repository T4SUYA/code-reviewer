package mcp

import "encoding/json"

// toMap converts a schema value received over the wire (already decoded into
// maps, or any JSON-marshalable value) into a plain map.
func toMap(v any) (map[string]any, error) {
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
