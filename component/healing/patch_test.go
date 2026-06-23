package healing

import (
	"context"
	"testing"
)

// Epic 4 / memql#2141: the typed-patch model.
//
// Each of the four patch kinds applies to a base construct to produce a
// VALID healed overlay override; invalid patches are rejected; and a patch
// that would produce an unloadable construct is rejected.

// A representative base automation construct (the JSON shape the loader
// produces: name + trigger + preconditions[] + steps[]).
func baseAutomation() map[string]any {
	return map[string]any{
		"name": "deployStaging",
		"preconditions": []any{
			map[string]any{"id": "envIsStaging", "check": `$config.MEMQL_ENV == "staging"`},
		},
		"steps": []any{
			map[string]any{
				"id":   "run",
				"type": "function",
				"input": map[string]any{
					"path": "/Users/alice/engine/digest",
					"from": "$steps.fetch.result.id",
				},
			},
		},
	}
}

// --- add-precondition ----------------------------------------------------

func TestApply_AddPrecondition(t *testing.T) {
	base := baseAutomation()
	p := &Patch{
		Kind: PatchAddPrecondition,
		Precondition: &PatchPrecondition{
			ID:      "digestPinned",
			Check:   "exists(event.payload.imageDigest)",
			Literal: "imageDigest",
		},
		Reason: "deploy needs a pinned digest",
	}
	out, err := p.Apply(base)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	pcs, _ := out["preconditions"].([]any)
	if len(pcs) != 2 {
		t.Fatalf("want 2 preconditions after add, got %d", len(pcs))
	}
	added, _ := pcs[1].(map[string]any)
	if added["id"] != "digestPinned" || added["check"] != "exists(event.payload.imageDigest)" {
		t.Errorf("appended precondition wrong: %v", added)
	}
	// Base must be untouched (immutability of the base tier).
	if basePcs, _ := base["preconditions"].([]any); len(basePcs) != 1 {
		t.Errorf("Apply mutated the base construct: base now has %d preconditions", len(basePcs))
	}
}

func TestApply_AddPrecondition_DuplicateIdRejected(t *testing.T) {
	base := baseAutomation()
	p := &Patch{
		Kind:         PatchAddPrecondition,
		Precondition: &PatchPrecondition{ID: "envIsStaging", Check: "x == y"},
	}
	if _, err := p.Apply(base); err == nil {
		t.Fatalf("expected duplicate-id rejection")
	}
}

// --- insert-guard --------------------------------------------------------

func TestApply_InsertGuard_SetsConditionOnStep(t *testing.T) {
	base := baseAutomation()
	p := &Patch{
		Kind:   PatchInsertGuard,
		Target: "steps.run",
		Guard:  "exists(event.payload.ready)",
	}
	out, err := p.Apply(base)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	step, _ := findStepById(out["steps"], "run")
	if step["condition"] != "exists(event.payload.ready)" {
		t.Errorf("guard not set on step: %v", step["condition"])
	}
}

func TestApply_InsertGuard_ANDsExistingCondition(t *testing.T) {
	base := baseAutomation()
	// Pre-set a condition so the guard must AND-compose (strengthen).
	step, _ := findStepById(base["steps"], "run")
	step["condition"] = "a == b"
	p := &Patch{Kind: PatchInsertGuard, Target: "steps.run", Guard: "c == d"}
	out, err := p.Apply(base)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := findStepById(out["steps"], "run")
	want := "(a == b) && (c == d)"
	if got["condition"] != want {
		t.Errorf("composed condition = %q, want %q", got["condition"], want)
	}
}

func TestApply_InsertGuard_MissingStepRejected(t *testing.T) {
	base := baseAutomation()
	p := &Patch{Kind: PatchInsertGuard, Target: "steps.nope", Guard: "x == y"}
	if _, err := p.Apply(base); err == nil {
		t.Fatalf("expected rejection for a guard targeting a non-existent step")
	}
}

// --- relativize-literal --------------------------------------------------

