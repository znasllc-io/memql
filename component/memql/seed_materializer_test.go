package memql

import (
	"strings"
	"testing"
)

// renderArgsObject is the materializer's mutation-arg renderer.
// MemQL mutation calls require bare-identifier keys, not JSON-style
// quoted keys; an earlier raw-JSON implementation looked fine in
// Go but the engine's mutation parser rejected the calls silently
// (the materializer logged success but no rows landed). These tests
// pin the bare-key format + nested-block handling.

func TestRenderArgsObject_BareKeysScalarValues(t *testing.T) {
	got, err := renderArgsObject(map[string]any{
		"agentId":     "generalAssistant-user-abc",
		"name":        "General Assistant",
		"active":      true,
		"deleted":     false,
		"description": "Has a \"quote\" in it",
	})
	if err != nil {
		t.Fatalf("renderArgsObject: %v", err)
	}
	// Keys are sorted alphabetically for log-diff stability.
	want := `{active: true, agentId: "generalAssistant-user-abc", deleted: false, description: "Has a \"quote\" in it", name: "General Assistant"}`
	if got != want {
		t.Errorf("got:  %s\nwant: %s", got, want)
	}
}

func TestRenderArgsObject_NestedBlocks(t *testing.T) {
	got, err := renderArgsObject(map[string]any{
		"agentId": "ga-jose",
		"capabilities": map[string]any{
			"avatar":  true,
			"claw":    false,
			"domains": []any{"general", "copresent_ui"},
			"tools":   []any{},
		},
		"providerConfig": map[string]any{
			"llm": map[string]any{
				"policyName":  "balancedChat",
				"temperature": 0.7,
				"maxTokens":   int64(4000),
			},
		},
	})
	if err != nil {
		t.Fatalf("renderArgsObject: %v", err)
	}
	// Verify the key invariants rather than the whole string (sort
	// order pins it but the assertion is more readable this way).
	mustContain(t, got, `agentId: "ga-jose"`)
	mustContain(t, got, `capabilities: {`)
	mustContain(t, got, `avatar: true`)
	mustContain(t, got, `claw: false`)
	mustContain(t, got, `domains: ["general", "copresent_ui"]`)
	mustContain(t, got, `tools: []`)
	mustContain(t, got, `providerConfig: {`)
	mustContain(t, got, `llm: {`)
	mustContain(t, got, `policyName: "balancedChat"`)
	mustContain(t, got, `temperature: 0.7`)
	mustContain(t, got, `maxTokens: 4000`)

	// Bare identifier keys ONLY -- no quoted keys allowed (this is
	// the bug the rewrite fixes: json.Marshal produced `"agentId":`
	// which the mutation parser rejected).
	for _, badKey := range []string{`"agentId"`, `"capabilities"`, `"llm"`, `"policyName"`} {
		if strings.Contains(got, badKey) {
			t.Errorf("output contains JSON-style quoted key %q -- MemQL mutation parser rejects this:\n%s", badKey, got)
		}
	}
}

func TestRenderArgsObject_EmptyMap(t *testing.T) {
	got, err := renderArgsObject(map[string]any{})
	if err != nil {
		t.Fatalf("renderArgsObject: %v", err)
	}
	if got != "{}" {
		t.Errorf("empty map should render as `{}`, got %q", got)
	}
}

func TestRenderArgsObject_NilValue(t *testing.T) {
	got, err := renderArgsObject(map[string]any{
		"id":      "x",
		"missing": nil,
	})
	if err != nil {
		t.Fatalf("renderArgsObject: %v", err)
	}
	mustContain(t, got, "missing: null")
	mustContain(t, got, `id: "x"`)
}

func TestBuildArgsFromBody_PerUserStampsConceptIdAndOwner(t *testing.T) {
	body := seedBlock{
		fields: map[string]seedValue{
			"name": {kind: seedString, str: "General Assistant"},
			"role": {kind: seedString, str: "general_assistant"},
		},
	}
	body.keys = []string{"name", "role"}

	args := buildArgsFromBody(body, "agent", "generalAssistant-user-jose", "user-jose")

	// The synthetic id field uses the conceptName+Id convention.
	if args["agentId"] != "generalAssistant-user-jose" {
		t.Errorf("agentId = %v, want generalAssistant-user-jose", args["agentId"])
	}
	// ownerUserId is stamped from the user context.
	if args["ownerUserId"] != "user-jose" {
		t.Errorf("ownerUserId = %v, want user-jose", args["ownerUserId"])
	}
	// Body fields pass through by name.
	if args["name"] != "General Assistant" {
		t.Errorf("name = %v, want General Assistant", args["name"])
	}
	if args["role"] != "general_assistant" {
		t.Errorf("role = %v, want general_assistant", args["role"])
	}
}

func TestBuildArgsFromBody_GlobalUsesBodyIdNotOwner(t *testing.T) {
	body := seedBlock{
		fields: map[string]seedValue{
			"partitionType": {kind: seedString, str: "standard"},
			"displayName":   {kind: seedString, str: "Default"},
		},
	}
	body.keys = []string{"partitionType", "displayName"}

	// For @scope("global"), the materializer passes idVal from
	// body.fields["id"] and ownerUserId="" (no user context).
	args := buildArgsFromBody(body, "partition", "default", "")

	if args["partitionId"] != "default" {
		t.Errorf("partitionId = %v, want default", args["partitionId"])
	}
	if _, hasOwner := args["ownerUserId"]; hasOwner {
		t.Errorf("global seed must not stamp ownerUserId, got %v", args["ownerUserId"])
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain %q\nfull output:\n%s", needle, haystack)
	}
}
