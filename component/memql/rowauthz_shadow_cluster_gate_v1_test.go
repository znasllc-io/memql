package memql

import (
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// TestShadowReadsTheLoweredClusterOwnerGate: an edition-2026 filter lowers
// the cluster-owner gate -- a comparison that reads no row field -- to a plan
// constant carrying its expression, where the pre-2026 converter produced a
// comparison on the actor path (memql#5368). The analyzer credits both
// spellings of the gate and nothing else: before it did, the flipped tree
// read every composite-tier query as undecidable and the land gate
// (TestRowAuthzEnforcementLandGate) failed on 260 constructs that carry the
// tier's own term.
func TestShadowReadsTheLoweredClusterOwnerGate(t *testing.T) {
	lower := func(src string) ExpressionNode {
		t.Helper()
		lam, err := languageParser.ParseV1Lambda(src)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		ir, err := Lower(lam.Body, LowerEnv{Position: tiers.PositionQueryFilter, Param: lam.Params[0], Args: map[string]ArgType{}})
		if err != nil {
			t.Fatalf("lower %s: %v", src, err)
		}
		return ir
	}
	composite := &languageParser.RowAuthzDecl{Tier: languageParser.RowAuthzOwned, Owner: "ownerUserId", ClusterOwnerBypass: true}
	plainOwned := &languageParser.RowAuthzDecl{Tier: languageParser.RowAuthzOwned, Owner: "ownerUserId"}
	clusterOwner := &languageParser.RowAuthzDecl{Tier: languageParser.RowAuthzClusterOwner}

	for _, tc := range []struct {
		decl   *languageParser.RowAuthzDecl
		filter string
		want   ShadowVerdict
	}{
		// The composite tier's own term, as every query over such a concept
		// spells it, and either of its arms alone.
		{composite, `row => row.status == "open" && (row.ownerUserId == actor.userId || actor.isClusterOwner == true)`, ShadowAlreadyImplied},
		{composite, `row => row.status == "open" && (actor.isClusterOwner == true || row.ownerUserId == actor.userId)`, ShadowAlreadyImplied},
		{composite, `row => row.status == "open" && actor.isClusterOwner == true`, ShadowAlreadyImplied},
		// Every spelling of the gate the comparison arm accepts.
		{clusterOwner, `row => actor.isClusterOwner`, ShadowAlreadyImplied},
		{clusterOwner, `row => true == actor.isClusterOwner`, ShadowAlreadyImplied},
		{clusterOwner, `row => actor.isClusterOwner != false`, ShadowAlreadyImplied},
		// A plain owned tier has no cluster-owner escape, so the gate does not
		// imply it -- and it is still a transparent conjunct, so the verdict
		// is would-narrow, as it was for the pre-2026 comparison.
		{plainOwned, `row => row.status == "open" && actor.isClusterOwner == true`, ShadowWouldNarrow},
		// Nothing but the gate is credited: another actor comparison is an
		// opaque plan constant, and a gate compared with false is not the gate.
		{clusterOwner, `row => actor.role == "admin"`, ShadowUndecidable},
		{clusterOwner, `row => actor.isClusterOwner == false`, ShadowUndecidable},
	} {
		got, reason := AnalyzeShadow(lower(tc.filter), tc.decl)
		if got != tc.want {
			t.Errorf("%s under %s: %s (%s), want %s", tc.filter, tc.decl.Tier, got, reason, tc.want)
		}
	}
}
