package planner

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/harness/actionpin"
	"github.com/znasllc-io/memql/component/harness/actionplan"
	"github.com/znasllc-io/memql/component/harness/parambind"
	"github.com/znasllc-io/memql/component/memql"
)

// actionReplayEnabled gates the whole action-library emission path behind the
// same MEMQL_ACTION_REPLAY_ENABLED flag the harness replay/mint path uses
// (app/integrations_harness_replay.go). Default OFF: on shared main the
// planner never emits action steps, so the harness action-dispatch branch
// (#1758, PR B) never triggers either.
func actionReplayEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_ACTION_REPLAY_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// librarySearchK is how many library hits searchActions returns per sub-goal.
const librarySearchK = 5

// libraryHit pairs a ranked fit with the action row's payload, so a single
// search pass feeds both the decision (score/composite) and the bind check
// (the recorded calls) without re-querying.
type libraryHit struct {
	fit     actionplan.RankedFit
	payload map[string]any
}

// maybeSubstituteActionStep is the planner's per-sub-goal action-library
// decision (#1758, epic #1734): before dispatching an LLM task, cosine-search
// the action library for a reusable action that already accomplishes this
// sub-goal. On a REUSE/ADAPT verdict it rewrites the task input to an
// action-typed step (`{type:"action", ref, args}`) the harness replays
// token-free (PR B); on SYNTHESIZE it returns the task unchanged so the LLM
// runs and (on success) contributes a new action to the library.
//
// Best-effort: any search/parse failure leaves the task as the LLM path. The
// whole method is a no-op unless MEMQL_ACTION_REPLAY_ENABLED is set.
func (l *PlannerAgentLoop) maybeSubstituteActionStep(ctx context.Context, task plannerTask) plannerTask {
	if !actionReplayEnabled() {
		return task
	}
	// Don't re-substitute an already action-typed task.
	if t, _ := task.Input["type"].(string); t == "action" {
		return task
	}
	text := subGoalText(task)
	if text == "" {
		return task
	}

	hits := l.searchActionLibrary(ctx, text)
	if len(hits) == 0 {
		return task
	}

	thresholds := actionplan.DefaultThresholds()
	ranked := make([]actionplan.RankedFit, len(hits))
	for i, h := range hits {
		ranked[i] = h.fit
	}
	best := actionplan.BestFit(ranked, thresholds)
	if best.ActionID == "" {
		return task
	}
	winner := payloadForAction(hits, best.ActionID)
	best.CanBind = canBindAction(winner, task.Input)

	decision := actionplan.Decide(best, thresholds)
	if decision == actionplan.Synthesize {
		return task
	}

	ref := bestRef(best.ActionID, winner)
	// Carry the sub-goal's data fields through as the action's invocation
	// args (the executor's parambind re-binds the action's params from them);
	// strip the LLM-only control keys.
	args := map[string]any{}
	for k, v := range task.Input {
		switch k {
		case "template", "type", "ref", "args", "surface":
			continue
		}
		args[k] = v
	}
	rewritten := task
	rewritten.Input = map[string]any{
		"type": "action",
		"ref":  ref,
		"args": args,
	}
	if l.logger != nil {
		l.logger.Info("planner: substituting action-library step for sub-goal",
			"decision", string(decision),
			"ref", ref,
			"fitScore", best.Score,
			"canBind", best.CanBind,
			"logicalStepId", task.LogicalStepId,
		)
	}
	return rewritten
}