func TestApply_RelativizeLiteral(t *testing.T) {
	base := baseAutomation()
	p := &Patch{
		Kind:        PatchRelativizeLiteral,
		Target:      "steps.run.input.path",
		Literal:     "/Users/alice/engine/digest",
		Replacement: "$config.MEMQL_ENGINE_DIGEST",
	}
	out, err := p.Apply(base)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	step, _ := findStepById(out["steps"], "run")
	input, _ := step["input"].(map[string]any)
	if input["path"] != "$config.MEMQL_ENGINE_DIGEST" {
		t.Errorf("literal not relativized: %v", input["path"])
	}
}

func TestApply_RelativizeLiteral_MismatchRejected(t *testing.T) {
	base := baseAutomation()
	p := &Patch{
		Kind:        PatchRelativizeLiteral,
		Target:      "steps.run.input.path",
		Literal:     "/some/other/path", // does not match the current value
		Replacement: "$config.X",
	}
	if _, err := p.Apply(base); err == nil {
		t.Fatalf("expected rejection when the named literal does not match the current value")
	}
}

func TestApply_RelativizeLiteral_AbsentFieldRejected(t *testing.T) {
	base := baseAutomation()
	p := &Patch{Kind: PatchRelativizeLiteral, Target: "steps.run.input.nope", Replacement: "$config.X"}
	if _, err := p.Apply(base); err == nil {
		t.Fatalf("expected rejection for relativizing an absent field")
	}
}

// --- rebind-param --------------------------------------------------------

func TestApply_RebindParam(t *testing.T) {
	base := baseAutomation()
	p := &Patch{
		Kind:        PatchRebindParam,
		Target:      "steps.run.input.from",
		Replacement: "$steps.lookup.result.id",
	}
	out, err := p.Apply(base)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	step, _ := findStepById(out["steps"], "run")
	input, _ := step["input"].(map[string]any)
	if input["from"] != "$steps.lookup.result.id" {
		t.Errorf("param not rebound: %v", input["from"])
	}
}

// --- Validate ------------------------------------------------------------

func TestValidate_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name  string
		patch *Patch
	}{
		{"unknown kind", &Patch{Kind: "bogus"}},
		{"add-precondition no precondition", &Patch{Kind: PatchAddPrecondition}},
		{"add-precondition no id", &Patch{Kind: PatchAddPrecondition, Precondition: &PatchPrecondition{Check: "x"}}},
		{"add-precondition no check", &Patch{Kind: PatchAddPrecondition, Precondition: &PatchPrecondition{ID: "x"}}},
		{"insert-guard no target", &Patch{Kind: PatchInsertGuard, Guard: "x"}},
		{"insert-guard no guard", &Patch{Kind: PatchInsertGuard, Target: "steps.run"}},
		{"relativize no target", &Patch{Kind: PatchRelativizeLiteral, Replacement: "$config.X"}},
		{"relativize no replacement", &Patch{Kind: PatchRelativizeLiteral, Target: "steps.run.input.path"}},
		{"rebind no target", &Patch{Kind: PatchRebindParam, Replacement: "$x"}},
		{"rebind no replacement", &Patch{Kind: PatchRebindParam, Target: "steps.run.input.from"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.patch.Validate(); err == nil {
				t.Errorf("expected Validate to reject %s", tc.name)
			}
		})
	}
}

func TestValidate_AcceptsWellFormed(t *testing.T) {
	good := []*Patch{
		{Kind: PatchAddPrecondition, Precondition: &PatchPrecondition{ID: "a", Check: "x == y"}},
		{Kind: PatchInsertGuard, Target: "steps.run", Guard: "x == y"},
		{Kind: PatchRelativizeLiteral, Target: "steps.run.input.path", Replacement: "$config.X"},
		{Kind: PatchRebindParam, Target: "steps.run.input.from", Replacement: "$steps.b.result"},
	}
	for _, p := range good {
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(%s) rejected a well-formed patch: %v", p.Kind, err)
		}
	}
}

// --- result-loads gate ---------------------------------------------------

