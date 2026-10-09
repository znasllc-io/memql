//go:build agent

package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/agent"
)

// computerUseStatusFn builds the agent.Replier hook that resolves
// computer_use availability for an agent at prompt-build time.
//
// The hook returns three values:
//
//	status -- worker reachability tag:
//	  "connected"    eligible worker online in the shared fleet.
//	  "disconnected" registration row exists, no online worker.
//	  "unconfigured" no registration rows.
//	  ""             agent owner unresolved; suppress prompt context.
//
//	detail -- eligible worker name and headless-only limitation, when connected;
//	          empty otherwise.
//
//	scope  -- the agent's CURRENT standing computer_use scope from
//	          v1:agents:agentAuthorization (observe / interact /
//	          full), or "" if no standing grant exists. The agent
//	          reads this each turn so it can dispatch worker tools
//	          directly when its standing scope already covers the
//	          action -- without this, the agent had no signal of its
//	          own scope and called requestComputerUseScope before
//	          every elevated action regardless of prior approvals
//	          (the user-visible "I keep clicking Allow but Sofia
//	          keeps asking for full" loop).
func (a *App) computerUseStatusFn() agent.ComputerUseStatusFn {
	if a.engine == nil {
		return nil
	}
	return func(ctx context.Context, agentId string) (status, detail, scope string) {
		owner, err := resolveAgentOwner(ctx, a.engine, agentId)
		if err != nil || owner == "" {
			return "", "", ""
		}
		scope = standingComputerUseScope(ctx, a.engine, agentId, owner, a.Logger)
		integration := a.lookupWorkerIntegration()
		if integration == nil {
			return "", "", scope
		}
		availability, err := integration.Dispatcher().Router().Availability(auth.ContextWithUserActor(ctx, owner), owner)
		if err != nil {
			if a.Logger != nil {
				a.Logger.Warn("computer_use availability could not be read", "error", err)
			}
			return "", "", scope
		}
		detail = availability.Detail
		if availability.Online && !availability.ComputerUseOnline {
			detail += " (headless operations only; no eligible desktop control)"
		}
		return availability.Status, detail, scope
	}
}

// standingComputerUseScope reads the agent's current standing
// computer_use scope from agentAuthorizationsForSelf. Tolerates
// both bare-slug and canonical-form agentIds on the stored row
// because v1:agents:agentAuthorization has no @relationship on
// agentId yet (auto-canon doesn't fire). Returns "" on lookup
// errors; the caller treats empty as "no standing grant" which
// prompts the agent to request elevation, the safe default.
func standingComputerUseScope(ctx context.Context, engine *memqlengine.MemQLEngine, agentId, ownerUserId string, logger interface {
	Debug(msg string, args ...any)
}) string {
	if engine == nil || strings.TrimSpace(ownerUserId) == "" {
		return ""
	}
	// #3177: `agentAuthorizationsForSelf` is self-scoped on actor.userId and
	// takes no userId argument, because v1:agents:agentAuthorization declares
	// `@rowAuthz(owner="userId")` and a caller-supplied-id read of a declared
	// concept is what #3172's land gate refuses.
	//
	// ownerUserId here is resolved from the AGENT row (resolveAgentOwner), not
	// from the caller, and an agent answers in spaces its owner need not be the
	// caller in -- so the owner's actor envelope is supplied for this ONE
	// Execute, built inline as the argument in the memql#3072 shape epic
	// decision C blesses.
	res, err := engine.Execute(
		auth.ContextWithUserActor(ctx, ownerUserId),
		`query agentAuthorizationsForSelf()`)
	if err != nil {
		if logger != nil {
			logger.Debug("computer_use scope: agentAuthorizationsForSelf failed",
				"owner_user_id", ownerUserId,
				"error", err,
			)
		}
		return ""
	}
	if res == nil {
		return ""
	}
	targetSuffix := agentId
	if i := strings.LastIndex(agentId, ":"); i >= 0 {
		targetSuffix = agentId[i+1:]
	}
	for _, row := range outputPayloadRows(res.OutputPayload()) {
		if row == nil {
			continue
		}
		rowAgent, _ := row["agentId"].(string)
		if rowAgent == "" {
			continue
		}
		rowSuffix := rowAgent
		if i := strings.LastIndex(rowAgent, ":"); i >= 0 {
			rowSuffix = rowAgent[i+1:]
		}
		if rowAgent != agentId && rowSuffix != targetSuffix {
			continue
		}
		if scope, ok := row["computerUseScope"].(string); ok {
			scope = strings.TrimSpace(scope)
			if scope == "observe" || scope == "interact" || scope == "full" {
				return scope
			}
		}
	}
	return ""
}

