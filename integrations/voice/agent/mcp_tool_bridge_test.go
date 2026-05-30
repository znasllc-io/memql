package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func td(name string, clientExecution bool, scopes ...string) *memqlv1.ToolDefinition {
	return &memqlv1.ToolDefinition{
		Name:            name,
		Description:     name + " description",
		InputSchema:     `{"type":"object","properties":{"q":{"type":"string"}}}`,
		ClientExecution: clientExecution,
		Scopes:          scopes,
	}
}

// TestIsLowRiskTool_TierBoundary covers the default-deny tier decision:
// allowlisted read tools are exposed; privileged, untiered, client-executed,
// and write-scoped tools are withheld.
func TestIsLowRiskTool_TierBoundary(t *testing.T) {
	cases := []struct {
		name            string
		toolName        string
		clientExecution bool
		scopes          []string
		want            bool
	}{
		{"allowlisted read tool exposed", "webSearch", false, []string{"read"}, true},
		{"allowlisted snake_case exposed", "knowledge_lookup", false, nil, true},
		{"untiered tool withheld (default-deny)", "createSpace", false, nil, false},
		{"privileged scope withheld", "webSearch", false, []string{"write"}, false},
		{"computer_use scope withheld", "webSearch", false, []string{"computer_use"}, false},
		{"control scope withheld", "domainLookup", false, []string{"control"}, false},
		{"client_execution withheld", "webSearch", true, []string{"read"}, false},
		{"privileged tool name withheld", "copresent_control", false, []string{"control"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isLowRiskTool(tc.toolName, tc.clientExecution, tc.scopes))
		})
	}
}

// TestRealtimeTools_ExposesOnlyLowRisk verifies the bridge hands ONLY low-risk
// tools to the model and records the exposed / denied partition.
func TestRealtimeTools_ExposesOnlyLowRisk(t *testing.T) {
	tools := []*memqlv1.ToolDefinition{
		td("webSearch", false, "read"),
		td("knowledgeLookup", false),
		td("copresent_control", false, "control"),  // privileged scope
		td("computerUse", false, "computer_use"),   // privileged scope
		td("uiClick", true),                        // client_execution
		td("mutationCreateSpace", false, "create"), // privileged scope + untiered
		td("someFutureTool", false),                // untiered -> default-deny
	}
	b := NewMcpToolBridge(nil, nil, tools, "s1", "ga1", nil)
	rt := b.RealtimeTools()

	require.Len(t, rt, 2, "only the two low-risk read tools are exposed")
	exposed := map[string]bool{}
	for _, tool := range rt {
		exposed[tool.Name] = true
		assert.Equal(t, "function", tool.Type)
		assert.NotEmpty(t, tool.Parameters, "parameters carry the input schema")
	}
	assert.True(t, exposed["webSearch"])
	assert.True(t, exposed["knowledgeLookup"])

	assert.ElementsMatch(t, []string{"webSearch", "knowledgeLookup"}, b.ExposedToolNames())
	assert.ElementsMatch(t,
		[]string{"copresent_control", "computerUse", "uiClick", "mutationCreateSpace", "someFutureTool"},
		b.DeniedToolNames())
}

// TestRealtimeTools_EmptySchemaDefaultsToObject verifies a tool with no input
// schema is exposed with an empty object schema rather than omitting params.
func TestRealtimeTools_EmptySchemaDefaultsToObject(t *testing.T) {
	tool := &memqlv1.ToolDefinition{Name: "webSearch", InputSchema: ""}
	b := NewMcpToolBridge(nil, nil, []*memqlv1.ToolDefinition{tool}, "s1", "ga1", nil)
	rt := b.RealtimeTools()
	require.Len(t, rt, 1)
	assert.JSONEq(t, `{"type":"object","properties":{}}`, string(rt[0].Parameters))
}

// TestDispatch_ExecutesAndMirrors verifies a model-driven call runs through the
// transport and is mirrored into cognition with the call + result.
func TestDispatch_ExecutesAndMirrors(t *testing.T) {
	var mu sync.Mutex
	var transportCalls []string
	transport := func(_ context.Context, name string, args map[string]any) (string, bool, error) {
		mu.Lock()
		transportCalls = append(transportCalls, name)
		mu.Unlock()
		return "search results for " + args["q"].(string), false, nil
	}
	var mirrored []MirrorRecord
	mirror := func(_ context.Context, rec MirrorRecord) error {
		mu.Lock()
		mirrored = append(mirrored, rec)
		mu.Unlock()
		return nil
	}

	b := NewMcpToolBridge(transport, mirror, []*memqlv1.ToolDefinition{td("webSearch", false, "read")}, "s1", "ga1", nil)
	b.RealtimeTools() // populate exposed set

	out := b.Dispatch(context.Background(), "webSearch", `{"q":"memql"}`)
	assert.Equal(t, "search results for memql", out)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, transportCalls, 1)
	require.Len(t, mirrored, 1, "every model-driven call is mirrored into cognition")
	assert.Equal(t, "webSearch", mirrored[0].ToolName)
	assert.Equal(t, "s1", mirrored[0].SpaceID)
	assert.Equal(t, "ga1", mirrored[0].AgentID)
	assert.False(t, mirrored[0].IsError)
	assert.JSONEq(t, `{"q":"memql"}`, mirrored[0].ArgumentsJSON)
	assert.Equal(t, "search results for memql", mirrored[0].ResultText)
}

