package memql

import (
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestSafetyConceptsRegisterCanonicalIds is the regression test for #1632.
//
// The output-screening (v1:safety:outputScreening) and approval-request
// (v1:safety:approvalRequest) concepts shipped WITHOUT the @version /
// @namespace annotations that every other concept carries. The unified
// loader assembles a concept's canonical id from @version + @namespace +
// name (see AssembleConceptIdFromDecl); when both are absent the id comes
// back empty and the loader skips the concept silently (unified_loader.go
// "Concept hasn't migrated to @version + @namespace yet").
//
// The downstream effect: every query / mutation / logic construct that
// binds to one of those concepts resolves an EMPTY concept id over the
// connector -- queryAllOutputScreenings / queryActiveApprovalsByCorrelationKey
// failed with `concept "" not found in registry`, the insert/resolve
// mutations with `mutation template concept is required`, and the retention
// sweep logic at its `allRows` step.
//
// This test loads the full embedded DSL tree and asserts each safety
// concept is registered under its canonical id. It FAILS before the
// @version/@namespace fix (the two concepts are never registered) and
// PASSES after.
func TestSafetyConceptsRegisterCanonicalIds(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}

	// classification is the WORKING control -- it always carried the
	// annotations; the other two are the #1632 regressions.
	for _, id := range []string{
		"v1:safety:classification",
		"v1:safety:approvalRequest",
		"v1:safety:outputScreening",
	} {
		got, err := concept.Get(id)
		if err != nil {
			t.Errorf("concept %q not registered after LoadUnifiedConcepts: %v "+
				"(missing @version/@namespace would leave the canonical id empty "+
				"and the unified loader skips the concept silently -- #1632)", id, err)
			continue
		}
		if got == nil || got.Name != id {
			t.Errorf("concept %q resolved to unexpected record %+v", id, got)
		}
	}
}
