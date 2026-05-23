package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// tools.go -- MCP-shaped tool RPC surface + inbound client-tool
// dispatch. memql#174.
//
// Hand-rolled (parallels concept_browser.go) because the MCP wire
// messages -- ListToolsMsg / CallToolMsg / ClientToolCall /
// ClientToolResult -- are not DSL queries / mutations. They're the
// gRPC envelope for the tool-call portion of the agent loop, so
// sdk-gen has no input to generate from. Every OTHER tool-shaped
// call should reach for a typed generated method.
//
// Three surfaces ship here:
//
//   1. ListTools / CallTool -- the outbound RPC pair the consumer
//      uses to enumerate and invoke server-side tools.
//   2. ClientToolHandler + RegisterClientToolHandler -- the inbound
//      surface for client-executed tools. When the server's agent
//      loop resolves a tool marked client_execution=true it emits
//      a ClientToolCall; the SDK dispatches to the registered
//      handler and ships the returned result back as a
//      ClientToolResult correlated by call_id.
//   3. SDK-owned types (ToolDefinition / ClientToolCall / etc.)
//      mirror the proto fields per the no-memqlv1-in-consumers
//      rule (sdk/go/CLAUDE.md rule 2).

// -----------------------------------------------------------------
// SDK-owned types -- wrappers over the proto so consumers never see
// memqlv1.* in their imports.
// -----------------------------------------------------------------

// ToolDefinition is one entry from a ListTools response. Mirrors
// memqlv1.ToolDefinition.
type ToolDefinition struct {
	Name            string
	Description     string
	InputSchema     string // JSON Schema describing the tool's input args.
	ClientExecution bool   // when true, the tool runs in the connected client (see RegisterClientToolHandler).
	Scopes          []string
}

// ListToolsArgs is the optional argument shape for ListTools. Cursor
// is the MCP pagination token; pass "" for the first page.
type ListToolsArgs struct {
	Cursor string
}

// ListToolsResult is the SDK-owned shape returned by ListTools.
type ListToolsResult struct {
	Tools      []ToolDefinition
	NextCursor string // empty when there are no more pages.
}

// CallToolArgs is the input shape for CallTool. Arguments is a
// JSON-serialisable map; the SDK marshals it into the proto Struct
// envelope at the boundary.
type CallToolArgs struct {
	Name      string
	Arguments map[string]any
}

// ToolResultContent is one content fragment of a tool's reply.
// Type discriminates: "text" / "image" / "resource". Fields populate
// per type per MCP spec.
type ToolResultContent struct {
	Type     string
	Text     string
	MimeType string
	Data     string // base64 for "image".
	URI      string // resource locator for "resource".
}

// CallToolResult is the SDK-owned shape returned by CallTool.
type CallToolResult struct {
	Content []ToolResultContent
	IsError bool
}

// ClientToolCall is the inbound shape the SDK hands to a registered
// ClientToolHandler when the server pushes a client-execution tool
// invocation. The handler returns a *ClientToolResult; the SDK
// auto-ships it back over the stream correlated by CallId.
type ClientToolCall struct {
	CallId        string
	TurnId        string
	AgentId       string
	ToolName      string
	ArgumentsJson string // raw JSON; the handler unmarshals per-tool.
	TimeoutMs     int32
}

// ClientToolResult is what a ClientToolHandler returns. The SDK
// stamps CallId from the inbound call when shipping.
type ClientToolResult struct {
	Content      []ToolResultContent
	IsError      bool
	ErrorMessage string
}

// ClientToolHandler is the inbound-dispatch contract. The SDK calls
// the handler from a goroutine spawned for each incoming
// ClientToolCall; handlers may block (up to the call's TimeoutMs
// budget the server is willing to wait). A nil return is treated as
// is_error=true with a generic message so the agent's parked tool
// call always unblocks.
type ClientToolHandler func(ctx context.Context, call *ClientToolCall) *ClientToolResult

// -----------------------------------------------------------------
// Outbound RPCs (ListTools / CallTool)
// -----------------------------------------------------------------

