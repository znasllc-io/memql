package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// ANONYMOUS ROW ADMISSION (epic memql#4541, D4).
//
// The tier ships ENFORCED AND EMPTY. Nothing in the engine tree declares
// @rowAuthz(public); a product bundle declares it on its own content
// concepts when it means to publish them. So on every existing cluster this
// file changes nothing at all -- there is no concept for it to admit anyone
// to, and no bridge accepts an anonymous session unless an operator turned
// one on.
//
// # The rule, stated once
//
// An anonymous actor is admitted to a concept declaring the `public` tier,
// and to no other concept, whatever that concept declares -- including a
// concept that declares NOTHING.
//
// # Why the undeclared case is the whole design
//
// rowAuthzAdmits answers ADMIT for an undeclared concept. That is correct
// for the callers it was written for: an undeclared concept is one nobody
// has classified, the tree has about 88 of them, and refusing them all
// would break every read in the product to buy a guarantee nobody had
// stated. It is exactly wrong for an anonymous caller. "Nobody has
// classified this" cannot mean "publish it to the internet", and a public
// tier that inherited that default would publish most of the graph on the
// day it shipped -- with every gate in the system reporting success,
// because each one did precisely what it was asked.
//
// So the anonymous branch answers FIRST and never falls through, in both
// directions. That is the same shape as connectorAdmission, and for a
// related reason: a targeted rule keyed on what the concept DECLARES, never
// a bypass that inherits whatever some other tier happens to grant.
//
// # Two seams, again
//
// Row-authz enforcement has two mechanisms and the anonymous caller has to
// be decided at both:
//
//   - The ROW GATE (rowAuthzAdmits), below. It decides one row from its own
//     concept's declaration and is the ONLY enforcement a raw query string
//     gets, since filter injection resolves from plan.BoundConcept and a
//     generic browse has none.
//   - The PLAN, refuseNonPublicReadForAnonymous. Strictly speaking the row
//     gate alone is sufficient -- it denies every row a non-public read
//     could return -- but "sufficient" there means the query RUNS, scans,
//     and returns an empty result. An empty result is indistinguishable
//     from "there is nothing here", which is the wrong thing to tell a
//     caller who was refused, and it makes an unauthenticated visitor a
//     free way to run arbitrary reads against the database for their side
//     effects on load. The plan refusal makes it an error before the read.

// anonymousAdmission decides a row for an ANONYMOUS actor, and reports
// whether the caller is anonymous at all.
//
// The two return values are separate for the reason connectorAdmission's
// are: "not anonymous" must fall through to the ordinary tier rules, while
// "anonymous, and this concept is not public" must DENY and never fall
// through -- or the undeclared default would hand a stranger everything.
func anonymousAdmission(ctx context.Context, conceptName string) (admitted bool, isAnonymous bool) {
	if !auth.IsAnonymousActor(ctx) {
		return false, false
	}
	return conceptDeclaresPublicTier(conceptName), true
}

// conceptDeclaresPublicTier reports whether the concept declares
// @rowAuthz(public).
//
// A concept the registry cannot produce answers FALSE. That is the opposite
// of the undeclared default and it is deliberate: reach comes from a
// declaration, and a concept nothing can resolve has made none. For every
// other caller an unresolvable concept still takes the ordinary path.
func conceptDeclaresPublicTier(conceptName string) bool {
	decl := rowAuthzDeclFor(strings.TrimSpace(conceptName))
	return decl != nil && decl.Tier == langparser.RowAuthzPublic
}

// refuseNonPublicReadForAnonymous refuses a read an anonymous caller is not
// entitled to, BEFORE it runs.
//
// It resolves from plan.BoundConcept, like enforceRowAuthzOnPlan. A plan
// with no bound concept -- a raw query string, the generic browse -- cannot
// be decided here and is left to the row gate, which denies every row it
// could have returned. That is the same division of labour the rest of this
// package uses, and it is why the row gate is the load-bearing half rather
// than this one.
//
// The refusal is an ERROR rather than an empty result, for the reason
// refuseRowAuthzWithoutActor gives: an empty result is indistinguishable
// from "there is nothing here", and telling an anonymous caller that a
// concept they may not read is empty is a worse answer than telling them
// they may not read it. Neither answer reveals a row.
func refuseNonPublicReadForAnonymous(ctx context.Context, plan *QueryPlan) error {
	if plan == nil || !auth.IsAnonymousActor(ctx) {
		return nil
	}
	conceptName := strings.TrimSpace(plan.BoundConcept)
	if conceptName == "" {
		return nil
	}
	if conceptDeclaresPublicTier(conceptName) {
		return nil
	}
	return fmt.Errorf(
		"row-authz: %s does not declare @rowAuthz(public) and this read carries no identity. "+
			"An anonymous caller reaches public-tier concepts only -- an UNDECLARED concept is "+
			"unmeasured rather than public, so it refuses too (epic memql#4541, D4). Authenticate "+
			"the call, or declare the tier on the concept if its rows are genuinely publishable",
		conceptName)
}
