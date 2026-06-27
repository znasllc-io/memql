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
	// #2235: NOT migrated, blocked on engine bug #2254. These two (plus
	// workerInvocationRetentionSweep below) gate their per-row write on a
	// date window `addDuration(item.<ts>, "P{N}D") < timestamp()`, which is
	// CONSTANT-FALSE in the automation condition evaluator (numeric-only `<`
	// via toNumber() -> 0 for RFC3339; addDuration/timestamp not evaluated in
	// the filter-value path). The current impure logic compiles to the
	// identical condition, so these crons are already dead in production --
	// accountDeletionSweep never hard-deletes a user past cooldown
	// (compliance). A migration that preserved that dead behavior while
	// removing the baseline line would falsely mark them "done." Migrate once
	// #2254 lands. The 4 non-windowing identity/worker/workbench sweeps were
	// migrated to forEach (#2251 + follow-up).
	"logic-purity|logic|accessRequestExpirySweep",
	"logic-purity|logic|accountDeletionSweep",
	"logic-purity|logic|appendAttachmentToRequest",
	// #2235: deferred. The 3 conditional creates gate the CRITICAL system.startup
	// bootstrap; migrating to if-steps needs behavioral verification of the
	// condition-eval semantics (existingCluster.Empty() && node.type=="bff") that a
	// load-only test can't prove. Left baselined per the safe-or-defer rule.
	"logic-purity|logic|bootstrapSession",
	"logic-purity|logic|generateResponse",
	// #2235: deferred. Per-row sweep (updateNodeHealth inside `for ... range`) needs
	// a forEach automation step; struct-form forEach/for is documented (ADR S7) but
	// NOT yet parsed (NormaliseAutomationSource rejects it), and a go-style
	// `func (Automation)` loop is the internal-only lowering form, not an author
	// surface. Same blocker as the identity/worker sweep logics below.
	// #2235: NOT migrated, blocked on engine bug #2254 -- date-window gate
	// `addDuration(item.createdAt, "P{N}D") < timestamp()` is constant-false
	// in the condition evaluator. See the accessRequest/accountDeletion note
	// at the top of this baseline. Migrate once #2254 lands.
	"logic-purity|logic|workerInvocationRetentionSweep",
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
