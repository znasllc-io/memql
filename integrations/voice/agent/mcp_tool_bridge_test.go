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

// TestIsExposableTool_FullSurface covers the #1416 + #1420 contract: every
// server-executed tool is exposable (authorization is the server's job), and a
// client-executed tool is exposable WHEN a relay-capable browser path is
// available (#1420 routes a voice-agent client-tool call through the cross-node
// relay) -- and withheld only when no relay path exists.
func TestIsExposableTool_FullSurface(t *testing.T) {
	assert.True(t, isExposableTool(false, false), "server-executed tools are exposed regardless of relay capability")
	assert.True(t, isExposableTool(false, true), "server-executed tools are exposed regardless of relay capability")
	assert.True(t, isExposableTool(true, true), "client-executed tools are exposed when a relay/browser path is available (#1420)")
	assert.False(t, isExposableTool(true, false), "client-executed tools are withheld when no relay/browser path exists")
}

// TestRealtimeTools_ExposesFullSurface verifies the bridge hands the agent's
// FULL tool surface to the model (#1416 -- the #458 low-risk tier is retired;
// write/control/computer_use tools are exposed and gated server-side). With a
// nil transport the bridge is NOT relay-capable, so client-executed tools stay
// withheld (the #1420 exposure guard: no browser path -> not exposed).
func TestRealtimeTools_ExposesFullSurface(t *testing.T) {
	tools := []*memqlv1.ToolDefinition{
		td("webSearch", false, "read"),
		td("knowledgeLookup", false),
		td("copresent_control", false, "control"),
		td("computerUse", false, "computer_use"),
		td("uiClick", true), // client_execution; nil transport -> not relay-capable -> withheld
		td("mutationCreateSpace", false, "create"),
		td("someFutureTool", false),
	}
	b := NewMcpToolBridge(nil, nil, tools, "s1", "ga1", nil)
	rt := b.RealtimeTools()

	require.Len(t, rt, 6, "every server-executed tool is exposed")
	exposed := map[string]bool{}
	for _, tool := range rt {
		exposed[tool.Name] = true
		assert.Equal(t, "function", tool.Type)
		assert.NotEmpty(t, tool.Parameters, "parameters carry the input schema")
	}
	for _, name := range []string{"webSearch", "knowledgeLookup", "copresent_control", "computerUse", "mutationCreateSpace", "someFutureTool"} {
		assert.True(t, exposed[name], "expected %s exposed", name)
	}
	assert.False(t, exposed["uiClick"], "client-executed tool withheld with no relay path")

	assert.ElementsMatch(t,
		[]string{"webSearch", "knowledgeLookup", "copresent_control", "computerUse", "mutationCreateSpace", "someFutureTool"},
		b.ExposedToolNames())
	assert.ElementsMatch(t, []string{"uiClick"}, b.DeniedToolNames())
}

// TestRealtimeTools_ExposesClientToolsWhenRelayCapable verifies the #1420
// contract: with a relay-capable transport (the production CallTool transport,
// which the engine relays to the browser for a voice-agent client-tool call),
// client-executed (browser-UI) tools ARE handed to the model alongside the
// server-executed surface.
func TestRealtimeTools_ExposesClientToolsWhenRelayCapable(t *testing.T) {
	tools := []*memqlv1.ToolDefinition{
		td("webSearch", false, "read"),
		td("uiClick", true),          // client_execution -> exposed via relay
		td("uiType", true),           // client_execution -> exposed via relay
		td("copresent_control", false, "control"),
	}
	// A non-nil transport makes the bridge relay-capable by default.
	transport := func(_ context.Context, _ string, _ map[string]any) (string, bool, error) {
		return "ok", false, nil
	}
	b := NewMcpToolBridge(transport, nil, tools, "s1", "ga1", nil)
	rt := b.RealtimeTools()

	exposed := map[string]bool{}
	for _, tool := range rt {
		exposed[tool.Name] = true
	}
	assert.True(t, exposed["uiClick"], "client tool exposed when relay-capable (#1420)")
	assert.True(t, exposed["uiType"], "client tool exposed when relay-capable (#1420)")
	assert.True(t, exposed["webSearch"], "server tool still exposed")
	assert.True(t, exposed["copresent_control"], "server tool still exposed")
	assert.ElementsMatch(t,
		[]string{"webSearch", "uiClick", "uiType", "copresent_control"},
		b.ExposedToolNames())
	assert.Empty(t, b.DeniedToolNames(), "nothing denied when relay-capable")

	// SetRelayCapable(false) forces the audio-only guard: client tools withheld.
	b.SetRelayCapable(false)
	rt = b.RealtimeTools()
	exposed = map[string]bool{}
	for _, tool := range rt {
		exposed[tool.Name] = true
	}
	assert.False(t, exposed["uiClick"], "client tool withheld when relay forced off")
	assert.False(t, exposed["uiType"], "client tool withheld when relay forced off")
	assert.True(t, exposed["webSearch"], "server tool still exposed")
	assert.ElementsMatch(t, []string{"uiClick", "uiType"}, b.DeniedToolNames())
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
	assert.Equal(t, "s1", mirrored[0].PartitionID)
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
// (absent from the agent's registry) is refused at dispatch -- defence in
// depth so the model can only run tools the bridge actually handed it.
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
