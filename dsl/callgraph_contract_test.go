package dsl_test

import (
	"sort"
	"testing"

	"github.com/znasllc-io/memql/component/actions/capability"
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
// Every entry is a `<rule>|<kind>|<construct>` key. All are `logic-purity`:
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
	// #2235: DEFERRED (safe-or-defer). bootstrapSession and generateResponse are
	// cognition's two critical event-driven orchestrations -- session bootstrap
	// (read existing session, then conditionally create the session row + emit
	// session.created) and the core AI-response path (idempotency check, then a
	// gated ai() call whose result feeds a sendTextUtterance write + a presence
	// update). Both carry multiple conditional writes with intermediate
	// dependencies; an `if`-step / forEach migration is mechanically possible but
	// needs DB-backed behavioral verification of the gate + chaining semantics
	// that a load-only test can't prove, on paths where a regression silently
	// breaks session creation or every AI reply. A prior migration attempt was
	// reverted for exactly this reason. Left baselined pending an owner decision
	// to migrate the critical paths with DB-backed verification.
	//
	// The retention/expiry/deletion date-window sweeps (accountDeletionSweep,
	// accessRequestExpirySweep, workerInvocationRetentionSweep) were migrated to
	// pure decide + window + forEach automation steps once the condition
	// evaluator learned to evaluate the date-window gate (#2256/#2254). The 4
	// non-windowing identity/worker/workbench sweeps migrated earlier (#2251).
	"logic-purity|logic|bootstrapSession",
	"logic-purity|logic|generateResponse",
}

// TestCallGraphContract is the HARD whole-tree gate for the behavioral
// call-graph contract (ADR §2). I7 sources the builtin side-effect classifier
// from the capability registry (component/actions/capability), so the
// read-only-builtin rule (ADR §3) now fires on the real tree: a side-effecting
// integration builtin used inside a query/logic is a finding (one such
// pre-existing case is baselined above).
func TestCallGraphContract(t *testing.T) {
	sideEffecting, err := capability.ClassifierFromDir(".")
	if err != nil {
		t.Fatalf("build builtin side-effect classifier: %v", err)
	}
	findings, err := callgraph.CheckTree(".", callgraph.SideEffectClassifier(sideEffecting))
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
