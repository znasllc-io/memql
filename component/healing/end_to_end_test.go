package healing

import (
	"context"
	"testing"
)

// Epic 4 / memql#2144 (E4.6): the self-healing loop, end to end.
//
// This test wires all five prior pieces together in one flow, exercising the
// contract the meta-epic locked:
//
//	precondition-miss (E4.1)
//	  -> repair-loop proposal via a STUB model (E4.4)
//	    -> typed-patch Apply (E4.3)
//	      -> proposed overlay override, valid=false (E4.2 store)
//	        -> role-gated human validation flips valid=true + new version (E4.5)
//	          -> the two-tier resolver PREFERS the validated overlay over base
//
// It is engine/DB-free: the store + role gate are modeled by the same
// injected OverlayLookup the production resolver consumes, so the test proves
// the resolution contract deterministically. The DB-backed mutation +
// rank-gate paths are covered by their own guard tests
// (component/memql/healing_validation_rankbound_test.go) + the full DSL
// load-test; this test proves the PIECES compose.

func TestSelfHealing_EndToEnd(t *testing.T) {
	// The base construct: an automation whose `path` step input is a
	// machine-specific literal -- the portability problem a precondition miss
	// surfaces.
	base := map[string]any{
		"name": "deployStaging",
		"steps": []any{
			map[string]any{
				"id":   "run",
				"type": "function",
				"input": map[string]any{
					"path": "/Users/alice/engine/digest",
				},
			},
		},
	}

	// 1. PRECONDITION MISS (E4.1): the machine-specific literal does not hold
	//    here. The harness emitted healing.precondition.missed; we reconstruct
	//    the structured miss from the event payload.
	missEvent := map[string]any{
		"automationName":          "deployStaging",
		"preconditionId":          "enginePathPortable",
		"check":                   "exists(event.payload.enginePath)",
		"literal":                 "enginePath",
		"preconditionDescription": "the engine path must be portable",
		"triggerPayload":          map[string]any{"enginePath": ""},
	}
	miss := MissFromEventPayload(missEvent)
	if miss.BaseConstructId != "deployStaging" {
		t.Fatalf("miss mapping wrong: %+v", miss)
	}

	// 2. REPAIR-LOOP PROPOSAL via a STUB model (E4.4): the model proposes a
	//    relativize-literal patch -- the portability heal.
	stub := &stubProvider{respond: `{"patches":[
		{"kind":"relativize-literal","target":"steps.run.input.path","literal":"/Users/alice/engine/digest","replacement":"$config.MEMQL_ENGINE_DIGEST","reason":"relativize the machine-specific engine path"}
	]}`}
	loop := NewRepairLoop(stub)
	proposals, err := loop.Propose(context.Background(), miss, base)
	if err != nil {
		t.Fatalf("repair loop: %v", err)
	}
	if len(proposals) != 1 || proposals[0].Kind != PatchRelativizeLiteral {
		t.Fatalf("expected one relativize-literal proposal, got %+v", proposals)
	}

	// 3. TYPED-PATCH APPLY (E4.3): the proposal Apply'd to the base produces
	//    the healed overrideData. (The loop already dry-ran this; here we take
	//    the materialized result the store would persist.)
	overrideData, err := proposals[0].Apply(base)
	if err != nil {
		t.Fatalf("patch apply: %v", err)
	}

	// 4. PROPOSED OVERLAY OVERRIDE (E4.2 store), valid=false: an unvalidated
	//    heal is INVISIBLE to resolution. We model the store as the lookup the
	//    resolver consumes; while the override is unvalidated, the lookup
	//    returns nothing (the resolveValidOverride query filters valid=true).
	var validated bool
	var overrideVersion = 1
	lookup := func(_ context.Context, id string) (*Override, error) {
		if id != "deployStaging" || !validated {
			return nil, nil // unvalidated => invisible to resolution
		}
		return &Override{
			ID:              "ov-1",
			BaseConstructId: id,
			OverrideData:    overrideData,
			Version:         overrideVersion,
			Valid:           true,
		}, nil
	}
	baseProv := func(id string) (map[string]any, bool) {
		if id == "deployStaging" {
			return base, true
		}
		return nil, false
	}
	resolver := NewResolver(lookup, baseProv)

	// Before validation: resolution falls back to BASE (the heal is not yet
	// accepted, so it can never silently take effect).
	pre, err := resolver.Resolve(context.Background(), "deployStaging")
	if err != nil {
		t.Fatalf("pre-validation resolve: %v", err)
	}
	if pre.Tier != TierBase {
		t.Fatalf("an unvalidated heal must not shadow base; got tier %q", pre.Tier)
	}

	// 5. ROLE-GATED HUMAN VALIDATION (E4.5): a role-appropriate human accepts
	//    the heal -> valid=true + a new version. (The blast-radius rank gate
	//    is enforced server-side; here the acceptance is the state flip.)
	validated = true
	overrideVersion = 2 // capture-as-version: the accepted heal is a new version

	// After validation: resolution PREFERS the validated overlay over base,
	// and the resolved definition carries the healed (relativized) literal.
	post, err := resolver.Resolve(context.Background(), "deployStaging")
	if err != nil {
		t.Fatalf("post-validation resolve: %v", err)
	}
	if post.Tier != TierOverlay {
		t.Fatalf("a validated heal must shadow base; got tier %q", post.Tier)
	}
	if post.Override == nil || post.Override.Version != 2 {
		t.Fatalf("resolved override should carry the captured version 2, got %+v", post.Override)
	}
	step, _ := findStepById(post.Definition["steps"], "run")
	input, _ := step["input"].(map[string]any)
	if input["path"] != "$config.MEMQL_ENGINE_DIGEST" {
		t.Errorf("resolved healed definition missing the relativized literal: %v", input["path"])
	}

	// The base tier is untouched throughout (immutable, never LLM-healed).
	bstep, _ := findStepById(base["steps"], "run")
	binput, _ := bstep["input"].(map[string]any)
	if binput["path"] != "/Users/alice/engine/digest" {
		t.Errorf("the base construct was mutated by the heal: %v", binput["path"])
	}
}
