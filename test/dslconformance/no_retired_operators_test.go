package dslconformance

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslgate"
)

// TestNoRetiredOperatorForms is the #977 lock-in: filter clauses must use the
// single unified operator grammar. The retired forms are rejected tree-wide:
//   - `;` AND separator   -> use `&&`
//   - `,` OR separator     -> use `||`
//   - `has` membership     -> use `in`
//   - `?.` optional-chain  -> use `when(args.x) { ... }`
//
// The rule itself now runs at LOAD time (memql#3629), so it covers a product
// DSL bundle mounted at MEMQL_DSL_PATH as well as this tree. What remains here
// is the corpus assertion over the embedded tree, running the same detector:
// the engine refuses a bundle that violates it, and this refuses a commit that
// would.
func TestNoRetiredOperatorForms(t *testing.T) {
	for _, v := range scanTreeForGate(t, dslgate.GateRetiredOperator) {
		t.Errorf("%s:%d %s", v.File, v.Line, v.Detail)
	}
}

// TestRetiredCommaIsFoundAtEveryDepth is the memql#3612 lock.
//
// `hasTopLevelComma` reported only depth 0, so the retired OR separator passed
// unnoticed inside parentheses -- which is exactly where an author reaches for
// it. The corpus was clean, so nothing failed; what was broken was the gate.
//
// The cases below are the whole distinction the detector has to make, because
// depth alone cannot make it: a comma inside `[ ... ]` or inside a CALL's
// parens is a separator, and a comma inside GROUPING parens is the retired OR.
func TestRetiredCommaIsFoundAtEveryDepth(t *testing.T) {
	retired := []string{
		`ownerUserId==actor.userId, title==args.v`,          // depth 0, always caught
		`(ownerUserId==actor.userId, visibility=="public")`, // the authz bypass
		`a && (b, c)`,
		`((x, y))`,
	}
	for _, s := range retired {
		if !hasTopLevelComma(s) {
			t.Errorf("hasTopLevelComma(%q) = false; the retired ',' OR separator must be "+
				"found at any depth -- inside parens it is an authorization bypass, because "+
				"the engine reads it as || and the per-row classifier read it as a conjunction", s)
		}
	}

	separators := []string{
		`isActiveRecord && name in ["MEMQL_A", "MEMQL_B"]`, // list literal
		`status in ["superseded", "failed"]`,               // list literal
		`concat(a, b) == "x"`,                              // call arguments
		`name in ["a,b", "c"]`,                             // comma inside a string
		`ownerUserId==actor.userId && title==args.v`,       // no comma at all
	}
	for _, s := range separators {
		if hasTopLevelComma(s) {
			t.Errorf("hasTopLevelComma(%q) = true; a comma separating list elements or call "+
				"arguments is not the retired operator, and flagging it would refuse the "+
				"three `in [...]` filters the tree ships today", s)
		}
	}
}
