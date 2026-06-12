package agent

// mcp_tool_bridge.go exposes memQL's tool surface to the gpt-realtime model
// via async function calling, and mirrors every model-driven tool call back
// into cognition so cognition is never blind to what the model did (#458).
//
// Full surface, server-enforced (#1416 -- owner decision, retiring the
// #435/#458 client-side risk tier). The realtime model gets the SAME tool
// list the text loop binds: every ToolDefinition the agent's ListToolsMsg
// returns. Authorization is enforced where it already lives for every caller
// -- server-side at handleCallTool (the per-tool role gate via
// IsAllowedForRole) and in the engine's own scope / kill-switch /
// agentAuthorization checks. The text loop's model picks tools freely under
// exactly those gates; the realtime model is no different in kind, so a
// client-side allowlist in front of the locked door added denial without
// adding enforcement.
//
// ONE mechanical exclusion -- NOT a policy tier: client-executed tools
// (ClientExecution=true, the browser-UI drive surface). handleCallTool's
// executeClientTool emits the ClientToolCall on the CALLING session and parks
// for its ClientToolResult -- and the calling session here is the voice-agent
// process, which has no browser on it, so the call can only time out. Routing
// those through the cross-node client-tool relay (the
// v1:cognition:client:tool:request graph rows) is the #1416 follow-up; until
// then they are excluded for transport reasons and logged as such.
//
// Awareness mirror. When the model calls a tool, the bridge (1) executes it
// through memQL's MCP surface (CallToolMsg -> CallToolResult), the same
// authorized backend path the text loop uses, so server-side enforcement is
// the source of truth; and (2) emits a best-effort mirror of the call +
// result into cognition so transcripts / history / the conductor / the
// chat-canvas affordance stay coherent. The mirror is best-effort: a mirror
// failure must NEVER fail the tool call the model is awaiting (it still gets
// its result; we only lose the awareness breadcrumb, which we log).
//
// Everything here is pure Go and CGO-free: the exposure decision, the
// RealtimeTool build, and the dispatch/mirror flow are exercised in the
// default lane with a stubbed transport + mirror. The live wiring (ListTools
// fetch + CallTool transport) is thin glue over the existing gRPC Client.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/integrations/openai"
)

// isExposableTool reports whether a tool may be handed to the realtime model.
// Full surface (#1416): every registry tool is exposed; authorization is the
// server's job (handleCallTool role gate + engine scope / kill-switch checks),
// identical to the text loop. The ONLY exclusion is mechanical, not policy:
// a client-executed tool needs a browser on the calling session to satisfy
// its ClientToolCall round-trip, and the voice-agent's session has none --
// the call would park until timeout. Goes away once the #1416 follow-up
// routes client tools through the cross-node client-tool relay.
func isExposableTool(clientExecution bool) bool {
	return !clientExecution
}

// ToolCallTransport executes a tool against memQL's MCP surface and
// returns (resultText, isError). The production transport sends a CallToolMsg
// on the stream and awaits the CallToolResult; tests inject a fake. Running the
// model-driven call through memQL's CallTool path (rather than any direct
// provider tool) is itself part of the awareness contract: the call traverses
// the same authorized backend path the text loop uses, so server-side
// enforcement (role gate, scopes, kill switch) is the source of truth.
type ToolCallTransport func(ctx context.Context, name string, arguments map[string]any) (string, bool, error)

// MirrorRecord is one awareness breadcrumb for a model-driven tool call. It is
// a plain value so it is trivially serializable and inspectable in tests.
// Mirrors the dict mcp_tool_bridge.py::CognitionMirror.record_call emits.
type MirrorRecord struct {
	SpaceID       string
	AgentID       string
	ToolName      string
	ArgumentsJSON string
	ResultText    string
	IsError       bool
}

// MirrorSink receives one MirrorRecord per model-driven tool call. The default
// sink threads it into cognition; tests substitute a recording sink. Mirrors
// mcp_tool_bridge.py::MirrorSink.
type MirrorSink func(ctx context.Context, record MirrorRecord) error

