package auth

import "context"

// A VISITOR'S CALL RUNS UNDER SOMEBODY ELSE'S AUTHORITY.
//
// Two HTTP surfaces let a member of the public cause work on a deployable: a
// shopper form and an access-code call. A visitor is nobody to MemQL, so each
// runs the declared construct under the site owner's BORROWED authority
// (ContextWithUserActor). To everything downstream that context looks like the
// owner acting: it carries their id, and it is not synthetic.
//
// One decision must still tell the two apart. A person's API key pays for the
// work THAT PERSON does; it must never pay for work a stranger's click started
// (design record 2026-11-21-vendor-accounts-and-api-keys, decision K8 line 2).
// This mark is how the account rule knows: a context that carries it is the
// owner's authority lent to a visitor, and the only account that may pay for a
// model call made under it is the cluster's own.
//
// IT IS A MARK ON THE REQUEST'S CONTEXT. It grants nothing and changes no row
// tier. Two things follow the request and must not lose it:
//
//   - a GOAL the construct opens runs later, as its owner's own run, on a node
//     that never saw this context. Goal creation copies the mark onto the
//     goal's and the run's routing (common.RouteChoice.ForVisitor), and every
//     model call of that run, and of any goal it opens, carries it;
//   - an AUTOMATION the visitor's write triggers runs as the cluster, which
//     spends no key either, so nothing has to be carried there.
//
// Regression tests: visitor_test.go; the two surfaces assert it on the context
// they hand the engine (component/server); the goal's copy is
// integrations/work TestAGoalOpenedByAVisitorsRequestStaysAVisitors.

type visitorBorrowKey struct{}

// ContextWithVisitorBorrow marks ctx as a person's authority borrowed for a
// visitor's request.
func ContextWithVisitorBorrow(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, visitorBorrowKey{}, true)
}

// BorrowedForVisitor reports whether ctx was marked by
// ContextWithVisitorBorrow.
func BorrowedForVisitor(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(visitorBorrowKey{}).(bool)
	return marked
}
