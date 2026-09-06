package planner

import (
	"context"
	"strings"
	"testing"
)

// mint_specialist_bundle_test.go -- the parts of the bundle mint that are
// decidable without an engine: what it refuses before it reads anything, and
// how it derives its edge ids.
//
// The refusals below run BEFORE the first Execute, which is what makes them
// testable against a loop with no engine at all -- and is also the property
// worth having: a malformed bundle costs no catalog read.

func TestABundleThatComposesNothingIsRefusedBeforeAnyRead(t *testing.T) {
	loop := &PlannerAgentLoop{}
	_, err := loop.MintSpecialistBundle(context.Background(), "plan-1", SpecialistBundle{
		Slug: "contract-review", Name: "Contract Review", Tier: "A",
		Justification: "the same three steps every contract review takes",
	}, "agent-1", "user-1")

	if err == nil {
		t.Fatal("an empty bundle was accepted")
	}
	if !strings.Contains(err.Error(), "composes at least one skill") {
		t.Fatalf("err = %v, want it to say what a bundle is", err)
	}
}

func TestAMalformedBundleIsRefusedBeforeAnyRead(t *testing.T) {
	loop := &PlannerAgentLoop{}
	for _, bundle := range []SpecialistBundle{
		{Name: "No Slug", Tier: "A", Justification: "why", ComponentSkillIds: []string{"s-1"}},
		{Slug: "no-name", Tier: "A", Justification: "why", ComponentSkillIds: []string{"s-1"}},
		{Slug: "no-tier", Name: "No Tier", Justification: "why", ComponentSkillIds: []string{"s-1"}},
		{Slug: "bad-tier", Name: "Bad Tier", Tier: "Z", Justification: "why", ComponentSkillIds: []string{"s-1"}},
		// A bundle that cannot say why it is not an extendSpecialist.
		{Slug: "no-why", Name: "No Why", Tier: "A", ComponentSkillIds: []string{"s-1"}},
	} {
		if _, err := loop.MintSpecialistBundle(context.Background(), "plan-1", bundle, "agent-1", "user-1"); err == nil {
			t.Fatalf("%+v was accepted", bundle)
		}
	}
}

// A re-mint over the same components must re-version the same edge rows
// rather than accumulating duplicates -- the singleton-id reasoning
// memql#4766 settled for the cluster rows, applied to a relation.
func TestABundleEdgeIdIsStableForAPair(t *testing.T) {
	first := bundleEdgeId("v1:skills:skill:contract-review", "v1:skills:skill:web-research")
	second := bundleEdgeId("v1:skills:skill:contract-review", "v1:skills:skill:web-research")
	if first != second {
		t.Fatalf("edge id moved between calls: %q then %q", first, second)
	}
	// And it is DIRECTED: the bundle depending on a component is not the same
	// fact as the reverse, so the two must not collide.
	reverse := bundleEdgeId("v1:skills:skill:web-research", "v1:skills:skill:contract-review")
	if reverse == first {
		t.Fatal("the two directions of a dependsOn edge share an id")
	}
}

// The id has to survive the engine's own segment rules, so a canonical id
// full of colons is reduced to the short id it was composed from.
func TestAnEdgeIdCarriesNoColons(t *testing.T) {
	got := bundleEdgeId("v1:skills:skill:contract-review", "v1:skills:skill:web-research")
	if strings.Contains(got, ":") {
		t.Fatalf("edge id = %q, which is not one id segment", got)
	}
	if !strings.Contains(got, "contract-review") || !strings.Contains(got, "web-research") {
		t.Fatalf("edge id = %q, want both ends legible in it", got)
	}
}

func TestABareIdPassesThroughUnchanged(t *testing.T) {
	if got := shortSegment("contract-review"); got != "contract-review" {
		t.Fatalf("shortSegment = %q", got)
	}
}
