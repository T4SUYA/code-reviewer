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
// turn to answer instead of asking for more context. With the downgrade
// policy a "claims" branch lets the model report, before it finishes, which
// findings assert facts about something outside the repository; the final
// turn leaves it out with the tools. A finding may also carry
// external_claim and verified_by itself.
func agentResponseSchema(tools *ToolRegistry, withTools bool, policy agentPolicy) *genai.Schema {
	str := &genai.Schema{Type: genai.TypeString}
	num := &genai.Schema{Type: genai.TypeInteger}

	var branches []*genai.Schema
	if withTools {
		branches = tools.schemaBranches()
	}
	branches = append(branches, &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"action":   {Type: genai.TypeString, Enum: []string{"findings"}},
			"findings": {Type: genai.TypeArray, Items: findingSchema(str, num, policy)},
		},
		Required:         []string{"action", "findings"},
		PropertyOrdering: []string{"action", "findings"},
	})
	branches = append(branches, &genai.Schema{
		Type:       genai.TypeObject,
		Properties: map[string]*genai.Schema{"action": {Type: genai.TypeString, Enum: []string{"done"}}},
		Required:   []string{"action"},
	})
	if policy.downgrade && withTools {
		branches = append(branches, claimsSchema(str, num))
	}
	return &genai.Schema{AnyOf: branches}
}

// claimsSchema is the claims action of the downgrade policy: for each
// finding, by position, whether it asserts a fact about something outside the
// repository and which tool calls confirm it.
func claimsSchema(str, num *genai.Schema) *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"action": {Type: genai.TypeString, Enum: []string{"claims"}},
			"claims": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"index":          num,
						"external_claim": {Type: genai.TypeBoolean},
						"verified_by":    {Type: genai.TypeArray, Items: str},
					},
					Required:         []string{"index", "external_claim", "verified_by"},
					PropertyOrdering: []string{"index", "external_claim", "verified_by"},
				},
			},
		},
		Required:         []string{"action", "claims"},
		PropertyOrdering: []string{"action", "claims"},
	}
}

// findingSchema mirrors model.Finding's JSON fields. A refined finding replaces
// the initial one, so a field missing here would be dropped from the result.
// Location and content fields are required: the API treats any other field as
// optional, and a refined finding without its line or old_line no longer
// matches its initial finding, which would silently dismiss it.
func findingSchema(str, num *genai.Schema, policy agentPolicy) *genai.Schema {
	schema := &genai.Schema{
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
	if policy.downgrade {
		schema.Properties["external_claim"] = &genai.Schema{Type: genai.TypeBoolean}
		schema.Properties["verified_by"] = &genai.Schema{Type: genai.TypeArray, Items: str}
	}
	if policy.explore {
		schema.Properties["evidence"] = &genai.Schema{
			Type: genai.TypeArray,
			Items: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"tool_call_id": str, "file": str, "start_line": num, "end_line": num,
				},
				Required: []string{"tool_call_id", "file", "start_line", "end_line"},
			},
		}
	}
	return schema
}