// ListTools issues a ListToolsMsg over the dispatcher and returns the
// SDK-shape ListToolsResult. Pass an empty Cursor for the first page.
func (qc *QueryClient) ListTools(ctx context.Context, args ListToolsArgs) (*ListToolsResult, error) {
	req := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ListTools{
			ListTools: &memqlv1.ListToolsMsg{
				Cursor: args.Cursor,
			},
		},
	}
	resp, err := qc.dispatcher.SendAndWait(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("ListTools: %w", err)
	}
	proto := resp.GetListToolsResult()
	if proto == nil {
		return nil, fmt.Errorf("ListTools: unexpected response shape (missing list_tools_result)")
	}
	out := &ListToolsResult{
		NextCursor: proto.GetNextCursor(),
		Tools:      make([]ToolDefinition, 0, len(proto.GetTools())),
	}
	for _, t := range proto.GetTools() {
		out.Tools = append(out.Tools, ToolDefinition{
			Name:            t.GetName(),
			Description:     t.GetDescription(),
			InputSchema:     t.GetInputSchema(),
			ClientExecution: t.GetClientExecution(),
			Scopes:          append([]string(nil), t.GetScopes()...),
		})
	}
	return out, nil
}

// CallTool issues a CallToolMsg and returns the SDK-shape CallToolResult.
// Arguments are marshalled into the proto Struct envelope at the
// boundary; pass any JSON-serialisable map.
func (qc *QueryClient) CallTool(ctx context.Context, args CallToolArgs) (*CallToolResult, error) {
	if args.Name == "" {
		return nil, fmt.Errorf("CallTool: Name is required")
	}
	argsStruct, err := mapToProtoStruct(args.Arguments)
	if err != nil {
		return nil, fmt.Errorf("CallTool: marshal arguments: %w", err)
	}
	req := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_CallTool{
			CallTool: &memqlv1.CallToolMsg{
				Name:      args.Name,
				Arguments: argsStruct,
			},
		},
	}
	resp, err := qc.dispatcher.SendAndWait(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("CallTool(%s): %w", args.Name, err)
	}
	proto := resp.GetCallToolResult()
	if proto == nil {
		return nil, fmt.Errorf("CallTool(%s): unexpected response shape (missing call_tool_result)", args.Name)
	}
	return &CallToolResult{
		Content: contentFromProto(proto.GetContent()),
		IsError: proto.GetIsError(),
	}, nil
}

// -----------------------------------------------------------------
// Inbound dispatch (ClientToolCall -> handler -> ClientToolResult)
// -----------------------------------------------------------------

// clientToolHandlerRegistry holds the registered handler on the
// Dispatcher. One handler at a time -- callers who need per-tool
// fanout dispatch from the handler body (a switch on call.ToolName
// is the standard pattern).
//
// Versioning: each Register bumps `version`; the returned unregister
// captures the version at registration time. Calling that unregister
// only clears the slot when the current version still matches --
// so a stale unregister returned by a superseded Register is a
// no-op, and a re-Register cleanly replaces without the prior
// unregister being able to clobber the new handler.
type clientToolHandlerRegistry struct {
	mu      sync.RWMutex
	handler ClientToolHandler
	version uint64
}

// loadHandler returns the active handler under the read lock.
func (r *clientToolHandlerRegistry) loadHandler() ClientToolHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handler
}

// setNew installs a new handler and returns the version stamp the
// caller should use to bound its unregister.
func (r *clientToolHandlerRegistry) setNew(h ClientToolHandler) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version++
	r.handler = h
	return r.version
}

// clearIfVersion clears the handler only when the current version
// matches `v`. Returns true when the clear actually fired.
func (r *clientToolHandlerRegistry) clearIfVersion(v uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.version != v {
		return false
	}
	r.handler = nil
	return true
}

// RegisterClientToolHandler installs the handler the dispatcher
// invokes for every inbound ClientToolCall. Returns an unregister
// function that clears the handler when called.
//
// Re-register semantics: a second Register replaces the first; the
// first call's returned unregister becomes a NO-OP (it can no longer
// clear the slot it didn't install). The unregister is bound to the
// specific handler it registered.
//
// The handler runs on a goroutine dedicated to each incoming call,
// so a slow tool dispatch never blocks the dispatcher's read loop.
// Returning nil from the handler is interpreted as
// is_error=true + a generic error message so a malformed handler
// always unblocks the agent's parked tool call.
func (d *Dispatcher) RegisterClientToolHandler(handler ClientToolHandler) func() {
	if d.clientTools == nil {
		// Defensive: clientTools is initialised by NewDispatcher.
		// Treat a missing registry as a no-op so a caller can't
		// crash a stub dispatcher.
		return func() {}
	}
	version := d.clientTools.setNew(handler)
	once := sync.Once{}
	return func() {
		once.Do(func() {
			d.clientTools.clearIfVersion(version)
		})
	}
}

