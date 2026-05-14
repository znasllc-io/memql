//go:build agent

package app

import (
	"context"
	"fmt"
	"strings"

	memqlengine "github.com/visionarys-io/memql/component/memql"
	"github.com/visionarys-io/memql/component/worker"
	"github.com/visionarys-io/memql/integrations/agent"
)

// computerUseStatusFn builds the agent.Replier hook that resolves
// computer_use availability for an agent at prompt-build time.
//
// The hook returns three values:
//
//	status -- worker reachability tag:
//	  "connected"    cockpit online for the owner. Detail is hostname.
//	  "disconnected" registration row exists, no online worker.
//	  "unconfigured" no registration rows.
//	  ""             agent owner unresolved; suppress prompt context.
//
//	detail -- connected worker's hostname when status="connected";
//	          empty otherwise.
//
//	scope  -- the agent's CURRENT standing computer_use scope from
//	          v1:copresent:agentAuthorization (observe / interact /
//	          full), or "" if no standing grant exists. The agent
//	          reads this each turn so it can dispatch worker tools
//	          directly when its standing scope already covers the
//	          action -- without this, the agent had no signal of its
//	          own scope and called requestComputerUseScope before
//	          every elevated action regardless of prior approvals
//	          (the user-visible "I keep clicking Allow but Sofia
//	          keeps asking for full" loop).
func (a *App) computerUseStatusFn() agent.ComputerUseStatusFn {
	if a.workerService == nil {
		return nil
	}
	svc, ok := a.workerService.(*worker.Service)
	if !ok || svc == nil {
		return nil
	}
	registry := svc.Registry()
	engine := a.engine
	logger := a.Logger

	return func(ctx context.Context, agentId string) (status, detail, scope string) {
		if engine == nil || registry == nil {
			return "", "", ""
		}
		ownerUserId, err := resolveAgentOwner(ctx, engine, agentId)
		if err != nil || ownerUserId == "" {
			if logger != nil {
				logger.Debug("computer_use status: agent owner unresolved",
					"agent_id", agentId,
					"error", err,
				)
			}
			return "", "", ""
		}

		// Resolve the standing scope independently of worker
		// reachability -- a connected cockpit + empty scope means
		// "the user paired a worker but hasn't approved any
		// computer_use action yet"; an offline cockpit + full
		// scope means "the user previously approved full but the
		// cockpit isn't running this turn." Both are real states
		// the prompt branches on.
		scope = standingComputerUseScope(ctx, engine, agentId, ownerUserId, logger)

		// Online check: in-memory registry knows currently-streaming
		// cockpits.
		if workers := registry.WorkersForUser(ownerUserId); len(workers) > 0 {
			// Pick the first online worker for the detail line --
			// stable enough for the prompt's natural-language echo.
			w := workers[0]
			name := strings.TrimSpace(w.Name)
			if name == "" {
				name = ownerUserId
			}
			return "connected", name, scope
		}

		// Configured-but-offline check: query the persistent
		// v1:worker:registration rows. Any non-revoked row means
		// the user has paired a computer at some point.
		hasConfigured, err := userHasConfiguredWorker(ctx, engine, ownerUserId)
		if err != nil {
			if logger != nil {
				logger.Debug("computer_use status: registration lookup failed",
					"owner_user_id", ownerUserId,
					"error", err,
				)
			}
			// On error, default to "unconfigured" -- it's the
			// safer guidance than implying the user just needs to
			// start the cockpit.
			return "unconfigured", "", scope
		}
		if hasConfigured {
			return "disconnected", "", scope
		}
		return "unconfigured", "", scope
	}
}

// standingComputerUseScope reads the agent's current standing
// computer_use scope from queryAgentAuthorizationsForUser. Tolerates
// both bare-slug and canonical-form agentIds on the stored row
// because v1:copresent:agentAuthorization has no @relationship on
// agentId yet (auto-canon doesn't fire). Returns "" on lookup
// errors; the caller treats empty as "no standing grant" which
// prompts the agent to request elevation, the safe default.
func standingComputerUseScope(ctx context.Context, engine *memqlengine.MemQLEngine, agentId, ownerUserId string, logger interface {
	Debug(msg string, args ...any)
}) string {
	if engine == nil || strings.TrimSpace(ownerUserId) == "" {
		return ""
	}
	q := fmt.Sprintf(`queryAgentAuthorizationsForUser({userId:%q})`, ownerUserId)
	res, err := engine.Execute(ctx, q)
	if err != nil {
		if logger != nil {
			logger.Debug("computer_use scope: queryAgentAuthorizationsForUser failed",
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
// queryAgentOwner is a shape() query, so results land in
// r.OutputPayload (the Data axis). The older code read Bundle.Nodes
// and silently returned empty -- that was the "agent owner
// unresolved" log line that killed Computer Use prompt awareness.
//
// Why ownerUserId exists alongside createdBy: when the GA is
// auto-seeded by provisionGeneralAssistantOnUserCreate, the
// automation runs with `system:automation:<name>` as the actor, so
// createdBy gets stamped with that system actor and the lookup
// against the user-keyed worker registry would never match. The
// automation explicitly stamps ownerUserId to the user's id so
// owner-keyed lookups work regardless of who actually wrote the row.
func resolveAgentOwner(ctx context.Context, engine *memqlengine.MemQLEngine, agentId string) (string, error) {
	if engine == nil || strings.TrimSpace(agentId) == "" {
		return "", nil
	}
	q := fmt.Sprintf(`queryAgentOwner({agentId:%q})`, agentId)
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

// userHasConfiguredWorker reports whether the user has any non-
// revoked v1:worker:registration row. Distinguishes "the user has
// paired a computer but it's offline" from "the user has never
// paired anything" so the agent's prompt-context message can
// branch on it.
//
// queryWorkersForUser is a shape() query -- same Data-vs-Bundle
// caveat as resolveAgentOwner above. Read OutputPayload first.
func userHasConfiguredWorker(ctx context.Context, engine *memqlengine.MemQLEngine, ownerUserId string) (bool, error) {
	if engine == nil || strings.TrimSpace(ownerUserId) == "" {
		return false, nil
	}
	q := fmt.Sprintf(`queryWorkersForUser({ownerUserId:%q})`, ownerUserId)
	res, err := engine.Execute(ctx, q)
	if err != nil {
		return false, err
	}
	if res == nil {
		return false, nil
	}
	// Shape-query path: walk the Data array, skip rows with non-empty
	// revokedAt.
	if rows := outputPayloadRows(res.OutputPayload()); rows != nil {
		for _, row := range rows {
			if row == nil {
				continue
			}
			if rev, ok := row["revokedAt"].(string); ok && strings.TrimSpace(rev) != "" {
				continue
			}
			return true, nil
		}
		return false, nil
	}
	// Bundle path (legacy fallback).
	if res.Bundle != nil {
		for _, n := range res.Bundle.Nodes {
			if n == nil || n.Payload == nil {
				continue
			}
			fields := n.Payload.GetFields()
			if fields == nil {
				continue
			}
			if v, ok := fields["revokedAt"]; ok && v != nil {
				if rev := strings.TrimSpace(v.GetStringValue()); rev != "" {
					continue
				}
			}
			return true, nil
		}
	}
	return false, nil
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
