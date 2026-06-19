package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/harness"
	"github.com/znasllc-io/memql/component/harness/actionreplay"
	"github.com/znasllc-io/memql/component/harness/actiontrace"
	"github.com/znasllc-io/memql/component/harness/surfaceresolver"
)

// actionReplayEnabled reports whether action-library literal replay (#1736) is
// turned on. It is OFF by default: with the flag unset the harness behaves
// exactly as before (Phase 0 candidate capture still runs, but no action is
// minted and no step is ever replayed), so landing Phase 1 changes no
// production behavior until an operator opts in with
// MEMQL_ACTION_REPLAY_ENABLED=1.
func actionReplayEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_ACTION_REPLAY_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// tryReplayAction looks up an active action minted under this step's input
// fingerprint and, if found, replays its captured capability calls token-free.
//   - replayed=true  -> the step was satisfied without the LLM; result is the
//     recorded result to return.
//   - found reports whether an action existed for this fingerprint at all (so
//     the caller mints one only when none existed, avoiding duplicate rows).
//
// A replay that errors or whose result fingerprint does not match the recorded
// one returns replayed=false so the caller falls back to the LLM. Trust decay
// on a mismatch lands in Phase 4 (#1739); Phase 1 simply declines to replay.
func (a *App) tryReplayAction(ctx context.Context, step harness.StepView, inputFP string) (replayed bool, result map[string]any, found bool) {
	adapter := &CognitionEngineAdapter{Engine: a.engine}
	q := fmt.Sprintf(`queryActionByInputFingerprint({inputFingerprint:%q})`, inputFP)
	res, err := adapter.Execute(ctx, q)
	if err != nil {
		return false, nil, false
	}
	payload, id, ok := firstActionRow(res)
	if !ok {
		return false, nil, false
	}
	found = true

	calls := parseReplayCalls(payload["calls"])
	if len(calls) == 0 {
		return false, nil, found
	}
	_, replayFP, err := actionreplay.Replay(ctx, a.engine, calls)
	if err != nil {
		if a.Logger != nil {
			a.Logger.Debug("action replay missed; falling back to LLM",
				"action", id, "step", step.ID, "error", err)
		}
		return false, nil, found
	}
	recordedFP, _ := payload["resultFingerprint"].(string)
	if !actionreplay.Verify(recordedFP, replayFP) {
		if a.Logger != nil {
			a.Logger.Debug("action replay fingerprint mismatch; falling back to LLM",
				"action", id, "step", step.ID)
		}
		return false, nil, found
	}

	a.reinforceAction(ctx, id, payload)

	result = map[string]any{"replayed": true}
	if rr, ok := payload["recordedResult"].(map[string]any); ok && rr != nil {
		// Copy so we can annotate without mutating the parsed payload.
		result = map[string]any{}
		for k, v := range rr {
			result[k] = v
		}
	}
	// Phase 2 (#1737): resolve a surface per world group and tag the result so
	// reads carry their resolved surface. Best-effort -- if no surfaces are
	// registered the replay still returns on the default surface.
	if tag := a.resolveReplaySurfaces(ctx, payload, calls); tag != nil {
		result["resolvedSurfaces"] = tag
	}
	return true, result, found
}

// resolveReplaySurfaces consults the surface registry + resolver to bind each
// replayed call to a surface (decision A: coupled calls pin to one surface),
// returning a per-call {capability, surface} tag plus any forced cross-world
// transfers. Returns nil when no surfaces are registered or resolution can't
// proceed -- replay then uses the default surface unchanged.
func (a *App) resolveReplaySurfaces(ctx context.Context, payload map[string]any, calls []actionreplay.ReplayCall) map[string]any {
	surfaces := a.loadOwnerSurfaces(ctx)
	if len(surfaces) == 0 {
		return nil
	}
	rcalls := make([]surfaceresolver.Call, 0, len(calls))
	for _, c := range calls {
		rcalls = append(rcalls, surfaceresolver.Call{Index: c.Index, Capability: c.Capability})
	}
	edges := parseSurfaceEdges(payload["resourceEdges"])
	plan, err := surfaceresolver.ResolvePlan(rcalls, edges, "", "", surfaces)
	if err != nil {
		if a.Logger != nil {
			a.Logger.Debug("surface resolution skipped; using default surface", "error", err)
		}
		return nil
	}
	perCall := make([]map[string]any, 0, len(plan.Resolutions))
	for _, r := range plan.Resolutions {
		perCall = append(perCall, map[string]any{
			"callIndex":  r.CallIndex,
			"capability": r.Capability,
			"surface":    r.Surface.Slug,
			"groupId":    r.GroupID,
		})
	}
	tag := map[string]any{"calls": perCall}
	if len(plan.Transfers) > 0 {
		transfers := make([]map[string]any, 0, len(plan.Transfers))
		for _, t := range plan.Transfers {
			transfers = append(transfers, map[string]any{
				"resource":    t.Resource,
				"fromSurface": t.FromSurface.Slug,
				"toSurface":   t.ToSurface.Slug,
				"fromCall":    t.FromCall,
				"toCall":      t.ToCall,
			})
		}
		tag["transfers"] = transfers
	}
	return tag
}