// McpToolBridge builds the realtime tool set (full surface, #1416) and mirrors every
// model-driven call. Constructed with the raw registry tool definitions
// (already fetched via ListToolsMsg), the transport that runs a tool via
// CallToolMsg, the cognition mirror, and the space/agent attribution. Mirrors
// mcp_tool_bridge.py::McpToolBridge.
type McpToolBridge struct {
	transport ToolCallTransport
	mirror    MirrorSink
	tools     []*memqlv1.ToolDefinition
	spaceID   string
	agentID   string
	logger    *slog.Logger

	exposed []string
	denied  []string
}

// NewMcpToolBridge builds a bridge. A nil mirror disables mirroring (the call
// still executes); a nil transport makes every model call error (the bridge is
// still safe to build for its exposure-partition side).
func NewMcpToolBridge(
	transport ToolCallTransport,
	mirror MirrorSink,
	tools []*memqlv1.ToolDefinition,
	spaceID, agentID string,
	logger *slog.Logger,
) *McpToolBridge {
	return &McpToolBridge{
		transport: transport,
		mirror:    mirror,
		tools:     tools,
		spaceID:   strings.TrimSpace(spaceID),
		agentID:   strings.TrimSpace(agentID),
		logger:    logger,
	}
}

// ExposedToolNames returns the names handed to the model (set after
// RealtimeTools).
func (b *McpToolBridge) ExposedToolNames() []string { return append([]string(nil), b.exposed...) }

// DeniedToolNames returns the names withheld from the model
// (the mechanical client-execution exclusion; set after RealtimeTools).
func (b *McpToolBridge) DeniedToolNames() []string { return append([]string(nil), b.denied...) }

// RealtimeTools returns the openai.RealtimeTool declarations the
// session.update carries -- the agent's full tool surface (#1416), minus the
// mechanical client-execution exclusion (see isExposableTool). As a side
// effect it records the exposed / denied partition for diagnostics.
func (b *McpToolBridge) RealtimeTools() []openai.RealtimeTool {
	b.exposed = b.exposed[:0]
	b.denied = b.denied[:0]
	var out []openai.RealtimeTool
	for _, td := range b.tools {
		if td == nil {
			continue
		}
		name := td.GetName()
		if isExposableTool(td.GetClientExecution()) {
			b.exposed = append(b.exposed, name)
			out = append(out, openai.RealtimeTool{
				Type:        "function",
				Name:        name,
				Description: td.GetDescription(),
				Parameters:  rawSchemaParameters(td.GetInputSchema()),
			})
		} else {
			b.denied = append(b.denied, name)
		}
	}
	if b.logger != nil {
		b.logger.Info("voice-agent realtime mcp bridge: tool surface resolved",
			"exposed_count", len(b.exposed), "exposed", strings.Join(b.exposed, ","),
			"denied_count", len(b.denied), "denied", strings.Join(b.denied, ","))
	}
	return out
}

// IsExposed reports whether the bridge handed name to the model. Used by the
// dispatch path to refuse a function call for a tool that was never exposed
// (defence in depth: the model should only call exposed tools, but a refusal
// here means a tool absent from the agent's registry can never run via the
// model path).
func (b *McpToolBridge) IsExposed(name string) bool {
	for _, n := range b.exposed {
		if n == name {
			return true
		}
	}
	return false
}

