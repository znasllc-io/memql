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
	// EMPTY -- the call-graph purity contract (ADR §2.1) is fully enforced
	// tree-wide with zero grandfathered violations (the #2235 burn-down is
	// complete).
	//
	// The final two entries (bootstrapSession + generateResponse, cognition's
	// two critical event-driven orchestrations) were migrated under #2271: the
	// engine now unwraps a logic/query step's Bundle-wrapped result so a
	// downstream automation step reads the returned value flat
	// (decide.result.x / field(decide.result, "x") / the scalar decide.result),
	// which unblocked the decide->persist pattern at real-DB runtime. Both pure
	// logics now only READ + decide (generateResponse also calls ai(), a
	// read/compute); their graph writes (createSessionForParticipant +
	// session.created emit; sendTextUtterance + presence bump) moved onto
	// `if`-gated automation steps in dsl/cognition/automations.memql.
	//
	// The retention/expiry/deletion date-window sweeps (accountDeletionSweep,
	// accessRequestExpirySweep, workerInvocationRetentionSweep) were migrated to
	// pure decide + window + forEach automation steps once the condition
	// evaluator learned to evaluate the date-window gate (#2256/#2254). The 4
	// non-windowing identity/worker/workbench sweeps migrated earlier (#2251).
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

// TestCallGraphCoverage asserts the gate above is actually running against the
// tree. TestCallGraphContract asserts only on FINDINGS, and a checker that
// splits nothing produces no findings -- so a clean tree and a dead checker are
// the same green.
//
// That is not hypothetical: in memql#3043 the mutation header matcher scanned
// for the retired `mutation` keyword (renamed to `mutate` in memql#2041), every
// mutations.memql split to nothing, and all four mutation rules -- including
// ADR §2.3 / authoring-rules Rule #1, one write per mutation body -- enforced
// nothing against 215 declarations while this gate passed throughout.
//
// Each restricted kind must split a non-zero number of constructs. A zero is
// not a small regression: it is that kind's rules silently enforcing nothing.
func TestCallGraphCoverage(t *testing.T) {
	coverage, err := callgraph.Coverage(".")
	if err != nil {
		t.Fatalf("walk DSL tree: %v", err)
	}
	if len(coverage) == 0 {
		t.Fatal("callgraph.Coverage reported no restricted kinds at all")
	}
	kinds := make([]string, 0, len(coverage))
	for kind := range coverage {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		if coverage[kind] == 0 {
			t.Errorf("call-graph coverage for %q is 0 constructs -- its rules are enforcing NOTHING against the tree.\n  Check the declaration keyword in component/memql/callgraph/tree.go against component/language/dslspec (memql#3043).", kind)
		}
	}
	t.Logf("call-graph coverage: %v", coverage)
}
