package memql

// THE CLUSTER-OWNER TIER'S READ FLOOR (memql#5216).
//
// ===========================================================================
// WHAT WAS MISSING, AND WHY IT SHOWED UP AS A LIE RATHER THAN AS A REFUSAL
// ===========================================================================
// `v1:bench:run` and `v1:bench:sample` are the proving suite's published
// figures. They have no owner field at all -- the concept's own comment says
// "there is nobody whose row this is" -- so `clusterOwner` was the only tier
// that fit, and declaring it said two things at once: only the deployment
// WRITES these, and only the cluster owner READS them.
//
// The second half was never intended. MemQL OS surfaces them at Settings ->
// Benchmarks with `roles: { min: "admin" }`, so a non-owner admin was admitted
// to the screen and served nothing -- and that screen is built so that an
// absence takes the same room as a number, an unmeasured run drawing an OPEN
// NOTCH rather than a bar of height zero. So the refusal rendered as the
// suite's own word for "nobody has measured this". A permission answer wearing
// the costume of a measurement, on the surface whose entire design principle is
// that an absence must be legible.
//
// ===========================================================================
// IT RELAXES THE READ AND LEAVES THE WRITE EXACTLY WHERE IT WAS
// ===========================================================================
// That asymmetry is the whole argument, and it is why this is an argument of
// the CLUSTER-OWNER tier rather than of `public`.
//
// These rows are what a forged benchmark number would be forged into: README
// and docs/public carry published claims checked against them by
// TestPublishedClaimsRestOnAScorecardNumber. On the public tier a floor would
// have had to carry the write question too, because public guards no write at
// all -- so relaxing the read would have relaxed the write in the same stroke.
// Here the tier keeps `actor.isClusterOwner==true` as its write rule, untouched,
// and only the read widens.
//
// The write protections that actually stand in front of those rows are
// unchanged and both gated: `@serverOnly` on the two mutations
// (test/dslconformance/server_only_parsed_test.go pins them by name) and the
// internal-origin stamp the writer takes (call_origin_conformance_test.go).
// The concept's own comment already said which of those was load-bearing --
// "@serverOnly is the point rather than a precaution here".
//
// ===========================================================================
// OR-ED, NOT SUBSTITUTED
// ===========================================================================
// The same rule orRankScope follows: the tier's own term stays, so a cluster
// owner whose rank cannot be resolved keeps reading their own administrative
// rows. Replacing the term would make an owner's access depend on a second
// lookup succeeding.
//
// ===========================================================================
// THE CACHE IS SAFE BY CONSTRUCTION HERE, AND IT IS WORTH SAYING WHY
// ===========================================================================
// A declared tier injects a predicate, which sets `plan.RowAuthzInjected`, and
// planCacheSignature folds the resolved actor into the key on that flag alone
// -- "an enforced plan is actor-dependent BY CONSTRUCTION ... rather than
// relying on the injected node landing somewhere planReferencesActor happens to
// walk". So an admin's result and an ordinary user's cannot share an entry.
// That is NOT true of the public tier, which injects nothing; a read floor
// there would have needed its own plan-level refusal ahead of the cache lookup.

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// ReadFloorExpression is the injected placeholder for a cluster-owner tier's
// read floor.
//
// SYMBOLIC on purpose, exactly as RankScopeExpression is. The floor is a
// property of the ACTOR rather than of the row, so it lowers to a constant --
// and a resolved constant baked into a cached plan is one caller's answer
// serving every other. The node names the RULE; the rank is resolved at
// execution, per request.
type ReadFloorExpression struct {
	// Floor is the role slug the concept declared.
	Floor string
}

func (*ReadFloorExpression) isExpressionNode() {}

// orReadFloor ORs the read floor onto a rendered tier predicate.
func orReadFloor(base ExpressionNode, decl *langparser.RowAuthzDecl) ExpressionNode {
	if decl == nil || decl.Tier != langparser.RowAuthzClusterOwner {
		return base
	}
	floor := strings.TrimSpace(decl.RankFloor)
	if floor == "" {
		return base
	}
	node := &ReadFloorExpression{Floor: floor}
	if base == nil {
		return node
	}
	return &LogicalExpression{Op: LogicalOr, Left: base, Right: node}
}

// lowerReadFloor resolves one read-floor node against this request's actor.
//
// It answers a CONSTANT, which is correct here and would not be for the rank
// modifiers: those compare a row's owner, so they lower to an `in` list the SQL
// compiler pushes down. A floor compares nothing on the row, so there is
// nothing to push -- every row either passes or none does, and folding to a
// constant says exactly that without inventing a column to compare against.
func (e *MemQLEngine) lowerReadFloor(ctx context.Context, n *ReadFloorExpression) ExpressionNode {
	return &constantBoolExpression{value: e.readFloorAdmits(ctx, n.Floor)}
}

// readFloorAdmits is THE rule, shared by the lowering and the row gate so the
// pushed-down half and the per-row half cannot disagree about one row.
//
// FAILS CLOSED on an unresolvable slug, via rankFloorAdmits -- the natural
// spelling (`actorRank >= rankOf(slug)`) reads correctly and fails OPEN, since
// rankOf answers 0 for a slug it does not know and every rank clears 0. The
// load-time check (validateRowAuthzUnownedSlugs) is what stops such a
// declaration reaching a booted engine; this is the runtime backstop, and the
// two are deliberately not one.
func (e *MemQLEngine) readFloorAdmits(ctx context.Context, floor string) bool {
	floor = strings.TrimSpace(floor)
	if floor == "" {
		return false
	}
	ac, _ := auth.AccessFromContext(ctx)
	role := ""
	if ac != nil {
		if auth.RoleAccountScope(ac.Role) != "" {
			return false
		}
		role = string(ac.Role)
	}
	ladder := e.rankLadder(ctx)
	return rankFloorAdmits(ladder, floor, ladder.rankOf(role))
}

// readFloorAdmitsRow is the ROW-GATE half: the answer for one row already in
// hand, on the paths with no filter to push down -- a raw query string, graph
// expansion, a builtin's results, and the subscription fan-out.
//
// THE SUBSCRIPTION HALF IS NOT INCIDENTAL. These rows are broadcast
// (component/node/routing.go carries created/updated/deleted rules for both
// concepts), and row admission gates a subscribed stream through this same
// function -- so without it an admin's Benchmarks screen would be correct on
// load and frozen after, which is the shape clients/os/README.md warns about by
// name.
//
// IT READS THE ENGINE OFF THE RANK MEMO AND NEVER RESOLVES THE SCOPE. The row
// gate is a package function with no engine of its own; the memo already
// carries one, "so a consumer that has no engine of its own ... can still reach
// the SAME resolution". Taking the engine without touching `once` is the point:
// resolving the scope scans the principal table, and a floor compares the
// caller's own rank against a slug -- it has no use for who else exists.
//
// A context no entry point stamped therefore DENIES. That is the correct
// direction for a term that is OR-ed onto a gate: declining leaves exactly the
// pre-floor behaviour, which is the cluster-owner tier alone.
func readFloorAdmitsRow(ctx context.Context, decl *langparser.RowAuthzDecl) bool {
	if decl == nil || decl.Tier != langparser.RowAuthzClusterOwner {
		return false
	}
	if strings.TrimSpace(decl.RankFloor) == "" {
		return false
	}
	memo, _ := ctx.Value(rankScopeMemoKey{}).(*rankScopeMemo)
	if memo == nil || memo.engine == nil {
		return false
	}
	return memo.engine.readFloorAdmits(ctx, decl.RankFloor)
}
