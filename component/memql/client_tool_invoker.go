package memql

import "context"

// ClientToolInvoker dispatches a tool whose implementation lives on the
// connected client rather than in the backend. The BFF's streamSession
// is the canonical implementation: it emits a ClientToolCall envelope
// on its wire and parks on a per-call channel until the client responds
// with ClientToolResult.
//
// The engine calls this from ExecuteTool when a resolved tool has
// ClientExecution=true. The invoker is carried on the request context
// (not wired statically on the engine) because a single engine can
// serve many concurrent streams; the routing target depends on who
// originated the request, which is only known to the handler that
// pulled the request off a specific stream.
type ClientToolInvoker interface {
	// InvokeClientTool runs the named tool against the connected client
	// and returns the result content.
	//
	// The argsJSON parameter is the JSON-encoded arguments object,
	// which matches the ClientToolCall.arguments_json wire field.
	//
	// agentRole, if non-empty, is the guardrail role of the acting
	// agent. The invoker uses this to enforce AllowedRoles the same way
	// the direct CallToolMsg path does, so an agent running tools via
	// the in-engine path doesn't bypass the role gate.
	InvokeClientTool(
		ctx context.Context,
		toolName string,
		argsJSON string,
		agentRole string,
	) (*ToolCallResult, error)
}

type clientToolInvokerKey struct{}
type actingAgentRoleKey struct{}
type actingAgentIdKey struct{}

// WithClientToolInvoker returns a child context that carries the invoker
// so downstream engine.ExecuteTool calls can find it.
func WithClientToolInvoker(ctx context.Context, invoker ClientToolInvoker) context.Context {
	if invoker == nil {
		return ctx
	}
	return context.WithValue(ctx, clientToolInvokerKey{}, invoker)
}

// ClientToolInvokerFromContext returns the invoker previously attached
// via WithClientToolInvoker, or nil if none was set.
func ClientToolInvokerFromContext(ctx context.Context) ClientToolInvoker {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(clientToolInvokerKey{}).(ClientToolInvoker); ok {
		return v
	}
	return nil
}

// WithActingAgentRole returns a child context that carries the acting
// agent's guardrail role (e.g. "assistant" or "specialist").
// Used by ExecuteTool so client-execution tools honour Tool.AllowedRoles
// even when they come in through the in-engine ExecuteToolByName path
// rather than a CallToolMsg with metadata on the wire.
func WithActingAgentRole(ctx context.Context, role string) context.Context {
	role = trimString(role)
	if role == "" {
		return ctx
	}
	return context.WithValue(ctx, actingAgentRoleKey{}, role)
}

// ActingAgentRoleFromContext returns the role previously attached via
// WithActingAgentRole, or "" if none was set.
func ActingAgentRoleFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(actingAgentRoleKey{}).(string); ok {
		return v
	}
	return ""
}

// WithActingAgentId returns a child context that carries the acting
// agent's id (e.g. "default:v1:agents:agent:<short-id>"). Used by
// the gRPC stream session to stamp ClientToolCall.AgentId so the
// frontend can attribute UI takeover sessions to the actual driving
// agent (e.g. show "Briar" in the CoPresent Control Widget transcript
// instead of a generic "Agent" label).
func WithActingAgentId(ctx context.Context, agentId string) context.Context {
	agentId = trimString(agentId)
	if agentId == "" {
		return ctx
	}
	return context.WithValue(ctx, actingAgentIdKey{}, agentId)
}

// ActingAgentIdFromContext returns the id previously attached via
// WithActingAgentId, or "" if none was set.
func ActingAgentIdFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(actingAgentIdKey{}).(string); ok {
		return v
	}
	return ""
}

type strictUnknownArgsKey struct{}

// WithStrictUnknownArgs returns a child context that opts the top-level
// mutation-function-call path into strict argument validation: a caller-
// supplied argument that the mutation's args { ... } block does not declare
// is rejected with an error instead of being silently dropped (memql#1633).
//
// Scoped to the MCP boundary (run_mutation / first-class @mcp mutation calls)
// rather than the whole engine: the internal automation / gRPC / inlined
// query-expansion paths stay lenient so this can't turn a benign extra arg on
// some existing live caller into a hard runtime failure. The MCP surface is
// where the silent-drop data loss was observed (Run-2 QA staging) and mirrors
// the unknown-tool rejection added for the MCP server in memql#1602.
func WithStrictUnknownArgs(ctx context.Context) context.Context {
	return context.WithValue(ctx, strictUnknownArgsKey{}, true)
}

// strictUnknownArgs reports whether WithStrictUnknownArgs was set on ctx.
func strictUnknownArgs(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(strictUnknownArgsKey{}).(bool)
	return v
}

// mcpToolExecutionKey signals that a tool call is executing on behalf of
// an MCP connector session (memql#1684). When set, applyToolDefaults
// preserves caller-supplied values for @autoInjected fields that have no
// server default (e.g. spaceId on recentChat, where no agent runtime is
// present to stamp the space). In the normal agent execution path, the
// server default ALWAYS wins and any LLM-supplied autoInjected value is
// dropped (security: LLM cannot forge ownerUserId / spaceId / agentId).
// Over MCP the caller IS the authenticated user, not an LLM, so a
// caller-supplied spaceId is a legitimate input that must be honoured.
type mcpToolExecutionKey struct{}

// WithMCPToolExecution stamps the context to indicate this tool dispatch
// originated from the MCP connector. Stamped by callMCPTool in
// component/mcp/tool_surface.go; checked by applyToolDefaults to
// preserve caller-supplied @autoInjected values when no server default
// is available (memql#1684).
func WithMCPToolExecution(ctx context.Context) context.Context {
	return context.WithValue(ctx, mcpToolExecutionKey{}, true)
}

// mcpToolExecution reports whether the context was stamped by the MCP
// connector tool-dispatch path.
func mcpToolExecution(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(mcpToolExecutionKey{}).(bool)
	return v
}

// providerOverrideKey carries an optional LLM provider name that
// si_tool_loop's InvokeSIChatWithFilteredTools should pass as the
// SIInvocation.ProviderOverride. Lets callers (e.g. delegate_takeover)
// route specific turns to a registered provider other than the
// template default -- for instance a stronger reasoning tier without
// changing the global default.
type providerOverrideKey struct{}

// WithProviderOverride returns a child context carrying the provider
// override name. Empty or whitespace-only names are ignored.
func WithProviderOverride(ctx context.Context, provider string) context.Context {
	provider = trimString(provider)
	if provider == "" {
		return ctx
	}
	return context.WithValue(ctx, providerOverrideKey{}, provider)
}

// ProviderOverrideFromContext returns the provider override previously
// attached via WithProviderOverride, or "" if none was set.
func ProviderOverrideFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(providerOverrideKey{}).(string); ok {
		return v
	}
	return ""
}

// trimString is a thin wrapper so this file doesn't pull "strings" just
// for TrimSpace. Callers pass short identifiers; this keeps the helper
// dependency-free.
func trimString(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
