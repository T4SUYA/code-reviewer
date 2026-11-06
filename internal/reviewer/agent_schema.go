package reviewer

import "google.golang.org/genai"

// agentResponseSchema constrains the refinement model to the JSON protocol
// described in the system prompt. The prompt alone is not enough: a model may
// read the tool descriptions as function signatures and attempt a native
// function call, which the API answers with finish reason
// MALFORMED_FUNCTION_CALL and no content. ResponseMIMEType and ToolConfig mode
// NONE do not prevent this; constrained decoding against a schema does.
//
// The root must be a bare AnyOf: the API rejects any other field beside it.
// The tool branches come from the registry, one per registered tool.
// Without withTools the tool branches are left out, which forces the final
// turn to answer instead of asking for more context.
func agentResponseSchema(tools *ToolRegistry, withTools bool) *genai.Schema {
	str := &genai.Schema{Type: genai.TypeString}
	num := &genai.Schema{Type: genai.TypeInteger}

	var branches []*genai.Schema
	if withTools {
		branches = tools.schemaBranches()
	}
	return &genai.Schema{AnyOf: append(branches,
		&genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action":   {Type: genai.TypeString, Enum: []string{"findings"}},
				"findings": {Type: genai.TypeArray, Items: findingSchema(str, num)},
			},
			Required:         []string{"action", "findings"},
			PropertyOrdering: []string{"action", "findings"},
		},
		&genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{"action": {Type: genai.TypeString, Enum: []string{"done"}}},
			Required:   []string{"action"},
		},
	)}
}

// findingSchema mirrors model.Finding's JSON fields. A refined finding replaces
// the initial one, so a field missing here would be dropped from the result.
// Location and content fields are required: the API treats any other field as
// optional, and a refined finding without its line or old_line no longer
// matches its initial finding, which would silently dismiss it.
func findingSchema(str, num *genai.Schema) *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"file": str, "line": num, "old_line": num, "end_line": num,
			"severity": str, "category": str, "title": str, "body": str,
			"suggestion": str, "code_snippet": str, "existing_code": str,
			"rule_name": str, "rule_source": str, "rule_file": str, "rule_url": str,
			"fingerprint": str, "anchor": str, "anchor_lines": num,
		},
		Required: []string{"file", "line", "old_line", "severity", "category", "title", "body"},
	}
}