// loadOwnerSurfaces reads the calling owner's registered surfaces via
// querySurfacesForOwner and projects them for the resolver.
func (a *App) loadOwnerSurfaces(ctx context.Context) []surfaceresolver.Surface {
	adapter := &CognitionEngineAdapter{Engine: a.engine}
	res, err := adapter.Execute(ctx, `querySurfacesForOwner()`)
	if err != nil {
		return nil
	}
	m, ok := res.(map[string]any)
	if !ok {
		return nil
	}
	bundle, ok := m["bundle"].(map[string]any)
	if !ok {
		return nil
	}
	nodes, ok := bundle["nodes"].([]any)
	if !ok {
		return nil
	}
	out := make([]surfaceresolver.Surface, 0, len(nodes))
	for _, n := range nodes {
		node, ok := n.(map[string]any)
		if !ok {
			continue
		}
		id, _ := node["id"].(string)
		p, ok := node["payload"].(map[string]any)
		if !ok {
			continue
		}
		avail := true
		if b, ok := p["available"].(bool); ok {
			avail = b
		}
		prio := 100
		if f, ok := p["priority"].(float64); ok {
			prio = int(f)
		}
		slug, _ := p["slug"].(string)
		kind, _ := p["kind"].(string)
		machineID, _ := p["machineId"].(string)
		out = append(out, surfaceresolver.Surface{
			ID:           id,
			Slug:         slug,
			Kind:         kind,
			MachineID:    machineID,
			Capabilities: stringSlice(p["capabilities"]),
			Available:    avail,
			Priority:     prio,
		})
	}
	return out
}

// parseSurfaceEdges converts a stored resourceEdges JSON array into resolver
// edges.
func parseSurfaceEdges(v any) []surfaceresolver.ResourceEdge {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]surfaceresolver.ResourceEdge, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		from, _ := m["fromCallIndex"].(float64)
		to, _ := m["toCallIndex"].(float64)
		resource, _ := m["resource"].(string)
		out = append(out, surfaceresolver.ResourceEdge{
			FromCallIndex: int(from),
			ToCallIndex:   int(to),
			Resource:      resource,
		})
	}
	return out
}