// TestDispatch_MirrorFailureToleratedl verifies a mirror failure does NOT fail
// the tool call the model is awaiting -- the result is still returned.
func TestDispatch_MirrorFailureTolerated(t *testing.T) {
	transport := func(_ context.Context, _ string, _ map[string]any) (string, bool, error) {
		return "ok result", false, nil
	}
	mirror := func(_ context.Context, _ MirrorRecord) error {
		return errors.New("cognition mirror unreachable")
	}
	b := NewMcpToolBridge(transport, mirror, []*memqlv1.ToolDefinition{td("webSearch", false, "read")}, "s1", "ga1", nil)
	b.RealtimeTools()

	out := b.Dispatch(context.Background(), "webSearch", `{}`)
	assert.Equal(t, "ok result", out, "mirror failure must not fail the call the model awaits")
}

// TestDispatch_RefusesNonExposedTool verifies a tool that was never exposed
// (privileged-by-default) is refused at dispatch -- defence in depth so a
// privileged tool can never run via the model path.
func TestDispatch_RefusesNonExposedTool(t *testing.T) {
	var transportCalled bool
	transport := func(_ context.Context, _ string, _ map[string]any) (string, bool, error) {
		transportCalled = true
		return "should not run", false, nil
	}
	b := NewMcpToolBridge(transport, nil, []*memqlv1.ToolDefinition{td("webSearch", false, "read")}, "s1", "ga1", nil)
	b.RealtimeTools()

	out := b.Dispatch(context.Background(), "mutationCreateSpace", `{}`)
	assert.Contains(t, out, "not available")
	assert.False(t, transportCalled, "a non-exposed tool must never reach the transport")
}

// TestDispatch_ErrorSurfacedToModel verifies a tool error is surfaced to the
// model as text (so it can retry / pick another tool) and still mirrored.
func TestDispatch_ErrorSurfacedToModel(t *testing.T) {
	transport := func(_ context.Context, _ string, _ map[string]any) (string, bool, error) {
		return "rate limited", true, nil
	}
	var mirrored []MirrorRecord
	mirror := func(_ context.Context, rec MirrorRecord) error {
		mirrored = append(mirrored, rec)
		return nil
	}
	b := NewMcpToolBridge(transport, mirror, []*memqlv1.ToolDefinition{td("webSearch", false, "read")}, "s1", "ga1", nil)
	b.RealtimeTools()

	out := b.Dispatch(context.Background(), "webSearch", `{}`)
	assert.Equal(t, "[tool error] rate limited", out)
	require.Len(t, mirrored, 1)
	assert.True(t, mirrored[0].IsError, "the error call is still mirrored")
}

// TestFetchToolDefinitions_OverStream verifies FetchToolDefinitions issues a
// ListTools request and returns the registry from the ListToolsResult.
func TestFetchToolDefinitions_OverStream(t *testing.T) {
	fs := newFakeStream()
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetListTools() == nil {
			return
		}
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_ListToolsResult{
				ListToolsResult: &memqlv1.ListToolsResult{
					Tools: []*memqlv1.ToolDefinition{td("webSearch", false, "read")},
				},
			},
		})
	}
	c := newTestClient(t, fs)

	defs := FetchToolDefinitions(context.Background(), c, nil)
	require.Len(t, defs, 1)
	assert.Equal(t, "webSearch", defs[0].GetName())
}

// TestGrpcToolCallTransport_OverStream verifies the transport sends a CallTool
// and flattens the CallToolResult content into the result text.
func TestGrpcToolCallTransport_OverStream(t *testing.T) {
	fs := newFakeStream()
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		call := env.GetCallTool()
		if call == nil {
			return
		}
		assert.Equal(t, "webSearch", call.GetName())
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_CallToolResult{
				CallToolResult: &memqlv1.CallToolResult{
					Content: []*memqlv1.ToolResultContent{
						{Type: "text", Text: "line one"},
						{Type: "text", Text: "line two"},
					},
					IsError: false,
				},
			},
		})
	}
	c := newTestClient(t, fs)

	transport := NewGrpcToolCallTransport(c)
	text, isErr, err := transport(context.Background(), "webSearch", map[string]any{"q": "x"})
	require.NoError(t, err)
	assert.False(t, isErr)
	assert.Equal(t, "line one\nline two", text)
}