// A patch that produces a structurally invalid construct (a precondition with
// a blank check) is rejected by the result-loads gate even though the patch
// itself is well-formed-looking. Construct directly to bypass Validate's
// per-kind checks and exercise validateConstructLoads.
func TestApply_ResultLoadsGate_RejectsBlankCheck(t *testing.T) {
	// add-precondition Validate requires a check, so to exercise the loads
	// gate we test validateConstructLoads directly on a corrupt construct.
	bad := map[string]any{
		"name":          "x",
		"preconditions": []any{map[string]any{"id": "p", "check": "   "}},
	}
	if err := validateConstructLoads(bad); err == nil {
		t.Fatalf("expected validateConstructLoads to reject a blank-check precondition")
	}
}

func TestValidateConstructLoads_DuplicatePrecondition(t *testing.T) {
	bad := map[string]any{
		"preconditions": []any{
			map[string]any{"id": "dup", "check": "a == b"},
			map[string]any{"id": "dup", "check": "c == d"},
		},
	}
	if err := validateConstructLoads(bad); err == nil {
		t.Fatalf("expected duplicate-precondition rejection")
	}
}

func TestValidateConstructLoads_StepMissingId(t *testing.T) {
	bad := map[string]any{
		"steps": []any{map[string]any{"type": "function"}},
	}
	if err := validateConstructLoads(bad); err == nil {
		t.Fatalf("expected step-missing-id rejection")
	}
}

// The produced override is the overrideData the proposeOverride mutation
// stores -- assert it round-trips as the same JSON shape a healedOverride
// carries (a plain map[string]any).
func TestApply_ProducesOverlayOverrideData(t *testing.T) {
	base := baseAutomation()
	p := &Patch{Kind: PatchAddPrecondition, Precondition: &PatchPrecondition{ID: "g", Check: "x == y"}}
	out, err := p.Apply(base)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out["name"] != "deployStaging" {
		t.Errorf("overrideData lost the construct identity")
	}
	// overrideData must be a plain JSON-shaped map (the healedOverride
	// overrideData field) -- assert it deep-copies cleanly.
	if _, err := deepCopyMap(out); err != nil {
		t.Errorf("overrideData is not a clean JSON map: %v", err)
	}
}

// End-to-end across E4.2 + E4.3: a typed patch's Apply output, stored as a
// healed overlay override, resolves through the two-tier resolver and
// SHADOWS the base construct. This is the load-bearing seam between the
// patch model (E4.3) and the base/overlay store (E4.2).
func TestPatchResultResolvesAsOverlay(t *testing.T) {
	base := baseAutomation()
	p := &Patch{
		Kind:        PatchRelativizeLiteral,
		Target:      "steps.run.input.path",
		Literal:     "/Users/alice/engine/digest",
		Replacement: "$config.MEMQL_ENGINE_DIGEST",
	}
	overrideData, err := p.Apply(base)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Wire the resolver: the overlay lookup returns the patched override; the
	// base provider returns the original base.
	lookup := func(_ context.Context, id string) (*Override, error) {
		return &Override{ID: "ov-1", BaseConstructId: id, OverrideData: overrideData, Version: 2, Valid: true}, nil
	}
	baseProv := func(id string) (map[string]any, bool) {
		if id == "deployStaging" {
			return base, true
		}
		return nil, false
	}
	r := NewResolver(lookup, baseProv)

	got, err := r.Resolve(context.Background(), "deployStaging")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Tier != TierOverlay {
		t.Fatalf("tier = %q, want overlay (the patched override must shadow base)", got.Tier)
	}
	step, _ := findStepById(got.Definition["steps"], "run")
	input, _ := step["input"].(map[string]any)
	if input["path"] != "$config.MEMQL_ENGINE_DIGEST" {
		t.Errorf("resolved definition does not carry the healed (relativized) literal: %v", input["path"])
	}
	// And the base tier is untouched -- base still has the machine literal.
	bstep, _ := findStepById(base["steps"], "run")
	binput, _ := bstep["input"].(map[string]any)
	if binput["path"] != "/Users/alice/engine/digest" {
		t.Errorf("base construct was mutated by the patch: %v", binput["path"])
	}
}
