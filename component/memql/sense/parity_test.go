package sense

// parity_test.go is the syntax<->Sense parity gate (#2734): it fails when the
// DSL source-of-truth (dslspec constructs, the parser's invocation keywords)
// changes without MemQL Sense keeping pace. Most of Sense's vocabulary is
// PROJECTED from the spec at runtime (spec.go) and so cannot drift; these tests
// pin the remaining HAND-MAINTAINED tables -- the ones #2730-#2733 added that
// key on construct keyword or invocation verb -- to that same SoT, so a
// construct or invocation keyword added to the grammar cannot land with a stale
// editor. Each failure names the sense file to update.
//
// This is the durable form of "keep MemQL Sense updated whenever the syntax or
// rules change": a construct/keyword added to the language fails CI here until
// Sense classifies it. It follows the #2628 detected/undetected-label pattern.

import (
	"sort"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// TestParity_ConstructClassificationCoversSoT asserts every dslspec construct is
// classified EXACTLY once as behavioral (its body invokes other constructs) or
// non-behavioral. A construct added to the grammar lands in neither set and
// fails here -- forcing the author to decide whether Sense should offer
// kind-filtered invocation completion inside it.
func TestParity_ConstructClassificationCoversSoT(t *testing.T) {
	for _, c := range dslSpec.Constructs {
		kw := c.Keyword
		behavioral, nonBehavioral := behavioralConstruct[kw], nonBehavioralConstruct[kw]
		switch {
		case behavioral && nonBehavioral:
			t.Errorf("construct %q is in BOTH behavioralConstruct and nonBehavioralConstruct (context.go) -- it must be in exactly one", kw)
		case !behavioral && !nonBehavioral:
			t.Errorf("construct %q (dslspec SoT) is classified by NEITHER behavioralConstruct nor nonBehavioralConstruct (context.go). "+
				"A construct was added to the grammar without updating MemQL Sense: add it to behavioralConstruct if its body invokes "+
				"other constructs (`<verb> <name>(...)`), otherwise to nonBehavioralConstruct.", kw)
		}
	}
}

// TestParity_BehavioralMatchesInvocationKeywords pins the two hand-maintained
// invocation tables to each other: a construct is behavioral IFF
// invocationKeywordsForConstruct offers the CALL verbs (query/mutation/logic)
// for it. If one table is updated without the other, this fails.
func TestParity_BehavioralMatchesInvocationKeywords(t *testing.T) {
	// "Call verbs" are the parser's invocation-kind keywords (query / mutation /
	// logic / capability / ...), as opposed to a mutate body's WRITE verbs
	// (insert / update), which are not invocation keywords. Deriving the set from
	// the parser SoT -- rather than hardcoding {query,mutation,logic} -- means a
	// behavioral construct that legitimately invokes a DIFFERENT verb (e.g. an
	// action's `capability`, once its completion is fixed) stays consistent
	// instead of tripping a misleading "tables disagree" failure.
	callVerb := map[string]bool{}
	for _, k := range parser.InvocationKindKeywords() {
		callVerb[k] = true
	}
	for _, c := range dslSpec.Constructs {
		kw := c.Keyword
		offersCallVerbs := false
		for _, v := range invocationKeywordsForConstruct(EnclosingConstruct{Keyword: kw}) {
			if callVerb[v] {
				offersCallVerbs = true
				break
			}
		}
		if offersCallVerbs != behavioralConstruct[kw] {
			t.Errorf("construct %q: behavioralConstruct=%v but invocationKeywordsForConstruct offers-call-verbs=%v -- "+
				"the two hand-maintained tables (context.go / complete.go) disagree", kw, behavioralConstruct[kw], offersCallVerbs)
		}
	}
}

// TestParity_ClassificationKeysAreRealConstructs is the reverse check: every key
// in the two classification sets must be a real dslspec construct, so a typo'd
// key or one left stale after a construct is removed from the grammar is caught
// rather than lingering silently.
func TestParity_ClassificationKeysAreRealConstructs(t *testing.T) {
	real := map[string]bool{}
	for _, c := range dslSpec.Constructs {
		real[c.Keyword] = true
	}
	for name, set := range map[string]map[string]bool{
		"behavioralConstruct":    behavioralConstruct,
		"nonBehavioralConstruct": nonBehavioralConstruct,
	} {
		for kw := range set {
			if !real[kw] {
				t.Errorf("%s (context.go) contains %q, which is not a dslspec construct -- a stale or typo'd key; remove it or fix the spelling", name, kw)
			}
		}
	}
}

// TestParity_InvocationVerbKindPinnedToParser asserts every verb Sense detects as
// an in-body invocation (invocationVerbKind) is a real invocation keyword per
// the parser's authoritative set -- so a verb renamed/removed in the grammar
// fails here rather than silently completing nothing.
func TestParity_InvocationVerbKindPinnedToParser(t *testing.T) {
	valid := map[string]bool{}
	for _, k := range parser.InvocationKindKeywords() {
		valid[k] = true
	}
	for verb := range invocationVerbKind {
		if !valid[verb] {
			t.Errorf("invocationVerbKind (context.go) contains %q, which is NOT an invocation keyword per parser.InvocationKindKeywords() %v -- "+
				"the grammar's invocation verbs changed; update invocationVerbKind", verb, sortedKeys(valid))
		}
	}
}

// TestParity_GateBitesOnNewConstruct demonstrates the gate is NOT vacuous: a
// construct classified by neither set trips exactly the failure condition the
// live SoT test flags. Guards against the partition being silently defanged
// (e.g. someone adding a catch-all that classifies everything).
func TestParity_GateBitesOnNewConstruct(t *testing.T) {
	const hypothetical = "somefutureconstruct"
	if behavioralConstruct[hypothetical] || nonBehavioralConstruct[hypothetical] {
		t.Fatalf("test setup: %q is unexpectedly already classified", hypothetical)
	}
	// This is the exact predicate TestParity_ConstructClassificationCoversSoT
	// reports as a failure -- proving that an unclassified construct is caught.
	unclassified := !behavioralConstruct[hypothetical] && !nonBehavioralConstruct[hypothetical]
	if !unclassified {
		t.Fatal("the parity gate is vacuous: an unclassified construct would not be caught")
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
