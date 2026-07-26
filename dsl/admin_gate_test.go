package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// ownerScopeLeaf and adminGateLeaf name the leaf predicates the authz gates
// recognise. They live here rather than inline so TestPerRowAuthzClassification
// and TestAdminGateIsATopLevelConjunct cannot drift about what counts as a
// caller check -- the drift this file's sibling gates keep being filed for.
func ownerScopeLeaf(pred string) bool { return strings.Contains(pred, "actor.userId") }

// adminGateRe matches a predicate that establishes admin-ness WHEN TRUE.
//
// POLARITY is load-bearing, and a bare strings.Contains does not carry it: the
// composition rule asks whether a FALSE gate zeroes the row set, so the leaf
// must be a term that is false for a non-admin. `actor.isClusterOwner!=true`
// and `==false` contain the same identifier and inverT the meaning -- under
// them a non-owner who satisfies the other conjunct gets rows and the cluster
// owner gets none, which is the very failure this gate exists to refuse.
// stripLeadingNot deliberately leaves `!=` alone (it is a comparison, not a
// negation), so nothing upstream catches it either.
//
// Word boundaries matter for the same reason: `requiresClusterOwnerXyz` is a
// different identifier, and a substring test accepted it as the gate.
//
// The spec alternatives are ordered longest-first so `requiresOwnerOrAdmin`
// cannot be consumed as `requiresOwner`. NOTE `requiresClusterOwner` is not
// defined anywhere in dsl/ today -- it is the name the per-row-authz audit doc
// uses and a #54 follow-up; the live context-spec gates are requiresAdmin /
// requiresOwner / requiresOwnerOrAdmin (dsl/common/specs.memql,
// dsl/deployment/specs.memql). None is currently used in a filter, so listing
// them is inert on the corpus and stops the gate going blind the first time
// one is.
var adminGateRe = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])(?:actor\.isClusterOwner[ \t]*==[ \t]*true|requiresOwnerOrAdmin|requiresClusterOwner|requiresAdmin|requiresOwner)([^A-Za-z0-9_]|$)`)

func adminGateLeaf(pred string) bool { return adminGateRe.MatchString(pred) }

// adminGateMentionRe is the POLARITY-BLIND twin, and the two must stay
// separate: selection has to be broad and assertion strict.
//
// If the gate selected constructs with the strict predicate, an inverted
// filter (`actor.isClusterOwner!=true`) would simply not be recognised as
// admin-gated, get skipped, and sail through -- swapping one fail-open for
// another. Selecting on any MENTION and then demanding the strict form is what
// turns the inverted spelling into an error instead of a silence.
var adminGateMentionRe = regexp.MustCompile(
	`(^|[^A-Za-z0-9_.])(?:actor\.isClusterOwner|requiresOwnerOrAdmin|requiresClusterOwner|requiresAdmin|requiresOwner)([^A-Za-z0-9_]|$)`)

func mentionsAdminGate(clause string) bool { return adminGateMentionRe.MatchString(clause) }

// TestAdminGateIsATopLevelConjunct hard-fails when an admin-gated filter
// composes its gate so a FALSE gate cannot zero the result set (memql#2839).
//
// The engine primitive is correct: a request with no AccessContext resolves
// `actor.isClusterOwner` to false (#2801/#2811), and the SQL bind and the
// in-process post-filter agree on it (#2822). Whether that false leaf actually
// empties the row set is a property of how the AUTHOR composed it, and nothing
// checked that:
//
//	filter  fromE164==args.e164 && actor.isClusterOwner==true   // gate holds
//	filter  fromE164==args.e164 || actor.isClusterOwner==true   // gate is OFF
//
// On the second, a false gate zeroes nothing -- the left term alone admits
// rows, so the construct reads as gated while serving any caller who supplies
// `fromE164`. `tryCompileCombinedFilter`'s LogicalOr branch emits
// `(left OR right)` and the database returns the left side.
//
// That is a fail-open COMPOSITION of a correct primitive, and it was invisible
// to everything: the term is well-formed, the field resolves, and the engine
// tests assert the leaf's value rather than its placement.
//
// This is the hard-failing half of #2832. That change stopped such a filter
// being MISLABELLED `admin` in the classification table; it could not fail the
// build, because reaching the `flagged` bucket additionally requires a
// user-scope field and an admin filter need not have one. Same predicate,
// different job: classify there, refuse here.
func TestAdminGateIsATopLevelConjunct(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	checked := 0
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		src := string(raw)

		for _, m := range constructHeaderRe.FindAllStringSubmatchIndex(src, -1) {
			closeIdx := matchingClose(src, m[1]-1)
			if closeIdx < 0 {
				continue
			}
			body := src[m[1]:closeIdx]
			name := src[m[4]:m[5]]

			// filterClauseOf strips comments, so the prose in
			// dsl/authoring/queries.memql and dsl/identity/queries.memql that
			// merely NAMES the gate is excluded, as is every `actor.` read in
			// a logic body -- neither is a filter.
			clause := filterClauseOf(body)
			if clause == "" || !mentionsAdminGate(clause) {
				continue
			}
			checked++
			if !clauseGuarantees(clause, adminGateLeaf) {
				lineNo := strings.Count(src[:m[0]], "\n") + 1
				t.Errorf("%s:%d  %s: the admin gate does not hold on every path through its filter -- a false gate would NOT zero the result set.\n    filter  %s\n    The gate must be a top-level conjunct in the affirmative form (`<predicate> && actor.isClusterOwner==true`). A gate inside a disjunction is switched off by the other arm; an inverted one (`!=true`, `==false`) admits exactly the callers it should refuse.",
					p, lineNo, name, clause)
			}
		}
	}

	// A corpus that stopped producing admin-gated filters, or an extractor
	// that stopped finding them, would leave this test green while protecting
	// nothing.
	if checked == 0 {
		t.Fatal("no admin-gated filter clauses found; the corpus shape or filterClauseOf changed and this gate has silently stopped protecting anything")
	}
	t.Logf("checked %d admin-gated filter clause(s)", checked)
}

// TestAdminGateCompositionRules pins the rule itself, independently of the
// corpus -- which today happens to satisfy it, so the corpus alone cannot
// prove the gate works.
func TestAdminGateCompositionRules(t *testing.T) {
	cases := []struct {
		name   string
		clause string
		want   bool
	}{
		{"bare gate", `actor.isClusterOwner==true`, true},
		{"gate as a conjunct", `partitionId==args.partitionId && actor.isClusterOwner==true`, true},
		{"gate first", `actor.isClusterOwner==true && statusIsActive`, true},
		// The shipped telephony shape: a disjunction is fine as long as the
		// gate sits OUTSIDE it.
		{"disjunction inside, gate outside", `(fromE164==args.e164 || toE164==args.e164) && actor.isClusterOwner==true`, true},
		{"spec form", `requiresClusterOwner && statusIsActive`, true},

		// The defect: the gate is switched off by the other arm.
		{"gate as a disjunct", `fromE164==args.e164 || actor.isClusterOwner==true`, false},
		{"gate as a disjunct, first", `actor.isClusterOwner==true || fromE164==args.e164`, false},
		{"parens dropped", `fromE164==args.e164 || toE164==args.e164 && actor.isClusterOwner==true`, false},
		{"spec form as a disjunct", `requiresClusterOwner || statusIsActive`, false},
		{"negated gate", `!(actor.isClusterOwner==true)`, false},
		{"gate behind a when guard", `when(args.adminMode) { actor.isClusterOwner==true }`, false},

		// Review round 1: POLARITY. These contain the gate identifier and a
		// top-level `&&`, so a substring leaf accepted them -- while inverting
		// the meaning. Under `!=true` a non-owner satisfying the other conjunct
		// gets rows and the cluster owner gets none.
		{"inverted with !=", `fromE164==args.e164 && actor.isClusterOwner!=true`, false},
		{"inverted with ==false", `fromE164==args.e164 && actor.isClusterOwner==false`, false},
		{"bare inverted", `actor.isClusterOwner!=true`, false},

		// Word boundaries: a different identifier is not the gate.
		{"identifier containing the spec name", `x==args.x && requiresClusterOwnerXyz`, false},
		{"identifier prefixed by the spec name", `x==args.x && myRequiresAdmin`, false},

		// The live context-spec gates, none of which the corpus filters use
		// yet -- listed so the gate does not go blind the first time one does.
		{"requiresAdmin as a conjunct", `statusIsActive && requiresAdmin`, true},
		{"requiresOwnerOrAdmin as a conjunct", `statusIsActive && requiresOwnerOrAdmin`, true},
		{"requiresOwnerOrAdmin is not requiresOwner", `statusIsActive && requiresOwnerOrAdmin`, true},
		{"requiresAdmin as a disjunct", `statusIsActive || requiresAdmin`, false},

		// Whitespace around the comparison is legal.
		{"spaced comparison", `x==args.x && actor.isClusterOwner == true`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clauseGuarantees(tc.clause, adminGateLeaf); got != tc.want {
				t.Errorf("clauseGuarantees(%q, adminGateLeaf) = %v, want %v", tc.clause, got, tc.want)
			}
		})
	}
}