// Dispatch runs one model-driven function call: it parses the JSON arguments,
// refuses any tool that was never exposed, executes the call through
// the transport, mirrors the call + result into cognition (best-effort), and
// returns the result text to hand back to the model as the function output.
// The returned string is ALWAYS suitable to send back as the function output
// (an error is surfaced to the model as text, matching the text loop, so it can
// retry / pick another tool). Mirrors mcp_tool_bridge.py handler semantics.
func (b *McpToolBridge) Dispatch(ctx context.Context, name, argumentsJSON string) string {
	if !b.IsExposed(name) {
		// Defence in depth: a non-exposed tool was never given to the model;
		// refuse rather than dispatch so a privileged tool can never run here.
		if b.logger != nil {
			b.logger.Warn("voice-agent realtime mcp bridge: refusing non-exposed tool call",
				"tool", name)
		}
		return fmt.Sprintf("[tool error] tool %q is not available to the voice model", name)
	}

	args := parseToolArguments(argumentsJSON)
	if b.transport == nil {
		return "[tool error] no tool transport configured"
	}

	// #1426: the transport leg is the synchronous memQL CallTool round-trip
	// (server-side engine + DB work) inside the tool span -- time it
	// individually so it separates from the model-side legs.
	transportStart := time.Now()
	resultText, isError, err := b.transport(ctx, name, args)
	logVoiceTiming(b.logger, "tool.transport", transportStart,
		"space_id", b.spaceID, "tool", name, "is_error", isError || err != nil)
	if err != nil {
		resultText = err.Error()
		isError = true
	}

	// Mirror BEFORE returning so cognition sees the call even if the model
	// immediately barges on. Mirror failure is swallowed -- it never fails the
	// call the model is awaiting.
	b.recordMirror(ctx, name, args, resultText, isError)

	if isError {
		return "[tool error] " + resultText
	}
	return resultText
}

// recordMirror emits the awareness breadcrumb best-effort. A nil sink or a
// sink error is logged and swallowed -- awareness must never break the call.
// Mirrors mcp_tool_bridge.py::CognitionMirror.record_call.
func (b *McpToolBridge) recordMirror(ctx context.Context, name string, args map[string]any, resultText string, isError bool) {
	if b.mirror == nil {
		return
	}
	argsJSON, err := marshalSortedJSON(args)
	if err != nil {
		argsJSON = "{}"
	}
	record := MirrorRecord{
		SpaceID:       b.spaceID,
		AgentID:       b.agentID,
		ToolName:      name,
		ArgumentsJSON: argsJSON,
		ResultText:    resultText,
		IsError:       isError,
	}
	if err := b.mirror(ctx, record); err != nil && b.logger != nil {
		b.logger.Warn("voice-agent realtime mcp bridge: cognition mirror failed "+
			"(tool result still delivered to the model)",
			"tool", name, "space_id", b.spaceID, "err", err)
	}
}

// rawSchemaParameters renders a registry inputSchema (a JSON Schema string)
// into the parameters RawMessage the RealtimeTool carries. A tool that takes no
// arguments (or a non-JSON schema) defaults to an empty object schema rather
// than omitting parameters. Mirrors mcp_tool_bridge.py::ToolDef.raw_schema.
func rawSchemaParameters(inputSchema string) json.RawMessage {
	emptyObject := json.RawMessage(`{"type":"object","properties":{}}`)
	schema := strings.TrimSpace(inputSchema)
	if schema == "" {
		return emptyObject
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(schema), &parsed); err != nil {
		return emptyObject
	}
	if _, ok := parsed["type"]; !ok {
		return emptyObject
	}
	// Re-marshal so the stored bytes are canonical (and valid).
	out, err := json.Marshal(parsed)
	if err != nil {
		return emptyObject
	}
	return out
}

// parseToolArguments decodes the model's function-call arguments JSON into a
// map. A blank or malformed arguments string degrades to an empty map (the
// tool runs with no arguments) rather than failing the call.
func parseToolArguments(argumentsJSON string) map[string]any {
	args := map[string]any{}
	s := strings.TrimSpace(argumentsJSON)
	if s == "" {
		return args
	}
	if err := json.Unmarshal([]byte(s), &args); err != nil {
		return map[string]any{}
	}
	return args
}

