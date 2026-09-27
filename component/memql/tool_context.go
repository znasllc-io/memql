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
type mcpHumanCallerKey struct{}

// WithActingAgentRole returns a child context that carries the acting
// agent's guardrail role (its v1:agents:agent.role: "assistant" or
// "specialist"). It is what makes a call an AGENT's (ToolCaller): ExecuteTool
// reads it to enforce @requiresAgentRole and the deprecated @allowedRoles on
// the in-engine ExecuteToolByName path as well as on a CallToolMsg with wire
// metadata.
func WithActingAgentRole(ctx context.Context, role string) context.Context {
	role = strings.TrimSpace(role)
	if role == "" {
		return ctx
	}
	return context.WithValue(ctx, actingAgentRoleKey{}, role)
}

// ActingAgentRoleFromContext returns the role previously attached via
// WithActingAgentRole, or "" if none was set. Absence means there is no
// acting agent: the call is a person's over MCP (WithMCPHumanCaller) or no
// tool caller at all.
func ActingAgentRoleFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(actingAgentRoleKey{}).(string); ok {
		return v
	}
	return ""
}

// ToolCallerKind is WHO is calling a tool, which decides which of its gates
// can admit the call (memql#5438). There are exactly two kinds of tool
// caller, and the context says which -- a tool call is never inferred from a
// role string's spelling:
//
//   - ToolCallerAgent: an agent's tool loop, stamped with the acting agent's
//     role by WithActingAgentRole. @requiresAgentRole is judged against that
//     role; @requiresRank against the person the agent acts for.
//   - ToolCallerMCPHuman: a person calling over MCP, stamped by
//     WithMCPHumanCaller. A person is not an agent kind, so @requiresAgentRole
//     refuses them; @requiresRank is judged against their own rank.
//
// Before the two kinds existed the MCP surface stamped the person's cluster
// role as if it were an agent role, which is how one @allowedRoles list came
// to gate agent kinds on some tools and human roles on others.
type ToolCallerKind string

const (
	// ToolCallerNone is a context carrying neither stamp: not a tool caller.
	ToolCallerNone ToolCallerKind = ""
	// ToolCallerAgent is an agent's tool loop.
	ToolCallerAgent ToolCallerKind = "agent"
	// ToolCallerMCPHuman is an authenticated person calling over MCP.
	ToolCallerMCPHuman ToolCallerKind = "mcp_human"
)

// ToolCaller is the caller a tool gate judges.
type ToolCaller struct {
	// Kind is who is calling.
	Kind ToolCallerKind
	// AgentRole is the acting agent's role; set only when Kind is
	// ToolCallerAgent.
	AgentRole string
	// LegacyRole is the ONE string the deprecated @allowedRoles compares, kept
	// exactly as it was while the form's window runs: the acting agent's role
	// for an agent, the person's cluster role for a person over MCP, and ""
	// (which @allowedRoles reads as "specialist") for anything else.
	LegacyRole string
}

// WithMCPHumanCaller returns a child context marking the call as an
// authenticated person's over MCP (memql#5438). role is the person's cluster
// role -- recorded ONLY for the deprecated @allowedRoles, which compared it;
// @requiresRank reads the person's rank from the auth context, never from
// this string.
//
// It does not stamp an acting agent, and that is the point: a person over MCP
// used to be stamped as an agent whose role was their cluster role.
func WithMCPHumanCaller(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, mcpHumanCallerKey{}, strings.TrimSpace(role))
}

// ToolCallerFromContext reports who ctx says is calling a tool. An acting
// agent wins over an MCP mark: an agent loop started inside an MCP call stamps
// its own agent, and it is that agent calling.
func ToolCallerFromContext(ctx context.Context) ToolCaller {
	if ctx == nil {
		return ToolCaller{}
	}
	if role := ActingAgentRoleFromContext(ctx); role != "" {
		return ToolCaller{Kind: ToolCallerAgent, AgentRole: role, LegacyRole: role}
	}
	if role, ok := ctx.Value(mcpHumanCallerKey{}).(string); ok {
		return ToolCaller{Kind: ToolCallerMCPHuman, LegacyRole: role}
	}
	return ToolCaller{}
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