// dispatchClientToolCall is the dispatcher-side hook called by Run()
// when a ClientToolCall envelope arrives. It pulls the registered
// handler, spawns a goroutine to invoke it, and ships the returned
// ClientToolResult back over the stream correlated by call_id. When
// no handler is registered the dispatcher logs + drops the call --
// the server's agent loop will time out on its own per the
// per-call deadline carried in the envelope.
func (d *Dispatcher) dispatchClientToolCall(call *memqlv1.ClientToolCall) {
	if d.clientTools == nil {
		return
	}
	handler := d.clientTools.loadHandler()
	if handler == nil {
		if d.logger != nil {
			d.logger.Warn("ClientToolCall received but no handler registered",
				"call_id", call.GetCallId(),
				"tool_name", call.GetToolName(),
			)
		}
		return
	}
	go d.runClientToolHandler(handler, call)
}

func (d *Dispatcher) runClientToolHandler(handler ClientToolHandler, call *memqlv1.ClientToolCall) {
	sdkCall := &ClientToolCall{
		CallId:        call.GetCallId(),
		TurnId:        call.GetTurnId(),
		AgentId:       call.GetAgentId(),
		ToolName:      call.GetToolName(),
		ArgumentsJson: call.GetArgumentsJson(),
		TimeoutMs:     call.GetTimeoutMs(),
	}
	ctx := context.Background()
	res := handler(ctx, sdkCall)
	if res == nil {
		res = &ClientToolResult{
			IsError:      true,
			ErrorMessage: "client tool handler returned nil",
		}
	}
	// Ship the result back. We don't fail the dispatcher on a send
	// error -- the stream is best-effort post-handoff; a missed result
	// surfaces as a server-side timeout, which is the same path as a
	// silent client.
	reply := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ClientToolResult{
			ClientToolResult: &memqlv1.ClientToolResult{
				CallId:       sdkCall.CallId,
				Content:      contentToProto(res.Content),
				IsError:      res.IsError,
				ErrorMessage: res.ErrorMessage,
			},
		},
	}
	if _, err := d.Send(reply); err != nil {
		if d.logger != nil {
			d.logger.Warn("ClientToolResult send failed",
				"call_id", sdkCall.CallId,
				"tool_name", sdkCall.ToolName,
				"error", err,
			)
		}
	}
}

// -----------------------------------------------------------------
// proto <-> SDK content conversion
// -----------------------------------------------------------------

func contentFromProto(in []*memqlv1.ToolResultContent) []ToolResultContent {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolResultContent, 0, len(in))
	for _, c := range in {
		out = append(out, ToolResultContent{
			Type:     c.GetType(),
			Text:     c.GetText(),
			MimeType: c.GetMimeType(),
			Data:     c.GetData(),
			URI:      c.GetUri(),
		})
	}
	return out
}

func contentToProto(in []ToolResultContent) []*memqlv1.ToolResultContent {
	if len(in) == 0 {
		return nil
	}
	out := make([]*memqlv1.ToolResultContent, 0, len(in))
	for _, c := range in {
		out = append(out, &memqlv1.ToolResultContent{
			Type:     c.Type,
			Text:     c.Text,
			MimeType: c.MimeType,
			Data:     c.Data,
			Uri:      c.URI,
		})
	}
	return out
}

// mapToProtoStruct marshals a JSON-shaped map into a protobuf Struct.
// Goes through encoding/json so any value that round-trips through
// JSON (the SPA's expectation) is preserved exactly.
func mapToProtoStruct(m map[string]any) (*structpb.Struct, error) {
	if len(m) == 0 {
		return &structpb.Struct{}, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var s structpb.Struct
	if err := s.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return &s, nil
}