// marshalSortedJSON marshals a map with sorted keys so the mirror's
// arguments_json is deterministic (matching the Python sort_keys=True so the
// breadcrumb is stable across runs and comparable in tests).
func marshalSortedJSON(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, err := json.Marshal(m[k])
		if err != nil {
			return "", err
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(kb)+":"+string(v))
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

// ---------------------------------------------------------------------------
// gRPC glue (production wiring)
// ---------------------------------------------------------------------------

// toolBridgeLister is the minimal gRPC surface for fetching the registry.
// Satisfied by *Client.
type toolBridgeLister interface {
	SendRequest(ctx context.Context, envelope *memqlv1.MemqlClientMessage) (*memqlv1.MemqlServerMessage, error)
}

// FetchToolDefinitions lists the memQL tool registry via ListToolsMsg. A failed
// list returns nil (not an error to the caller's session build): the realtime
// session simply comes up with no model-driven tools, a safe degradation
// (privileged tools were never exposed anyway, and the cognition tool loop is
// unaffected). Mirrors mcp_tool_bridge.py::wire_realtime_mcp_tools step 1.
func FetchToolDefinitions(ctx context.Context, client toolBridgeLister, logger *slog.Logger) []*memqlv1.ToolDefinition {
	if client == nil {
		return nil
	}
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ListTools{
			ListTools: &memqlv1.ListToolsMsg{},
		},
	}
	reply, err := client.SendRequest(ctx, envelope)
	if err != nil {
		if logger != nil {
			logger.Warn("voice-agent realtime mcp bridge: ListTools failed -- "+
				"realtime session comes up with no model-driven tools", "err", err)
		}
		return nil
	}
	result := reply.GetListToolsResult()
	if result == nil {
		return nil
	}
	return result.GetTools()
}

// NewGrpcToolCallTransport builds a ToolCallTransport backed by CallToolMsg on
// the stream. Mirrors mcp_tool_bridge.py::_grpc_call_tool_transport.
func NewGrpcToolCallTransport(client toolBridgeLister) ToolCallTransport {
	return func(ctx context.Context, name string, arguments map[string]any) (string, bool, error) {
		if client == nil {
			return "", true, fmt.Errorf("no tool transport client")
		}
		argsStruct, err := structpb.NewStruct(arguments)
		if err != nil {
			return "", true, fmt.Errorf("encode tool arguments: %w", err)
		}
		reply, err := client.SendRequest(ctx, &memqlv1.MemqlClientMessage{
			Payload: &memqlv1.MemqlClientMessage_CallTool{
				CallTool: &memqlv1.CallToolMsg{
					Name:      name,
					Arguments: argsStruct,
				},
			},
		})
		if err != nil {
			return "", true, err
		}
		callResult := reply.GetCallToolResult()
		if callResult == nil {
			return "tool returned no CallToolResult", true, nil
		}
		return flattenToolResultText(callResult.GetContent()), callResult.GetIsError(), nil
	}
}

// NewLogMirrorSink builds the default cognition-awareness sink: a structured
// voiceTrace-shaped log line carrying the call + result. Because the tool
// itself executed through memQL's CallTool path, the backend already saw the
// call; this breadcrumb makes it legible in the per-space trace that history /
// the conductor / the chat-canvas "searched the web" affordance consume. A
// dedicated cognition event message (threading the breadcrumb into the
// v1:cognition utterance/turn history as first-class structured data) is the
// integration seam left for the cognition-side follow-up; this sink is
// swappable without touching the bridge. Mirrors
// mcp_tool_bridge.py::_default_mirror_sink.
func NewLogMirrorSink(logger *slog.Logger) MirrorSink {
	return func(_ context.Context, record MirrorRecord) error {
		if logger == nil {
			return nil
		}
		result := record.ResultText
		if len(result) > 500 {
			result = result[:500]
		}
		logger.Info("voice trace: realtime mcp tool call",
			"stage", "realtime.mcp.tool.call",
			"space_id", record.SpaceID,
			"agent_id", record.AgentID,
			"tool", record.ToolName,
			"is_error", record.IsError,
			"arguments", record.ArgumentsJSON,
			"result", result)
		return nil
	}
}

// flattenToolResultText joins a CallToolResult's text content parts into one
// string, matching mcp_tool_bridge.py::_result_text.
func flattenToolResultText(content []*memqlv1.ToolResultContent) string {
	var parts []string
	for _, item := range content {
		if item == nil {
			continue
		}
		if t := item.GetText(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}