// stringSlice coerces a JSON []any (or []string) into []string.
func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// mintActionFromRun mints a v1:actions:action (primitive) from a just-completed
// LLM step, keyed by inputFP, so the next identical-input dispatch replays it.
// Only the successfully-executed (non-error) captured calls are stored, with
// their literal args. Best-effort: a mint failure never fails the step.
func (a *App) mintActionFromRun(ctx context.Context, step harness.StepView, inputFP string, sink *candidateTraceSink, recordedResult map[string]any) {
	// Build the non-error captured subset, re-indexed 0..n so the stored
	// calls and the resource edges share one contiguous index space.
	subset := make([]actiontrace.CapturedCall, 0, len(sink.captured()))
	for _, c := range sink.captured() {
		if c.IsError {
			continue
		}
		subset = append(subset, actiontrace.CapturedCall{
			Index:      len(subset),
			Capability: c.Capability,
			Args:       c.Args,
			Result:     c.Result,
		})
	}
	if len(subset) == 0 {
		return // nothing replayable (pure-reasoning step)
	}

	calls := make([]map[string]any, 0, len(subset))
	resultsForFP := make([]any, 0, len(subset))
	dominantCap := ""
	for _, c := range subset {
		calls = append(calls, map[string]any{
			"index":      c.Index,
			"capability": c.Capability,
			"args":       c.Args,
		})
		resultsForFP = append(resultsForFP, c.Result)
		if dominantCap == "" {
			dominantCap = c.Capability
		}
	}

	// Resource-coupling edges over the SAME re-indexed subset, so Phase 2's
	// world-group resolver indexes line up with the stored calls.
	trace := actiontrace.Trace(subset, actiontrace.ProvenanceSources{StepInput: step.Input})
	edges := trace.ResourceEdges
	if edges == nil {
		edges = []actiontrace.ResourceEdge{}
	}

	resultFP := actionreplay.Fingerprint(resultsForFP)
	intent, _ := step.Input["goal"].(string)
	if strings.TrimSpace(intent) == "" {
		intent = "Replayable action minted from harness step " + step.ID
	}
	slug := dominantCap
	if slug == "" {
		slug = "action"
	}

	callsJSON, err := json.Marshal(calls)
	if err != nil {
		return
	}
	edgesJSON, err := json.Marshal(edges)
	if err != nil {
		return
	}
	resultJSON, err := json.Marshal(recordedResult)
	if err != nil {
		return
	}

	q := fmt.Sprintf(
		`mutationMintAction({slug:%q, intent:%q, capability:%q, inputFingerprint:%q, calls:%s, resourceEdges:%s, recordedResult:%s, resultFingerprint:%q, recordedSurface:%q, provenancePlanId:%q, provenanceStepId:%q})`,
		slug, intent, dominantCap, inputFP, string(callsJSON), string(edgesJSON), string(resultJSON), resultFP, "workbench", step.PlanID, step.ID,
	)
	adapter := &CognitionEngineAdapter{Engine: a.engine}
	if _, err := adapter.Execute(ctx, q); err != nil && a.Logger != nil {
		a.Logger.Warn("mint action failed", "plan", step.PlanID, "step", step.ID, "error", err)
	}
}

// reinforceAction bumps an action's reliability + reinforceCount after a
// verified replay. The new values are computed in Go (the MemQL parser has no
// arithmetic) and handed to mutationReinforceAction. Best-effort.
func (a *App) reinforceAction(ctx context.Context, id string, payload map[string]any) {
	rel, _ := payload["reliability"].(float64)
	cnt, _ := payload["reinforceCount"].(float64)
	nextRel, nextCnt := actionreplay.Reinforce(rel, int(cnt))
	q := fmt.Sprintf(`mutationReinforceAction({actionId:%q, reliability:%v, reinforceCount:%d})`,
		id, nextRel, nextCnt)
	adapter := &CognitionEngineAdapter{Engine: a.engine}
	if _, err := adapter.Execute(ctx, q); err != nil && a.Logger != nil {
		a.Logger.Warn("reinforce action failed", "action", id, "error", err)
	}
}

// firstActionRow navigates a query result envelope to the first matching row's
// full payload + id. Returns ok=false when no row matched.
func firstActionRow(res any) (payload map[string]any, id string, ok bool) {
	m, ok := res.(map[string]any)
	if !ok {
		return nil, "", false
	}
	bundle, ok := m["bundle"].(map[string]any)
	if !ok {
		return nil, "", false
	}
	nodes, ok := bundle["nodes"].([]any)
	if !ok || len(nodes) == 0 {
		return nil, "", false
	}
	node, ok := nodes[0].(map[string]any)
	if !ok {
		return nil, "", false
	}
	id, _ = node["id"].(string)
	payload, ok = node["payload"].(map[string]any)
	if !ok || id == "" {
		return nil, "", false
	}
	return payload, id, true
}

// parseReplayCalls converts the stored calls JSON ([]{index, capability, args})
// back into replayable calls.
func parseReplayCalls(v any) []actionreplay.ReplayCall {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]actionreplay.ReplayCall, 0, len(arr))
	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		capability, _ := m["capability"].(string)
		if capability == "" {
			continue
		}
		args, _ := m["args"].(map[string]any)
		idx := i
		if f, ok := m["index"].(float64); ok {
			idx = int(f)
		}
		out = append(out, actionreplay.ReplayCall{Index: idx, Capability: capability, Args: args})
	}
	return out
}
