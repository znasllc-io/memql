package memql

import (
	"context"
	"strings"
)

// Per-request context carried into tool dispatch.
//
// These used to sit in client_tool_invoker.go beside the ClientToolInvoker
// seam. That seam was the browser client-tool relay and went with the
// conversational product (epic memql#4988); everything here is generic
// tool-execution context and had nothing to do with it.

type actingAgentRoleKey struct{}
type actingAgentIdKey struct{}

// WithActingAgentRole returns a child context that carries the acting
// agent's guardrail role (e.g. "assistant" or "specialist"). ExecuteTool
// reads it to enforce Tool.AllowedRoles on the in-engine
// ExecuteToolByName path as well as on a CallToolMsg with wire metadata.
func WithActingAgentRole(ctx context.Context, role string) context.Context {
	role = strings.TrimSpace(role)
	if role == "" {
		return ctx
	}
	return context.WithValue(ctx, actingAgentRoleKey{}, role)
}

// ActingAgentRoleFromContext returns the role previously attached via
// WithActingAgentRole, or "" if none was set. Absence means there is no
// acting agent, which ExecuteTool treats as "not a tool caller".
func ActingAgentRoleFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(actingAgentRoleKey{}).(string); ok {
		return v
	}
	return ""
}

// WithActingAgentId returns a child context carrying the acting agent's id
// (e.g. "v1:agents:agent:<short-id>"), so a tool invocation can be
// attributed to the agent that drove it.
func WithActingAgentId(ctx context.Context, agentId string) context.Context {
	agentId = strings.TrimSpace(agentId)
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
// where the silent-drop data loss was observed, and it mirrors the
// unknown-tool rejection added for the MCP server in memql#1602.
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

// mcpToolExecutionKey signals that a tool call is executing on behalf of an
// MCP connector session (memql#1684). When set, applyToolDefaults preserves
// caller-supplied values for @autoInjected fields that have no server
// default. In the normal agent execution path the server default ALWAYS wins
// and any LLM-supplied autoInjected value is dropped, because the LLM must
// not be able to forge ownerUserId / agentId. Over MCP the caller IS the
// authenticated user rather than an LLM, so a caller-supplied value is a
// legitimate input that must be honoured.
type mcpToolExecutionKey struct{}

// WithMCPToolExecution stamps the context to indicate this tool dispatch
// originated from the MCP connector. Stamped by callMCPTool in
// component/mcp/tool_surface.go.
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
