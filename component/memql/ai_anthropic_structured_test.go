package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// The shapes under test are the ones production actually produced on
// 2026-08-26: a structured invocation whose only conversation content is the
// rendered system prompt, and a chat fallback whose perfect JSON arrived
// wrapped in a ```json fence.

func factorySchema() common.StructuredSchema {
	return common.StructuredSchema{
		Name:        "agentFactoryDecision",
		Description: "match, extend or create",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string"},
				"reasoning": {"type": "string"}
			},
			"required": ["action", "reasoning"],
			"additionalProperties": false
		}`),
		Strict: true,
	}
}

func TestAnthropicStructuredParamsForceTheSchemaTool(t *testing.T) {
	params, err := anthropicStructuredParams(
		"claude-sonnet-4-6",
		map[string]any{"maxTokens": 4096},
		[]common.ChatMessage{{Role: "system", Content: "decide the specialist"}},
		factorySchema(),
	)
	if err != nil {
		t.Fatalf("assembly failed: %v", err)
	}

	if len(params.Tools) != 1 || params.Tools[0].OfTool == nil {
		t.Fatal("exactly one plain tool must carry the schema")
	}
	tool := params.Tools[0].OfTool
	if tool.Name != "agentFactoryDecision" {
		t.Fatalf("tool name should be the schema name, got %q", tool.Name)
	}
	if _, ok := tool.InputSchema.Properties.(map[string]any)["action"]; !ok {
		t.Fatal("the schema's properties must ride the tool's input_schema")
	}
	if len(tool.InputSchema.Required) != 2 {
		t.Fatalf("required fields must survive the lift, got %v", tool.InputSchema.Required)
	}

	if params.ToolChoice.OfTool == nil || params.ToolChoice.OfTool.Name != "agentFactoryDecision" {
		t.Fatal("tool choice must be FORCED to the schema tool -- auto would let the model answer in prose")
	}

	// The system-only conversation still carries a user turn: Anthropic
	// refuses an empty messages list (the memql#4636 class).
	if len(params.Messages) == 0 {
		t.Fatal("a system-only structured invocation must still produce a user turn")
	}
	if len(params.System) == 0 {
		t.Fatal("the rendered prompt must stay a system block")
	}
}

func TestAnthropicStructuredToolNameSanitized(t *testing.T) {
	got := anthropicStructuredToolName(common.StructuredSchema{Name: "cognition routing v2!"})
	if got != "cognition_routing_v2_" {
		t.Fatalf("sanitization should map disallowed runes to underscore, got %q", got)
	}
	if anthropicStructuredToolName(common.StructuredSchema{}) != "structured_result" {
		t.Fatal("an empty schema name needs a stable fallback")
	}
}

func TestStripJSONFences(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bare json untouched", `{"a":1}`, `{"a":1}`},
		{"json fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"upper fence", "```JSON\n{\"a\":1}\n```", `{"a":1}`},
		{"anonymous fence", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"whitespace around", "  ```json\n{\"a\":1}\n```  ", `{"a":1}`},
		{"prose stays prose", "not a fence at all", "not a fence at all"},
	}
	for _, c := range cases {
		if got := stripJSONFences(c.in); got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}

	// The production sample, verbatim shape: fenced JSON must round-trip
	// through json.Unmarshal after the strip.
	raw := "```json\n{\n  \"action\": \"create\",\n  \"reasoning\": \"the catalogs are empty\"\n}\n```"
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stripJSONFences(raw)), &decoded); err != nil {
		t.Fatalf("stripped fence must parse: %v", err)
	}
	if decoded["action"] != "create" {
		t.Fatal("content must survive the strip intact")
	}
	if strings.Contains(stripJSONFences(raw), "`") {
		t.Fatal("no backtick may survive")
	}
}
