package client

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// tools.go -- the MCP-shaped tool RPC surface. memql#174.
//
// Hand-rolled (parallels concept_browser.go) because the MCP wire
// messages -- ListToolsMsg / CallToolMsg -- are not DSL queries /
// mutations. They're the gRPC envelope for the tool-call portion of
// the agent loop, so sdk-gen has no input to generate from. Every
// OTHER tool-shaped call should reach for a typed generated method.
//
// Two surfaces ship here:
//
//   1. ListTools / CallTool -- the outbound RPC pair the consumer
//      uses to enumerate and invoke server-side tools.
//   2. SDK-owned types (ToolDefinition / CallToolResult / etc.)
//      mirror the proto fields per the no-memqlv1-in-consumers
//      rule (sdk/go/CLAUDE.md rule 2).
//
// There is no inbound half. Client-executed tools ran in the connected
// browser and reached it over the client-tool relay, which went with
// the conversational product (epic memql#4988).

// -----------------------------------------------------------------
// SDK-owned types -- wrappers over the proto so consumers never see
// memqlv1.* in their imports.
// -----------------------------------------------------------------

// ToolDefinition is one entry from a ListTools response. Mirrors
// memqlv1.ToolDefinition.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema string // JSON Schema describing the tool's input args.
	Scopes      []string
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
			Name:        t.GetName(),
			Description: t.GetDescription(),
			InputSchema: t.GetInputSchema(),
			Scopes:      append([]string(nil), t.GetScopes()...),
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
