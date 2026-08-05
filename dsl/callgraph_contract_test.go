package dsl_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

	// A non-zero floor catches TOTAL deadness and nothing weaker. Measured
	// against the code as it stood BEFORE this check existed: a filter skipping
	// 17 of the 24 mutations.memql files dropped mutation coverage from 215 to
	// 22 and the floor above still passed -- most of the rule surface silently
	// switched off, green. That filter now fails, here rather than above, which
	// is the whole reason the exact count is asserted against an independently
	// computed number and not just against zero.
	for _, kind := range kinds {
		want := declarationsInTree(t, kind)
		if got := coverage[kind]; got != want {
			t.Errorf("call-graph coverage for %q is %d constructs but the tree declares %d.\n"+
				"  The checker is looking at less of the tree than the tree contains, which is\n"+
				"  memql#3043's failure in partial form: the rules enforce nothing on the gap.\n"+
				"  Filters on what gets checked belong in constructsForFile so this stays honest.",
				kind, got, want)
		}
	}
}

// kindKeywords restates the kind -> declaration-keyword mapping DELIBERATELY,
// rather than importing the checker's own table.
//
// That restatement is the entire value of this oracle. A count derived from the
// same table and the same splitter the checker uses cannot detect the checker
// drifting -- it moves with it, and agrees with it while both are wrong. That
// is precisely how memql#3043 survived: every signal came from the one matcher
// that was broken. The keyword literals here are pinned independently by
// component/language/dslspec's own drift test, so a genuine rename turns THAT
// red rather than silently rotting this.
var kindKeywords = map[string]string{
	"mutation": "mutate",
	"query":    "query",
	"logic":    "logic",
	"action":   "action",
}

// declarationsInTree counts the declaration headers of one kind by scanning the
// tree directly -- not through callgraph's splitter.
func declarationsInTree(t *testing.T, kind string) int {
	t.Helper()
	keyword, ok := kindKeywords[kind]
	if !ok {
		t.Fatalf("no independent keyword known for restricted kind %q -- add it to kindKeywords, "+
			"otherwise this kind's coverage is asserted against nothing", kind)
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(keyword) + `[ \t]+[A-Za-z_]`)

	count := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Same skip rule as the engine walker and callgraph.walkTree.
			if d.Name() != "." && strings.HasPrefix(d.Name(), "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".memql") {
			return nil
		}
		// Only files whose name maps to this kind, matching how the checker
		// infers kind from the file name.
		base := strings.TrimSuffix(filepath.Base(path), ".memql")
		if singularForTest(base) != kind {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		count += len(re.FindAllIndex(raw, -1))
		return nil
	})
	if err != nil {
		t.Fatalf("scan tree for %q declarations: %v", kind, err)
	}
	return count
}

// singularForTest maps a construct file's base name to its kind. Restated here
// for the same reason kindKeywords is.
func singularForTest(base string) string {
	switch base {
	case "logic":
		return "logic"
	case "mutations":
		return "mutation"
	case "queries":
		return "query"
	case "actions":
		return "action"
	}
	return strings.TrimSuffix(base, "s")
}