// resolveAgentOwner returns the owning user id for the supplied
// agent. Prefers payload.ownerUserId (explicitly stamped on insert)
// and falls back to the engine-auto-stamped createdBy column. This
// fallback matters for agents created BEFORE the ownerUserId
// convention landed AND for user-created specialists where the
// auto-stamped createdBy IS the user (because the actor IS the
// user on user-driven create paths).
//
// agentOwner is a shape() query, so results land in
// r.OutputPayload (the Data axis). The older code read Bundle.Nodes
// and silently returned empty -- that was the "agent owner
// unresolved" log line that killed Computer Use prompt awareness.
//
// Why ownerUserId exists alongside createdBy: when the GA is
// auto-seeded by provisionAssistantOnUserCreate, the
// automation runs with `system:automation:<name>` as the actor, so
// createdBy gets stamped with that system actor and the lookup
// against the user-keyed worker registry would never match. The
// automation explicitly stamps ownerUserId to the user's id so
// owner-keyed lookups work regardless of who actually wrote the row.
func resolveAgentOwner(ctx context.Context, engine *memqlengine.MemQLEngine, agentId string) (string, error) {
	if engine == nil || strings.TrimSpace(agentId) == "" {
		return "", nil
	}
	q := fmt.Sprintf(`query agentOwner(agentId:%s)`, langparser.QuoteString(agentId))
	res, err := engine.Execute(ctx, q)
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", nil
	}
	// Shape-query path: prefer ownerUserId, fall back to createdBy.
	if v := firstStringField(res.OutputPayload(), "ownerUserId"); v != "" {
		return v, nil
	}
	if v := firstStringField(res.OutputPayload(), "createdBy"); v != "" {
		return v, nil
	}
	// Bundle path (legacy / non-shape callers): scan Nodes for the
	// same fields in the same priority order.
	if res.Bundle != nil {
		for _, n := range res.Bundle.Nodes {
			if n == nil || n.Payload == nil {
				continue
			}
			fields := n.Payload.GetFields()
			if v, ok := fields["ownerUserId"]; ok && v != nil {
				if s := strings.TrimSpace(v.GetStringValue()); s != "" {
					return s, nil
				}
			}
			if v, ok := fields["createdBy"]; ok && v != nil {
				if s := strings.TrimSpace(v.GetStringValue()); s != "" {
					return s, nil
				}
			}
		}
	}
	return "", nil
}

// firstStringField scans an OutputPayload (typed as `any` to handle
// the multiple shapes shape() can produce -- []any of map[string]any,
// []map[string]any, single map[string]any) and returns the first
// non-empty string value for the given field key, or "" if none found.
func firstStringField(payload any, field string) string {
	for _, row := range outputPayloadRows(payload) {
		if row == nil {
			continue
		}
		if s, ok := row[field].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// outputPayloadRows normalises a shape() query's OutputPayload into
// a []map[string]any slice. shape() can land as a slice of maps, a
// slice of `any` whose elements are maps, or a bare map (single-row
// projections). Returns nil when the payload doesn't carry rows.
func outputPayloadRows(payload any) []map[string]any {
	if payload == nil {
		return nil
	}
	switch v := payload.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}
