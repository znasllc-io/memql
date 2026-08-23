package memql

// Row-authz ENFORCEMENT on the READ path (memql#3172, Phase 3 of the
// #2803 ruling).
//
// Phase 1 (#2920) made each concept DECLARE who may see its rows.
// Phase 2 (#2921) computed the predicate that declaration WOULD inject
// and threw it away. This file ANDs it in.
//
// SCOPE IS THE READ PATH. The write side is its own file:
// rowauthz_write_guard.go landed the update/delete owner match
// (memql#3174), which resolves the target row in executeWrite and
// refuses when its declared owner is not the actor. It shares this
// file's rowAuthzAdmits, so "who owns this row" has one answer in both
// directions. Still open on the write side: the raw `insert(`
// short-circuit that reaches the store without passing the mutation
// template can forge the owner field on a CREATE (memql#3059 / #3175),
// and the boot seeder writes past executeWrite entirely (#3176).
//
// Two mechanisms, and they answer different questions.
//
//  1. FILTER INJECTION, resolved from the construct's DECLARED BINDING
//     (plan.BoundConcept). The tier's predicate is ANDed into the plan
//     root before the read runs, so the narrowing pushes down into SQL.
//     This is the mechanism for every named construct.
//
//  2. ROW ADMISSION, resolved from THE ROW'S OWN concept. Every row on
//     its way out of the engine is checked against the tier its own
//     concept declares. This is the mechanism for reads that have no
//     declared binding to resolve from -- a raw client-supplied query
//     string (`handleExecuteQuery` passes one straight through) -- and
//     the ONLY mechanism available to two paths with no filter to AND
//     anything into: graph expansion, which reaches rows through
//     relationship definitions, and a TOP-LEVEL BUILTIN CALL, whose
//     rows come out of a Go handler rather than a query (memql#3982).
//
// Neither mechanism reads the filter to decide whether to engage. That
// is the whole of memql#3172 finding 1: the refused implementation
// asked `extractConceptFromExpression`, which answers "" for anything
// that is not a top-level `concept==<id>` equality, so naming a row by
// id, a top-level `||`, or a negated concept each turned enforcement
// off -- from a raw query string AND from authored DSL, since an
// authored `filter a || b` lowers to a top-level OR.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowAuthzPredicateCache memoises the parsed form of a rendered
// predicate. The parse is pure and the rendered set is tiny (one entry
// per declared tier), so this keeps enforcement off the per-query parse
// budget without introducing a second spelling of the predicate.
var rowAuthzPredicateCache sync.Map // string -> ExpressionNode

// rowAuthzPredicateExpr builds the expression a tier ANDs into a read.
//
// It parses the string InjectedPredicate renders rather than hand-
// building the AST, so the term the engine enforces is literally the
// term shadow mode reports and the term an author would have typed.
// Two spellings of one predicate is how a renderer and a matcher drift
// into disagreeing about the very thing they both exist to describe --
// the memql#3029 near-miss this file refuses to repeat.
//
// Returns (nil, nil) for the `public` tier, which injects nothing by
// declaration. Every other unusable declaration is an ERROR rather than
// a silent skip: a tier that cannot be turned into a predicate is a
// broken authorization statement, and the read must refuse rather than
// proceed unenforced.
func rowAuthzPredicateExpr(decl *langparser.RowAuthzDecl) (ExpressionNode, error) {
	if decl == nil {
		return nil, nil
	}
	switch decl.Tier {
	case langparser.RowAuthzPublic:
		return nil, nil

	case langparser.RowAuthzGranted:
		spec := strings.TrimSpace(decl.Spec)
		if spec == "" {
			return nil, fmt.Errorf("row-authz: the granted tier names no spec, so there is no predicate to enforce")
		}
		// Built rather than parsed: a bare identifier does not parse as a
		// filter, and the engine's evaluator already expands a
		// SpecReferenceExpression through the spec registry.
		return &SpecReferenceExpression{Name: spec}, nil

	case langparser.RowAuthzOwned:
		if strings.TrimSpace(decl.Owner) == "" {
			return nil, fmt.Errorf("row-authz: the owned tier names no owner field, so there is no predicate to enforce")
		}
	case langparser.RowAuthzClusterOwner:
	default:
		return nil, fmt.Errorf("row-authz: unknown tier %q", decl.Tier)
	}

	rendered := InjectedPredicate(decl)
	if strings.TrimSpace(rendered) == "" {
		return nil, fmt.Errorf("row-authz: tier %q rendered no predicate", decl.Tier)
	}
	if cached, ok := rowAuthzPredicateCache.Load(rendered); ok {
		return cloneRowAuthzPredicate(cached.(ExpressionNode)), nil
	}
	parsed, err := parseViaLangparser(rendered, false)
	if err != nil {
		return nil, fmt.Errorf("row-authz: predicate %q does not parse: %w", rendered, err)
	}
	if parsed.Root == nil {
		return nil, fmt.Errorf("row-authz: predicate %q parsed to nothing", rendered)
	}
	rowAuthzPredicateCache.Store(rendered, parsed.Root)
	return cloneRowAuthzPredicate(parsed.Root), nil
}

