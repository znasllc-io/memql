package memql

// tool_gate.go -- who may be offered a tool, and who may call one (memql#5438).
//
// # Two axes, two gates
//
// A tool can be reached by two kinds of caller (ToolCaller, tool_context.go):
// an AGENT's tool loop, and a PERSON calling over MCP. Each kind carries a
// different question, and a tool answers each with its own annotation:
//
//   - @requiresAgentRole("assistant", ...) asks WHICH AGENT is calling: the
//     acting agent's v1:agents:agent.role. A person over MCP is not an agent
//     kind, so a tool carrying it is neither listed nor callable for them.
//   - @requiresRank("<role>") asks how senior the PERSON the call is for is:
//     the authenticated user over MCP, or the user an agent acts for. It is
//     resolved through the same ladder (rankLadder) as a query's floor, so a
//     custom role is admitted by its rank.
//
// Both replace @allowedRoles, which compared ONE role string whose meaning
// depended on the path a call arrived by -- the agent's role in an agent loop,
// the person's cluster role over MCP -- so one list gated agent kinds on some
// tools and human roles on others. It is DEPRECATED (component/language/
// deprecation, rule deprecated_allowed_roles) and keeps exactly that behaviour
// while its window runs: callerRefusal applies it to ToolCaller.LegacyRole.
//
// # Listed and callable are one decision, asked twice
//
// ToolListed and ToolCallRefusal share every gate, so a caller is never shown
// a tool it cannot call nor refused one it was shown. The one difference is
// the caller-kind rule: only a CALL must come from an agent or an
// authenticated person over MCP -- a listing is a description, and the gRPC
// ListTools surface has always described the tool set to callers that stamp
// neither.
//
// # Enforced on every path
//
// ExecuteTool (tool_execution.go) asks toolCallRefusal before a handler runs,
// and every surface that calls a tool reaches it: an agent's loop, the gRPC
// CallToolMsg handler (which asks first to answer cleanly), the MCP tools/call
// dispatcher and the engine's own tool loop. The listing surfaces -- gRPC
// ListToolsMsg and MCP tools/list -- ask ToolListed.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// ToolCallerAdmits reports whether c passes t's caller gates that need no
// engine: @requiresAgentRole, and the deprecated @allowedRoles. The rank floor
// needs the engine's role ladder and is MemQLEngine.ToolListed's.
func ToolCallerAdmits(t *Tool, c ToolCaller) bool { return t.callerRefusal(c) == nil }

// callerRefusal is the engine-free half of t's gate, or nil when c passes it.
func (t *Tool) callerRefusal(c ToolCaller) error {
	if t == nil {
		return fmt.Errorf("tool is nil")
	}
	// The deprecated @allowedRoles, kept exactly as it behaved: one string,
	// "" read as "specialist" (the Agent.role default). One edge is closed
	// rather than kept: a PERSON with no role was never admitted by this
	// gate -- the MCP surface stamped nothing for an empty role and the call
	// was refused outright -- so "" must not become "specialist" for them.
	if len(t.AllowedRoles) > 0 {
		if c.Kind == ToolCallerMCPHuman && strings.TrimSpace(c.LegacyRole) == "" {
			return fmt.Errorf("tool %q is not allowed for caller role %q", t.Name, c.LegacyRole)
		}
		if !t.IsAllowedForRole(c.LegacyRole) {
			return fmt.Errorf("tool %q is not allowed for caller role %q", t.Name, c.LegacyRole)
		}
	}
	if len(t.RequiresAgentRole) > 0 {
		if c.Kind != ToolCallerAgent {
			return fmt.Errorf("tool %q is not allowed for this caller: @requiresAgentRole admits only an agent whose role is %s, and %s",
				t.Name, joinOr(t.RequiresAgentRole), notAnAgentPhrase(c))
		}
		if !containsExact(t.RequiresAgentRole, c.AgentRole) {
			return fmt.Errorf("tool %q is not allowed for caller role %q: @requiresAgentRole admits only an agent whose role is %s",
				t.Name, c.AgentRole, joinOr(t.RequiresAgentRole))
		}
	}
	return nil
}