// searchActionLibrary cosine-searches the library by sub-goal text via the
// searchActions builtin (PR A) and returns the ranked hits with their
// payloads. The builtin embeds the query server-side and returns the latest
// active owner-scoped actions with `_similarity` injected on each payload.
func (l *PlannerAgentLoop) searchActionLibrary(ctx context.Context, text string) []libraryHit {
	argsJSON, err := json.Marshal(map[string]any{"text": text, "k": librarySearchK})
	if err != nil {
		return nil
	}
	res, err := l.engine.Execute(systemActorContext(ctx), "searchActions("+string(argsJSON)+")")
	if err != nil {
		if l.logger != nil {
			l.logger.Debug("planner: action library search failed; LLM path", "error", err)
		}
		return nil
	}
	rows := memql.MaterializeRows(res)
	hits := make([]libraryHit, 0, len(rows))
	for _, row := range rows {
		id := actionRowString(row, "id")
		if id == "" {
			continue
		}
		payload := row
		if p, ok := row["payload"].(map[string]any); ok {
			payload = p
		}
		hits = append(hits, libraryHit{
			fit: actionplan.RankedFit{
				Fit:         actionplan.Fit{ActionID: id, Score: actionRowFloat(row, "_similarity")},
				IsComposite: actionRowString(row, "kind") == "composite",
			},
			payload: payload,
		})
	}
	return hits
}

// canBindAction reports whether every step-input-sourced binding of the
// action's recorded calls resolves to a top-level key in the sub-goal input.
// A complete bind keeps a high-fit hit a REUSE; an incomplete bind downgrades
// it to ADAPT (the executor's parameterized replay / an LLM arg-fix closes the
// gap at execution). Both still emit an action step.
func canBindAction(payload, input map[string]any) bool {
	calls := parseActionCalls(payload)
	if len(calls) == 0 {
		// No recorded parameterization -> trivially bindable (a literal-only
		// replay needs nothing from the input).
		return true
	}
	for _, c := range calls {
		for _, b := range c.Bindings {
			if b.Kind != parambind.SourceStepInput || b.Ref == "" {
				continue
			}
			root := b.Ref
			if i := strings.IndexAny(root, ".["); i >= 0 {
				root = root[:i]
			}
			if _, ok := input[root]; !ok {
				return false
			}
		}
	}
	return true
}

func payloadForAction(hits []libraryHit, actionID string) map[string]any {
	for _, h := range hits {
		if h.fit.ActionID == actionID {
			return h.payload
		}
	}
	return nil
}

func parseActionCalls(payload map[string]any) []parambind.Call {
	if payload == nil {
		return nil
	}
	raw, ok := payload["calls"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var calls []parambind.Call
	if err := json.Unmarshal(b, &calls); err != nil {
		return nil
	}
	return calls
}

// bestRef formats the pinned ref (id@version) for the winning action, reading
// the version off its payload; floats to the latest version when no version is
// recorded (pin-by-default per the action-library design).
func bestRef(actionID string, payload map[string]any) string {
	ref := actionpin.Ref{ID: actionID, Floating: true}
	if payload != nil {
		switch v := payload["version"].(type) {
		case float64:
			if int(v) >= 1 {
				ref = actionpin.Ref{ID: actionID, Version: int(v)}
			}
		case json.Number:
			if n, err := v.Int64(); err == nil && n >= 1 {
				ref = actionpin.Ref{ID: actionID, Version: int(n)}
			}
		}
	}
	return actionpin.Format(ref)
}

// subGoalText derives the searchable sub-goal text from a dispatched task's
// input by scanning the descriptive string fields a planner task carries.
func subGoalText(task plannerTask) string {
	for _, key := range []string{"goal", "instructions", "description", "summary", "prompt", "text", "template"} {
		if s, _ := task.Input[key].(string); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func actionRowString(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return v
	}
	if p, ok := row["payload"].(map[string]any); ok {
		if v, ok := p[key].(string); ok {
			return v
		}
	}
	return ""
}

func actionRowFloat(row map[string]any, key string) float64 {
	read := func(m map[string]any) (float64, bool) {
		switch v := m[key].(type) {
		case float64:
			return v, true
		case json.Number:
			if f, err := v.Float64(); err == nil {
				return f, true
			}
		}
		return 0, false
	}
	if f, ok := read(row); ok {
		return f
	}
	if p, ok := row["payload"].(map[string]any); ok {
		if f, ok := read(p); ok {
			return f
		}
	}
	return 0
}