// cloneRowAuthzPredicate copies the cached node so a caller that mutates
// the injected term in place -- rewriteFilterFieldRefs does exactly that,
// turning the bare `ownerUserId` into `payload.ownerUserId` -- cannot
// corrupt the cache entry every later query is handed.
//
// It RECURSES, and that is not a tidy-up. Until the composite tier
// (memql#4312) every rendered predicate was a single comparison, so a
// one-level copy was a complete copy and the shape of the function said
// so. The composite renders a disjunction; a one-level copy of it copies
// the LogicalExpression and shares both branches, so the first query to
// rewrite `requestedBy` -> `payload.requestedBy` would rewrite it inside
// the cache -- and the second query over the concept would get a term
// reading `payload.payload.requestedBy`. Silent, and it narrows to
// nothing rather than failing.
func cloneRowAuthzPredicate(expr ExpressionNode) ExpressionNode {
	switch n := expr.(type) {
	case *ComparisonExpression:
		copied := *n
		return &copied
	case *LogicalExpression:
		copied := *n
		copied.Left = cloneRowAuthzPredicate(n.Left)
		copied.Right = cloneRowAuthzPredicate(n.Right)
		return &copied
	default:
		// A SpecReferenceExpression (the granted tier) carries a name and
		// nothing mutable; anything else is a node this renderer does not
		// produce. Returning it as-is preserves today's behaviour for both.
		return expr
	}
}

// stampRowAuthzConcept marks every comparison in an injected term with the
// concept whose declaration produced it.
//
// One stamp per comparison, because the composite tier's term has TWO --
// the owner equality and the admin gate -- and the canonicalize-RHS pass
// runs over whichever it finds. See ComparisonExpression.RowAuthzConcept
// for why an unstamped injected term matches nothing at all.
func stampRowAuthzConcept(expr ExpressionNode, conceptName string) {
	switch n := expr.(type) {
	case *ComparisonExpression:
		n.RowAuthzConcept = conceptName
	case *LogicalExpression:
		stampRowAuthzConcept(n.Left, conceptName)
		stampRowAuthzConcept(n.Right, conceptName)
	}
}

