package memql

import (
	"reflect"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestApplyToolDefaults_FillsMissing(t *testing.T) {
	tool := &Tool{Name: "t"}
	args := map[string]any{"keyword": "go"}
	defaults := map[string]any{"spaceId": "v1:cognition:space:abc", "keyword": "ignored"}

	got := applyToolDefaults(tool, args, defaults)
	want := map[string]any{
		"keyword": "go",                       // LLM-supplied value preserved
		"spaceId": "v1:cognition:space:abc",   // default fills missing
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyToolDefaults = %v, want %v", got, want)
	}
}

func TestApplyToolDefaults_AutoInjectedServerWins(t *testing.T) {
	tool := &Tool{
		Name:               "t",
		AutoInjectedFields: []string{"spaceId", "agentId"},
	}
	// LLM tries to forge spaceId + agentId. Server stamps the real
	// values via defaults. Server values must win.
	args := map[string]any{
		"spaceId": "v1:cognition:space:FORGED",
		"agentId": "v1:agents:agent:FORGED",
		"keyword": "go", // not auto-injected, LLM value preserved
	}
	defaults := map[string]any{
		"spaceId":       "v1:cognition:space:real",
		"agentId":       "v1:agents:agent:real",
		"participantId": "v1:cognition:participant:real",
	}

	got := applyToolDefaults(tool, args, defaults)
	want := map[string]any{
		"spaceId":       "v1:cognition:space:real",       // server wins
		"agentId":       "v1:agents:agent:real",          // server wins
		"keyword":       "go",                            // LLM-supplied non-auto-injected preserved
		"participantId": "v1:cognition:participant:real", // default fills missing
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyToolDefaults =\n  got:  %v\n  want: %v", got, want)
	}
}

func TestApplyToolDefaults_AutoInjectedNoDefaultStripsLLMValue(t *testing.T) {
	// LLM-supplied value for an auto-injected field where the server
	// has no default. Defense in depth: drop the LLM value rather
	// than let it ride through (a forged value past a runtime that
	// forgot to stamp the default would otherwise reach the handler).
	tool := &Tool{
		Name:               "t",
		AutoInjectedFields: []string{"ownerUserId"},
	}
	args := map[string]any{
		"ownerUserId": "v1:identity:user:FORGED",
		"keyword":     "go",
	}
	defaults := map[string]any{
		// note: no ownerUserId here
	}

	got := applyToolDefaults(tool, args, defaults)
	want := map[string]any{
		"keyword": "go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyToolDefaults =\n  got:  %v\n  want: %v", got, want)
	}
	if _, present := got["ownerUserId"]; present {
		t.Error("auto-injected ownerUserId survived when server had no default; should have been stripped")
	}
}

func TestApplyToolDefaults_NilToolPreservesLegacyBehavior(t *testing.T) {
	// Unknown-tool dispatch (tool lookup failed): no validator.
	// Defaults still fill missing fields; LLM-supplied values
	// preserved (including for fields that WOULD be auto-injected
	// on a known tool). The unknown-tool branch rejects the call
	// downstream so this isn't a leak.
	args := map[string]any{
		"ownerUserId": "forged",
	}
	defaults := map[string]any{
		"spaceId": "v1:cognition:space:real",
	}

	got := applyToolDefaults(nil, args, defaults)
	want := map[string]any{
		"ownerUserId": "forged", // unchanged -- no @autoInjected info available
		"spaceId":     "v1:cognition:space:real",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyToolDefaults(nil tool) =\n  got:  %v\n  want: %v", got, want)
	}
}

func TestApplyToolDefaults_NilArgsAllocatesWhenNeeded(t *testing.T) {
	// LLM emitted no args at all; defaults must materialize into a
	// fresh map so the handler receives the server values.
	tool := &Tool{Name: "t"}
	defaults := map[string]any{"spaceId": "v1:cognition:space:abc"}

	got := applyToolDefaults(tool, nil, defaults)
	want := map[string]any{"spaceId": "v1:cognition:space:abc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyToolDefaults(nil args) = %v, want %v", got, want)
	}
}

func TestApplyToolDefaults_NoDefaultsNoArgsReturnsNil(t *testing.T) {
	// No defaults, no LLM args, no auto-injected fields -- nothing
	// to do, return nil.
	tool := &Tool{Name: "t"}
	got := applyToolDefaults(tool, nil, nil)
	if got != nil {
		t.Errorf("applyToolDefaults(nil,nil) = %v, want nil", got)
	}
}

func TestApplyToolDefaults_AutoInjectedStripsEvenWhenNoDefaults(t *testing.T) {
	// Edge case: tool has @autoInjected fields but the runtime
	// passed no defaults at all. LLM forges an auto-injected field.
	// Without defaults, the merge must still walk
	// AutoInjectedFields and strip -- otherwise the LLM's forge
	// reaches the handler.
	tool := &Tool{
		Name:               "t",
		AutoInjectedFields: []string{"ownerUserId"},
	}
	args := map[string]any{
		"ownerUserId": "forged",
		"keyword":     "go",
	}
	got := applyToolDefaults(tool, args, nil)
	want := map[string]any{
		"keyword": "go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyToolDefaults(no defaults) =\n  got:  %v\n  want: %v", got, want)
	}
}

func TestToolParser_AutoInjectedAnnotation(t *testing.T) {
	// Parser-level smoke: a tool field with @autoInjected lands as
	// a Tool.AutoInjectedFields entry. The integration with
	// si_tool_loop is tested above; this just pins the parser path.
	src := `@enabled
@handler(type="function", name="myTool")
@description("test tool")
tool myTool {
  spaceId   string  @required @autoInjected @description("server-stamped")
  keyword   string  @description("LLM-supplied")
  ownerUserId string @autoInjected @description("server-stamped")
}`
	decl, err := langparser.ParseToolDecl(src)
	if err != nil {
		t.Fatalf("ParseToolDecl: %v", err)
	}
	tools, err := toolDeclToTool(decl, "test.memql")
	if err != nil {
		t.Fatalf("toolDeclToTool: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	got := tools[0].AutoInjectedFields
	want := []string{"spaceId", "ownerUserId"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoInjectedFields = %v, want %v", got, want)
	}
}
