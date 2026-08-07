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
//     the ONLY mechanism available to graph expansion, which reaches
//     rows through relationship definitions and has no filter to AND
//     anything into.
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
func cloneRowAuthzPredicate(expr ExpressionNode) ExpressionNode {
	cmp, ok := expr.(*ComparisonExpression)
	if !ok {
		return expr
	}
	copied := *cmp
	return &copied
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
			return rowAuthzDeny
		}
		if strings.TrimSpace(decl.Owner) == langparser.RowAuthzSelfOwnedField {
			// SELF-OWNED (memql#3029): the row IS the owner, so the
			// comparison is against the row's own identity. Ids are stored
			// canonically (`<concept>:<shortId>`) while a caller id is the
			// bare short form, so both spellings are accepted -- the id
			// contract says clients never compose canonical ids.
			short := id
			if idx := strings.LastIndex(id, ":"); idx >= 0 {
				short = id[idx+1:]
			}
			if id == caller || short == caller {
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
		if owner == caller {
			return rowAuthzAdmit
		}
		return rowAuthzDeny

	default:
		// An unknown tier is a broken declaration, not a permission.
		return rowAuthzDeny
	}
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