// enforceRowAuthzOnPlan ANDs the plan's declared tier into its root.
//
// Resolution is from plan.BoundConcept. A plan with no bound concept --
// a raw query string, an inline expression -- is left alone HERE and
// enforced per-row instead (rowAuthzAdmits); there is no filter-text
// fallback, deliberately.
//
// Precedence: the predicate is ANDed at the ROOT, so an author's
// `a || b` becomes `((a) || (b)) && (authz)` rather than binding to the
// last disjunct.
func enforceRowAuthzOnPlan(plan *QueryPlan) error {
	if plan == nil || plan.Root == nil {
		return nil
	}
	conceptName := strings.TrimSpace(plan.BoundConcept)
	if conceptName == "" {
		return nil
	}
	decl := rowAuthzDeclFor(conceptName)
	if decl == nil {
		return nil
	}
	predicate, err := rowAuthzPredicateExpr(decl)
	if err != nil {
		return fmt.Errorf("query over %s: %w", conceptName, err)
	}
	if predicate == nil {
		// The public tier, and only the public tier.
		return nil
	}
	// Stamp the concept ON the injected node so the canonicalize-RHS pass
	// resolves it from the DECLARATION too (memql#3172). Without this the
	// pass falls back to extractConceptFromExpression, which answers ""
	// for every spelling this task exists to cover, and the owner field
	// is an @relationship -- so the term would compare the bare
	// `actor.userId` against a canonical stored `v1:identity:user:<id>`
	// and match nothing at all.
	stampRowAuthzConcept(predicate, conceptName)
	plan.Root = &LogicalExpression{Op: LogicalAnd, Left: plan.Root, Right: predicate}
	plan.RowAuthzInjected = true
	plan.RowAuthzConcept = conceptName
	return nil
}

// refuseRowAuthzWithoutActor refuses a read whose injected term compares
// against a caller identity that is not there.
//
// memql#3172 finding 4. An `ownerUserId == <empty string>` term is a
// legal predicate that MATCHES: any row stored with an empty owner comes
// back, and an unauthenticated caller is handed rows on what is supposed
// to be an ownership gate. The
// self-owned spelling (`owner="id"`) fails closed for free, since no row
// carries an empty id -- so the two forms of one tier disagreed about
// what happens to an anonymous caller. They agree now, by refusing.
//
// The refusal is an ERROR rather than an empty result on purpose: an
// empty result is indistinguishable from "you own nothing", which is
// the answer a caller who never authenticated should not be given.
func refuseRowAuthzWithoutActor(ctx context.Context, plan *QueryPlan) error {
	if plan == nil || !plan.RowAuthzInjected {
		return nil
	}
	decl := rowAuthzDeclFor(plan.RowAuthzConcept)
	if decl == nil || decl.Tier != langparser.RowAuthzOwned {
		return nil
	}
	if strings.TrimSpace(rowAuthzActorUserId(ctx)) != "" {
		return nil
	}
	return fmt.Errorf(
		"row-authz: %s declares the owned tier (%s) and this read carries no caller identity, "+
			"so the injected term would compare against the empty string and return rows owned "+
			"by nobody. Refused (memql#3172). Authenticate the call, or read the concept from a "+
			"context carrying an AccessContext",
		plan.RowAuthzConcept, InjectedPredicate(decl))
}

