package dslconformance

// dslgate_delegation_test.go binds this package's gate vocabulary to
// component/memql/dslgate, which is where the CONTRACT gates now live
// (memql#3629).
//
// The definitions used to be here, and the gates that read them ran only as Go
// tests over this repo's own dsl.Tree(). A product's DSL arrives at RUNTIME
// through MEMQL_DSL_PATH and passes through no Go test at all, so the tree that
// most needs an authorization gate had none. The rules moved to load time; this
// file keeps the ~dozen tests in this package that consume them compiling
// against the one implementation.
//
// Aliases rather than copies, deliberately. Every defect this area has been
// filed for is two detectors that drifted, and the drift is reliably fail-open:
// the CI gate split filter clauses on `&&` while the editor rule did not
// (memql#2779), the classifier could not see the `,` operator the engine
// honours (memql#3612), the audit decided @serverOnly by a regex the loader did
// not use (memql#2875). A second copy here would be a fourth.

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslgate"
	"github.com/znasllc-io/memql/dsl"
)

// Detector patterns. See dslgate for what each one is for and why it is
// shaped the way it is.
var (
	constructHeaderRe  = dslgate.ConstructHeaderRe
	userScopeFieldRe   = dslgate.UserScopeFieldRe
	adminGateRe        = dslgate.AdminGateRe
	adminGateMentionRe = dslgate.AdminGateMentionRe
)

// maxPredicateNesting bounds the local predicate splitter's recursion, and is
// shared with dslgate's clause walker so the two agree about what is too deep
// to reason about.
const maxPredicateNesting = dslgate.MaxPredicateNesting

func blankComments(s string) string        { return dslgate.BlankComments(s) }
func structureOf(line string) string       { return dslgate.StructureOf(line) }
func rowSelectionSurface(b string) string  { return dslgate.RowSelectionSurface(b) }
func filterClauseOf(body string) string    { return dslgate.FilterClauseOf(body) }
func isClauseEndKeyword(trim string) bool  { return dslgate.IsClauseEndKeyword(trim) }
func delimitersBalanced(s string) bool     { return dslgate.DelimitersBalanced(s) }
func hasTopLevelComma(s string) bool       { return dslgate.HasTopLevelComma(s) }
func ownerScopeLeaf(pred string) bool      { return dslgate.OwnerScopeLeaf(pred) }
func adminGateLeaf(pred string) bool       { return dslgate.AdminGateLeaf(pred) }
func mentionsAdminGate(clause string) bool { return dslgate.MentionsAdminGate(clause) }

func matchingClose(src string, openIdx int) int { return dslgate.MatchingClose(src, openIdx) }

func splitTopLevelOn(s string, conn byte) []string     { return dslgate.SplitTopLevelOn(s, conn) }
func splitTopLevelSingle(s string, conn byte) []string { return dslgate.SplitTopLevelSingle(s, conn) }

func stripLeadingNot(p string) (string, bool)  { return dslgate.StripLeadingNot(p) }
func stripOuterParens(p string) (string, bool) { return dslgate.StripOuterParens(p) }
func unwrapWhenPredicate(p string) string      { return dslgate.UnwrapWhenPredicate(p) }

func clauseGuarantees(clause string, leaf func(string) bool) bool {
	return dslgate.ClauseGuarantees(clause, leaf)
}

// scanEmbeddedTree runs the load-time contract scan over the embedded tree.
//
// This is what makes the gates in this package and the engine's boot-time
// refusal the SAME check rather than two implementations of one rule. The
// @serverOnly verdict comes from the parsed tree here (serverOnlyConstructs);
// the engine takes it from its loaded FunctionRegistry. Both apply the loader's
// own rule -- presence of the attribute -- rather than a source regex, which is
// fail-open (memql#2875).
func scanEmbeddedTree(t *testing.T) []dslgate.Violation {
	t.Helper()
	serverOnly := serverOnlyConstructs(t)
	violations, err := dslgate.ScanTree(dsl.Tree(), dslgate.Options{
		ServerOnly: func(file, name string) bool {
			return serverOnly[serverOnlyKey{Path: file, Name: name}]
		},
	})
	if err != nil {
		t.Fatalf("dslgate.ScanTree: %v", err)
	}
	return violations
}

// scanTreeForGate returns the embedded tree's violations of one gate.
func scanTreeForGate(t *testing.T, gate dslgate.Gate) []dslgate.Violation {
	t.Helper()
	var out []dslgate.Violation
	for _, v := range scanEmbeddedTree(t) {
		if v.Gate == gate {
			out = append(out, v)
		}
	}
	return out
}
