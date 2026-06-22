package client

import "context"

// keyset_named.go is a documented carve-out (parallel to
// concept_browser.go + keyset_pagination.go): thin, SDK-owned
// keyset-paginated wrappers around specific named primitives whose
// list views can exceed the engine's page size.
//
// Why these exist: the generated typed methods (QueryAllPlans, ...)
// dispatch through executeNamed, which neither threads an inbound
// continuation cursor nor surfaces the response nextCursor -- so a
// consumer of the plain generated method silently sees only the first
// page (the underlying DSL queries are `sort` + `paginate 50`). These
// wrappers reuse the generated *Build() call string (so they STAY on
// the named-primitive surface -- no raw DSL inlined) and run it through
// ExecutePaginated, which does thread the cursor both ways. The result
// is a typed, cursor-aware "load more" method the cockpit list views
// call instead of the one-shot generated method.
//
// This is intentionally NOT the generated path: regenerating every
// query method to thread cursors (the Go mirror of TS PR #1994) is a
// generator change tracked separately; until then these hand-rolled
// wrappers cover the concrete now-capped list views (memql#1997 /
// epic #1964). Add a wrapper here only for a named query that genuinely
// renders an unbounded list AND declares `sort` + `paginate`.

// QueryAllPlansPage returns ONE keyset page of queryAllPlans plus the
// cursor to continue. Pass an empty cursor for the first page; thread
// PageResult.NextCursor back in for "load more". An empty NextCursor
// means the plan set is exhausted. Backs the cockpit Planner list view.
func (qc *QueryClient) QueryAllPlansPage(ctx context.Context, args QueryAllPlansArgs, cursor string) (*PageResult, error) {
	return qc.ExecutePaginated(ctx, QueryAllPlansBuild(args), cursor)
}

// QueryAuthoringBundlesForOwnerPage returns ONE keyset page of
// queryAuthoringBundlesForOwner plus the continuation cursor. Backs the
// cockpit Bundles list view, whose per-owner bundle set accumulates
// over time and exceeds one page.
func (qc *QueryClient) QueryAuthoringBundlesForOwnerPage(ctx context.Context, args QueryAuthoringBundlesForOwnerArgs, cursor string) (*PageResult, error) {
	return qc.ExecutePaginated(ctx, QueryAuthoringBundlesForOwnerBuild(args), cursor)
}