// rowAuthzActorUserId reads the caller's user id off the request
// context through the canonical envelope, so this file cannot drift
// from the four other surfaces that resolve `actor.userId`.
func rowAuthzActorUserId(ctx context.Context) string {
	ac, _ := auth.AccessFromContext(ctx)
	v, ok := auth.ActorEnvelopeValue(ac, "userId")
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// rowAuthzIsClusterOwner reads the caller's cluster-owner bit through
// the same envelope. A missing AccessContext DENIES (memql#2801).
func rowAuthzIsClusterOwner(ctx context.Context) bool {
	ac, _ := auth.AccessFromContext(ctx)
	v, ok := auth.ActorEnvelopeValue(ac, "isClusterOwner")
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// rowAuthzAdmission is what the row gate concluded about one row.
type rowAuthzAdmission int

const (
	// rowAuthzAdmit -- the row's concept declares nothing, declares
	// public, or declares a tier this caller satisfies.
	rowAuthzAdmit rowAuthzAdmission = iota
	// rowAuthzDeny -- the row's concept declares a tier this caller
	// does not satisfy.
	rowAuthzDeny
	// rowAuthzUndecided -- the tier cannot be decided against a single
	// row in isolation. Today that is the `granted` tier alone, whose
	// predicate is a relationship spec: deciding it needs the join the
	// filter performs, and a row on its own does not carry the answer.
	rowAuthzUndecided
)

// rowAuthzAdmits decides whether one row may be returned to the caller,
// from THE ROW'S OWN concept declaration.
//
// This is the half of enforcement that cannot be steered by how the
// filter was spelled, because it never looks at the filter. It is what
// covers a raw query string (which has no declared binding to resolve a
// tier from) and graph expansion (which has no filter at all).
func rowAuthzAdmits(ctx context.Context, conceptName string, id string, payload []byte) rowAuthzAdmission {
	decl := rowAuthzDeclFor(conceptName)
	if decl == nil {
		// UNDECLARED. Not "safe" and not "unchanged" -- unmeasured, in the
		// undeclared gate's own words. Admitting unconditionally is fine
		// for a concept whose rows carry nothing personal; it is how
		// memql#3350's generic-browse hole over v1:identity:user's eight
		// @pii fields existed. One narrowing, on the unbound read path
		// only: rowauthz_pii_unbound.go.
		if rowAuthzPIIUnboundDenies(ctx, conceptName, id) {
			return rowAuthzDeny
		}
		return rowAuthzAdmit
	}
	switch decl.Tier {
	case langparser.RowAuthzPublic:
		return rowAuthzAdmit

	case langparser.RowAuthzClusterOwner:
		if rowAuthzIsClusterOwner(ctx) {
			return rowAuthzAdmit
		}
		return rowAuthzDeny

	case langparser.RowAuthzGranted:
		return rowAuthzUndecided

	case langparser.RowAuthzOwned:
		caller := strings.TrimSpace(rowAuthzActorUserId(ctx))
		if caller == "" {
			// Finding 4 again, on the row side: no identity, no rows.
			//
			// CHECKED BEFORE the composite bypass below, deliberately. A
			// caller with no access context resolves isClusterOwner as
			// false (memql#2801), so the order does not change the answer
			// today -- but "no identity, no rows" is the stronger of the
			// two statements and reading it first is what keeps it true if
			// the envelope ever learns to answer differently.
			return rowAuthzDeny
		}
		// The COMPOSITE tier (memql#4312), `owner="<field>", clusterOwner`:
		// the owner, OR a cluster owner. Checked before the owner
		// comparison because the admin branch does not read the owner field
		// at all -- a row that cannot say who owns it is still an
		// administrable row.
		if decl.ClusterOwnerBypass && rowAuthzIsClusterOwner(ctx) {
			return rowAuthzAdmit
		}
		if strings.TrimSpace(decl.Owner) == langparser.RowAuthzSelfOwnedField {
			// SELF-OWNED (memql#3029): the row IS the owner, so the
			// comparison is against the row's own identity.
			if sameRowAuthzOwner(id, caller) {
				return rowAuthzAdmit
			}
			return rowAuthzDeny
		}
		owner, ok := rowAuthzOwnerValue(payload, decl.Owner)
		if !ok {
			// The declared owner field is absent from the row. Deny: a row
			// that cannot say who owns it is not a row this caller can be
			// shown to own.
			return rowAuthzDeny
		}
		if sameRowAuthzOwner(owner, caller) {
			return rowAuthzAdmit
		}
		return rowAuthzDeny

	default:
		// An unknown tier is a broken declaration, not a permission.
		return rowAuthzDeny
	}
}

// sameRowAuthzOwner reports whether two id spellings name the same
// owner. THE single answer to "is this row this caller's", shared by
// every in-Go row-authz comparison: the read path's row gate, the
// traversal gate, and -- through rowAuthzAdmits -- #3174's write guard.
//
// WHY IT CANNOT BE `==` (memql#3172, caught by the DB lane). An owner
// field is an outgoing `@relationship`, so executeWrite's
// canonicalizeRelationshipFields rewrites it to canonical
// `{concept}:{shortId}` form before the row is stored: a note created by
// caller `u1` stores `ownerUserId: "v1:identity:user:u1"`. The caller
// identity on the other side of the comparison is the BARE `u1`, which
// is what the actor envelope carries and what
// docs/public/concepts/identifiers.md says every client speaks. A raw
// `==` between those two is false for every row, so the gate denied the
// owner their own rows -- 13 concepts reading back empty for everyone.
// Local suites missed it because they seeded both sides with the same
// spelling; only a real insert applies the canonicalize step.
//
// The normalization is BareShortId, the repo's one outbound id
// primitive (#2441): it strips a canonical prefix and is a no-op on a
// value that has none, so a bare/bare, canonical/canonical, or mixed
// pair all collapse onto the same answer. Applied once per side, never
// to its own output (BareShortId is deliberately not idempotent,
// memql#2981).
//
// It does not use canonicalizeRelationshipFieldValue -- the engine-side
// canonicalizer the FILTER path uses -- because that needs the engine's
// concept registry and a target concept, and these callers are handed a
// row rather than a query. Comparing bare tails needs neither and gives
// the same answer: the SQL path canonicalizes the RHS so the column and
// the value match in the database; the Go path strips both to the id
// underneath. Stated here so the split is a decision rather than a
// coincidence.
func sameRowAuthzOwner(stored, caller string) bool {
	stored = strings.TrimSpace(stored)
	caller = strings.TrimSpace(caller)
	if stored == "" || caller == "" {
		// An empty owner never matches, and a caller with no identity is
		// refused upstream (finding 4). Neither is an ownership claim.
		return false
	}
	if stored == caller {
		return true
	}
	return BareShortId(stored) == BareShortId(caller)
}

// rowAuthzOwnerValue pulls the declared owner field off a stored
// payload. Only a TOP-LEVEL property counts, matching the analyzer's
// topLevelPayloadField: a nested key that happens to share the name is
// a different field.
func rowAuthzOwnerValue(payload []byte, ownerField string) (string, bool) {
	if len(payload) == 0 {
		return "", false
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", false
	}
	raw, ok := decoded[strings.TrimSpace(ownerField)]
	if !ok {
		return "", false
	}
	s, isString := raw.(string)
	if !isString {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// admitRowAuthzNode applies the row gate to a fetched row on a FILTERED
// read.
//
// `granted` is admitted here and denied on the traversal path, and the
// asymmetry is the point: on this path a filter ran, and for a bound
// construct that filter now carries the tier's spec as a top-level
// conjunct, so the join has already been performed. On the traversal
// path nothing ran, so there is nothing to defer to.
func admitRowAuthzNode(ctx context.Context, node memorynodes.MemoryNode) bool {
	return rowAuthzAdmits(ctx, node.Concept, node.ID, node.Payload) != rowAuthzDeny
}

// admitRowAuthzTraversal applies the row gate to a row reached by graph
// expansion, where there is no filter to have narrowed anything.
// Undecidable fails closed here for that reason.
func admitRowAuthzTraversal(ctx context.Context, node memorynodes.MemoryNode) bool {
	return rowAuthzAdmits(ctx, node.Concept, node.ID, node.Payload) == rowAuthzAdmit
}

// admitRowAuthzBuiltinResult applies the row gate to a row handed back by
// a TOP-LEVEL builtin call (memql#3982).
//
// THE SEAM. A Logic whose whole body is `return <builtin>({...})` resolves
// to a bare *BuiltinFunctionExpression at plan.Root, and executeWith
// dispatches that straight to evaluateBuiltinFunctionExpression and returns.
// That short-circuit stepped around BOTH mechanisms this file implements:
//
//   - FILTER INJECTION never engaged, and arguably could not have.
//     enforceRowAuthzOnPlan resolves the tier from plan.BoundConcept, and a
//     builtin call binds no concept -- there is nothing for the predicate to
//     bind to, and no filter to AND it into.
//   - ROW ADMISSION never engaged either, and that is the actual defect.
//     filterRowAuthzSet is applied inside evaluateExpressionSet, which this
//     branch returns BEFORE reaching. The NESTED spelling of the same call
//     (executor.go's `case *BuiltinFunctionExpression`, reached through
//     evaluateExpressionSetWithContext) is covered, because its rows come
//     back up through evaluateExpressionSet's gate. So one builtin call was
//     gated and the identical call one syntactic level up was not.
//
// Rows produced this way are not always synthetic. Most builtins return a
// result envelope under a made-up concept (`memql:validate`,
// `integration:email:send`), which declares no tier and is unaffected. But
// an integration capability is a builtin too, and several of them run SQL
// against the node store and hand back the rows they read, carrying the
// row's REAL concept and REAL payload -- `integration.embedding.findSimilar`
// scans the concept out of the query, `integration.harnessRecall.recall`
// takes it from caller args. Those are graph rows of a declared concept
// reaching a caller having passed no per-row authorization at all.
//
// UNDECIDABLE FAILS CLOSED, matching admitRowAuthzTraversal rather than
// admitRowAuthzNode, and the reason is the same one that splits those two.
// admitRowAuthzNode admits the `granted` tier because on the filtered path
// a filter ran and, for a bound construct, that filter now carries the
// tier's spec as a top-level conjunct -- the join has already happened.
// Here nothing ran: no filter, no injection, no join. There is nothing to
// defer to, so a tier whose predicate is a relationship spec cannot be
// satisfied on the strength of a row in isolation and must not be.
//
// A ROW WITH AN EMPTY CONCEPT IS ADMITTED, and that is a decision rather
// than an oversight. rowAuthzDeclFor("") answers nil, so such a row lands
// in rowAuthzAdmits' UNDECLARED branch -- which this file already argues at
// length is "not safe and not unchanged -- unmeasured", and admits. Denying
// it HERE would give the same row two different answers depending on which
// seam it left through, with nothing to say which is right; the traversal
// gate, whose semantics this one copies, admits it too. It also costs
// nothing against the hole being closed: a row with no concept is by
// definition not a row of a concept that declared a tier. If concept-less
// rows should be denied, that is a change to rowAuthzAdmits' undeclared
// branch and applies to every seam at once -- not a local override here.
func admitRowAuthzBuiltinResult(ctx context.Context, node memorynodes.MemoryNode) bool {
	return rowAuthzAdmits(ctx, node.Concept, node.ID, node.Payload) == rowAuthzAdmit
}

// filterRowAuthzBuiltinNodes drops every row of a top-level builtin's
// result the caller may not see.
//
// It filters the SLICE, before nodesToMap, rather than the map afterwards.
// The two are equivalent today -- nodesToMap only dedups by id and drops
// blank ids -- but gating the handler's OWN output means the gate cannot be
// routed around by a later change to how that output is keyed. Returns the
// slice unchanged when nothing was denied, so the common case (no declared
// concept in the result, which is every synthetic result envelope)
// allocates nothing.
func filterRowAuthzBuiltinNodes(ctx context.Context, nodes []memorynodes.MemoryNode) []memorynodes.MemoryNode {
	if len(nodes) == 0 {
		return nodes
	}
	denied := 0
	for _, node := range nodes {
		if !admitRowAuthzBuiltinResult(ctx, node) {
			denied++
		}
	}
	if denied == 0 {
		return nodes
	}
	admitted := make([]memorynodes.MemoryNode, 0, len(nodes)-denied)
	for _, node := range nodes {
		if admitRowAuthzBuiltinResult(ctx, node) {
			admitted = append(admitted, node)
		}
	}
	return admitted
}

// filterRowAuthzSet drops every row of the fetched set the caller may
// not see. Returns the set unchanged when nothing was denied, so the
// common case (no declared concept in the result) allocates nothing.
func filterRowAuthzSet(ctx context.Context, set map[string]memorynodes.MemoryNode) map[string]memorynodes.MemoryNode {
	if len(set) == 0 {
		return set
	}
	var denied []string
	for key, node := range set {
		if !admitRowAuthzNode(ctx, node) {
			denied = append(denied, key)
		}
	}
	if len(denied) == 0 {
		return set
	}
	for _, key := range denied {
		delete(set, key)
	}
	return set
}
