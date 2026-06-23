package automations

import "testing"

// Regression for memql#1396: the automation condition evaluator must
// implement the `exists(<path>)` filter builtin.
//
// Root cause: `exists(payload.X)` has no comparison operator, so before this
// fix it fell through `evaluateAtomicCondition` to EvaluateValue + isTruthy,
// where it rendered as the LITERAL string "exists(payload.X)". A non-empty
// string is always truthy, so `@filter(exists(payload.X))` ALWAYS passed
// regardless of the field. That silently disabled the gate on
// reRouteNeedsAgentOnAgentCreate (it fired on every signup's plan-less agent
// create, then called updatePlanStatus with an empty planId -> the
// per-signup "missing planId" failure) and on cascadeSupersession.
//
// Multi-node note: this is the engine's pure-Go condition evaluator -- it runs
// identically on whichever node hosts the automation scheduler, so there is no
// cross-node state involved. The fix is correct for the 2-replica mesh because
// the filter is evaluated wherever the agent.created event is consumed and the
// triggering event payload travels with the event; no node-local state is read.

func evalWithEventPayload(t *testing.T, payload map[string]any, cond string) bool {
	t.Helper()
	e := NewEvaluator()
	e.SetCustom("event", map[string]any{"payload": payload})
	got, err := e.EvaluateCondition(cond)
	if err != nil {
		t.Fatalf("EvaluateCondition(%q): %v", cond, err)
	}
	return got
}

// The plan-less agent create (the signup case) must NOT pass the gate: the
// field is absent from the event payload, so exists(...) is false and the
// reroute automation no-ops instead of calling updatePlanStatus with
// an empty planId.
func TestEvaluateCondition_ExistsAbsentField(t *testing.T) {
	// Mirrors a seed-materialized per-user agent: originatingPlanId lives under
	// lineage (or is absent entirely), never at the top level the filter reads.
	payload := map[string]any{
		"id":      "v1:agents:agent:assistant-user1",
		"lineage": map[string]any{"createdBy": "system", "originatingPlanId": ""},
	}
	if got := evalWithEventPayload(t, payload, "exists(payload.originatingPlanId)"); got {
		t.Errorf("exists(payload.originatingPlanId) = true for plan-less agent create, want false")
	}
}

// A present-but-empty-string field must also count as "not exists" (matches the
// coalesce empty-string-is-missing semantics), so a blank originatingPlanId
// stamped at the top level does not spuriously fire the reroute.
func TestEvaluateCondition_ExistsEmptyString(t *testing.T) {
	payload := map[string]any{"originatingPlanId": ""}
	if got := evalWithEventPayload(t, payload, "exists(payload.originatingPlanId)"); got {
		t.Errorf("exists(payload.originatingPlanId) = true for empty string, want false")
	}
	// whitespace-only is also empty
	payload2 := map[string]any{"originatingPlanId": "   "}
	if got := evalWithEventPayload(t, payload2, "exists(payload.originatingPlanId)"); got {
		t.Errorf("exists(payload.originatingPlanId) = true for whitespace-only string, want false")
	}
}

// The legitimate with-plan create (the [Create X agent] flow off a
// plan.needsAgent card stamps the Plan id at the top level) MUST pass the gate
// so the reroute still fires.
func TestEvaluateCondition_ExistsPresentField(t *testing.T) {
	payload := map[string]any{"originatingPlanId": "v1:planner:plan:abc123"}
	if got := evalWithEventPayload(t, payload, "exists(payload.originatingPlanId)"); !got {
		t.Errorf("exists(payload.originatingPlanId) = false for present field, want true")
	}
}

// exists() must compose with && (normalised to the AND separator) and with a
// leading ! NOT, the two ways the copresent filters actually use it
// (cascadeSupersession is `payload.validationStatus=="validated" &&
// exists(payload.supersedesDocumentId)`).
func TestEvaluateCondition_ExistsComposition(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		cond    string
		want    bool
	}{
		{
			name:    "and both true",
			payload: map[string]any{"validationStatus": "validated", "supersedesDocumentId": "v1:knowledge:document:d1"},
			cond:    `payload.validationStatus=="validated" && exists(payload.supersedesDocumentId)`,
			want:    true,
		},
		{
			name:    "and exists false short-circuits",
			payload: map[string]any{"validationStatus": "validated"},
			cond:    `payload.validationStatus=="validated" && exists(payload.supersedesDocumentId)`,
			want:    false,
		},
		{
			name:    "not exists on absent field is true",
			payload: map[string]any{"id": "x"},
			cond:    `!exists(payload.originatingPlanId)`,
			want:    true,
		},
		{
			name:    "not exists on present field is false",
			payload: map[string]any{"originatingPlanId": "v1:planner:plan:abc"},
			cond:    `!exists(payload.originatingPlanId)`,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalWithEventPayload(t, tc.payload, tc.cond); got != tc.want {
				t.Errorf("EvaluateCondition(%q) = %v, want %v", tc.cond, got, tc.want)
			}
		})
	}
}

// exists() over a non-scalar value: an empty array/object is "not exists",
// a populated one exists.
func TestEvaluateCondition_ExistsCollections(t *testing.T) {
	if got := evalWithEventPayload(t, map[string]any{"phases": []any{}}, "exists(payload.phases)"); got {
		t.Errorf("exists(empty array) = true, want false")
	}
	if got := evalWithEventPayload(t, map[string]any{"phases": []any{"a"}}, "exists(payload.phases)"); !got {
		t.Errorf("exists(non-empty array) = false, want true")
	}
}
