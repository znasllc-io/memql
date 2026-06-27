package dsl

import (
	"sort"
	"testing"

	"github.com/znasllc-io/memql/component/memql/callgraph"
)

// callGraphBaseline grandfathers the call-graph contract violations (ADR §2)
// that PREDATE the contract. The whole-tree gate is now HARD (memql#2217): it
// fails on any finding NOT in the baseline (a regression) and on any baseline
// entry that no longer matches a finding (burn-down -- delete the line). Newly
// authored constructs are additionally blocked at define->promote by the
// authoring sandbox (strict), so this baseline only guards direct edits to the
// existing debt.
//
// Every entry is a `<rule>|<kind>|<construct>` key. All 22 are `logic-purity`:
// logics that perform a graph write inline (the §2.1 single-writer debt). The
// handoff scoped only the four cluster pass-throughs, but enforcing the
// contract tree-wide surfaced the full set across cognition / planner /
// knowledge / identity / rbac. Migrating each to a pure logic + an
// automation-step write is the burn-down backlog (owner-prioritized; epic
// #2212). The sweep/loop logics (per-row writes inside `for ... range`) in
// particular need a forEach automation step, not a mechanical move. When one is
// migrated, its line here disappears from the findings and the gate flags it as
// stale -- delete the line in the same PR.
var callGraphBaseline = []string{
	"logic-purity|logic|accessRequestExpirySweep",
	"logic-purity|logic|accountDeletionSweep",
	"logic-purity|logic|appendAttachmentToRequest",
	"logic-purity|logic|bootstrapCluster",
	"logic-purity|logic|bootstrapSession",
	"logic-purity|logic|deregisterNode",
	"logic-purity|logic|generateResponse",
	"logic-purity|logic|killSwitchSuspendsRunningPlans",
	"logic-purity|logic|magicLinkExpirySweep",
	"logic-purity|logic|pruneStaleClusterNodes",
	"logic-purity|logic|recordMentoring",
	"logic-purity|logic|recordTransition",
	"logic-purity|logic|registerNode",
	"logic-purity|logic|releaseWorkspaceOnPlanTerminal",
	"logic-purity|logic|revokeExpiredDelegations",
	"logic-purity|logic|routeRequest",
	"logic-purity|logic|workerInvocationRetentionSweep",
}

// TestCallGraphContract is the HARD whole-tree gate for the behavioral
// call-graph contract (ADR §2). The builtin side-effect classifier is nil until
// I7 declares sideEffectClass on capabilities, so the read-only-builtin rule
// does not fire on the real tree yet.
func TestCallGraphContract(t *testing.T) {
	findings, err := callgraph.CheckTree(".", nil)
	if err != nil {
		t.Fatalf("walk DSL tree: %v", err)
	}

	keys := map[string]bool{}
	for _, f := range findings {
		keys[f.Rule+"|"+f.Kind+"|"+f.Construct] = true
	}
	base := map[string]bool{}
	for _, b := range callGraphBaseline {
		base[b] = true
	}

	var regressions, stale []string
	for k := range keys {
		if !base[k] {
			regressions = append(regressions, k)
		}
	}
	for b := range base {
		if !keys[b] {
			stale = append(stale, b)
		}
	}
	sort.Strings(regressions)
	sort.Strings(stale)

	for _, k := range regressions {
		t.Errorf("NEW call-graph violation (not baselined): %s\n  Fix it -- the contract is enforced (ADR §2). If it is intentional, append it to callGraphBaseline with a justification.", k)
	}
	for _, b := range stale {
		t.Errorf("stale baseline entry no longer violates: %q\n  Remove the line from callGraphBaseline (burn-down).", b)
	}
}