// notAnAgentPhrase says why a caller that is not an agent is refused by an
// agent-kind gate.
func notAnAgentPhrase(c ToolCaller) string {
	if c.Kind == ToolCallerMCPHuman {
		return "a person calling over MCP is not an agent"
	}
	return "this call carries no acting agent"
}

// ToolListed reports whether the caller on ctx may be offered t: its
// agent-kind gate, the deprecated @allowedRoles, and its rank floor against
// the person the call would be for. It is ToolCallRefusal without the
// caller-kind rule (see the file comment).
func (e *MemQLEngine) ToolListed(ctx context.Context, t *Tool) bool {
	if t == nil {
		return false
	}
	if t.callerRefusal(ToolCallerFromContext(ctx)) != nil {
		return false
	}
	return e.toolRankRefusal(ctx, t) == nil
}

// ToolCallRefusal is the whole call-time decision for t, or nil when the
// caller on ctx may call it. ExecuteTool asks it before any handler runs; a
// surface that wants to answer a refusal without dispatching asks it too.
func (e *MemQLEngine) ToolCallRefusal(ctx context.Context, t *Tool) error {
	if t == nil {
		return fmt.Errorf("tool is nil")
	}
	caller := ToolCallerFromContext(ctx)
	switch caller.Kind {
	case ToolCallerAgent:
	case ToolCallerMCPHuman:
		// "An AUTHENTICATED person over MCP": the HTTP head verifies a bearer
		// before a session exists, and a stdio session is a person only once
		// MEMQL_MCP_USER names one. A session with no identity has nobody for
		// @requiresRank to judge and nobody for the handler's writes to
		// belong to.
		if ac, ok := auth.AccessFromContext(ctx); !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" || ac.IsAnonymousActor() {
			return fmt.Errorf("tool %q: tools are agent-only outside an authenticated MCP session, and this MCP session has no authenticated identity (a stdio session names one with MEMQL_MCP_USER)", t.Name)
		}
	default:
		// The rule used to be "agent-only"; it is now "an agent, or an
		// authenticated person over MCP". "agent-only" stays in the text
		// because it is what ClassifyToolError reads as a permission refusal.
		return fmt.Errorf("tool %q: tools are agent-only -- called by an agent or by an authenticated person over MCP, and this call carries neither an acting agent nor an MCP caller. Use queries, mutations, or integration capabilities for non-agent paths", t.Name)
	}
	if err := t.callerRefusal(caller); err != nil {
		return err
	}
	return e.toolRankRefusal(ctx, t)
}

// toolRankRefusal enforces t's @requiresRank against the person the call is
// for, through refuseBelowRequiredRank -- the enforcement every query,
// mutation and logic floor has, so a tool's floor fails closed in the same
// two directions (an unresolvable caller rank, an unresolvable floor) and an
// internal-origin call passes it for the same reason.
func (e *MemQLEngine) toolRankRefusal(ctx context.Context, t *Tool) error {
	floor := strings.TrimSpace(t.RequiresRank)
	if floor == "" {
		return nil
	}
	if err := e.refuseBelowRequiredRank(ctx, &Function{RequiresRank: floor}, t.Name); err != nil {
		// "is not allowed for" is what ClassifyToolError reads as a
		// permission refusal, which is what the model must be told.
		return fmt.Errorf("tool %q is not allowed for this caller: %w", t.Name, err)
	}
	return nil
}

// joinOr renders a value list for a refusal: "assistant" or
// "assistant or specialist", each quoted.
func joinOr(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	switch len(quoted) {
	case 0:
		return ""
	case 1:
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}

// containsExact reports whether list holds v exactly. Agent roles are the
// lowercase enum values the concept declares; a role that differs from them
// in case is not one of them.
func containsExact(list []string, v string) bool {
	v = strings.TrimSpace(v)
	for _, item := range list {
		if strings.TrimSpace(item) == v {
			return true
		}
	}
	return false
}
