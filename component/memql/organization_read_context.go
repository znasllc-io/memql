package memql

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
)

// OrganizationReadContext gives a side-table reader fresh authorization
// evidence and a holder for failed authorization reads. Unlike ContextForPerson
// it does not replace the actor: borrowed, forwarded and synthetic authority
// must retain exactly the identity and restrictions on the original context.
// Both memos are fresh because a previously memoized failed read cannot report
// its failure to a holder installed later.
func (e *MemQLEngine) OrganizationReadContext(ctx context.Context) (context.Context, *auth.ReadFailures) {
	ctx, failures := auth.ContextWithReadFailures(ctx)
	return context.WithValue(ctx, accountScopeMemoKey{}, &accountScopeMemo{engine: e}), failures
}
