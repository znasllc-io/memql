package dslconformance

import (
	"errors"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslgate"
)

// TestNoRetiredOperatorForms is the #977 lock-in: filter clauses use one
// operator grammar -- edition 2026's, whose parser refuses every retired
// spelling (`;` and `,` as connectives, `has`, `not in`, `when(...) { }`,
// `?.`) with its replacement, and a filter with no lambda header is itself
// the retired form.
//
// The rule itself runs at LOAD time (memql#3629), so it covers a product DSL
// bundle mounted at MEMQL_DSL_PATH as well as this tree. What remains here is
// the corpus assertion over the embedded tree, running the same detector: the
// engine refuses a bundle that violates it, and this refuses a commit that
// would.
func TestNoRetiredOperatorForms(t *testing.T) {
	for _, v := range scanTreeForGate(t, dslgate.GateRetiredOperator) {
		t.Errorf("%s:%d %s", v.File, v.Line, v.Detail)
	}
}

// TestRetiredCommaIsFoundAtEveryDepth is the memql#3612 lock: the retired `,`
// OR separator is refused wherever it sits -- inside parentheses too, which is
// exactly where an author reaches for it, and where it was an authorization
// bypass: the engine read it as `||` while the per-row classifier read it as a
// conjunction. The parser is the detector, and depth alone cannot make the
// distinction it makes: a comma inside `[ ... ]`, inside a call's parens or
// between a lambda's parameters is a separator; anywhere else it is refused.
func TestRetiredCommaIsFoundAtEveryDepth(t *testing.T) {
	retired := []string{
		`row => row.ownerUserId == actor.userId, row.title == args.v`,
		`row => (row.ownerUserId == actor.userId, row.visibility == "public")`, // the authz bypass
		`row => row.a == 1 && (row.b == 2, row.c == 3)`,
	}
	for _, s := range retired {
		_, err := langparser.ParseV1Lambda(s)
		var rf *langparser.RetiredFormError
		if !errors.As(err, &rf) || rf.Form.Rule != "retired_comma_connective" {
			t.Errorf("%q: got %v, want the retired ',' OR separator refused -- inside parens it is "+
				"an authorization bypass", s, err)
		}
	}

	separators := []string{
		`row => isActiveRecord(row) && row.name in ["MEMQL_A", "MEMQL_B"]`, // list literal
		`row => row.status in ["superseded", "failed"]`,                    // list literal
		`row => row.campaignId == canonicalId(args.id, "campaign")`,        // call arguments
		`row => row.name in ["a,b", "c"]`,                                  // comma inside a string
		`row => row.items.all((a, b) => a == b)`,                           // lambda parameters
		`row => row.ownerUserId == actor.userId && row.title == args.v`,    // no comma at all
	}
	for _, s := range separators {
		if _, err := langparser.ParseV1Lambda(s); err != nil {
			t.Errorf("%q: %v; a comma separating list elements, call arguments or lambda "+
				"parameters is not the retired operator", s, err)
		}
	}
}
