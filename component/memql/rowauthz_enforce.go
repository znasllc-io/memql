package memql

import (
	"fmt"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowauthz_enforce.go -- memql#3076, Phase 3 of memql#2803.
//
// Shadow mode (rowauthz_shadow.go) computes the predicate a declared tier WOULD
// inject and throws it away. This file applies it: for a read whose bound
// concept declares `@rowAuthz`, the tier's predicate is ANDed into the filter
// before the expression is resolved and compiled.
//
// # One detector, three consumers
//
// The predicate now has three renderings, and they must agree or the gate lies:
//
//	InjectedPredicate      -- TEXT, for the shadow report
//	isOwnerScopeLeaf /
//	  isClusterOwnerLeaf   -- MATCHER, "does the author's filter already say it"
//	enforcedPredicateNode  -- NODE, what actually gets ANDed in (this file)
//
// They are written against each other deliberately: the node this file builds
// is exactly the node those matchers accept, so `AnalyzeShadow` reporting
// already-implied and this function injecting a duplicate term are the same
// statement. A test pins that round-trip rather than leaving it to inspection,
// because a matcher looser than the enforcer would credit a scope that is never
// applied -- the failure the shadow file's own doctrine calls out.
//
// # Scope: READ PATH ONLY
//
// Enforcement here means "you cannot READ a row whose declared owner is not
// you". It does NOT mean "nobody can WRITE a row claiming you own it" --
// memql#3059 (raw `insert()` bypasses accept/stamp) is the write-side
// counterpart and is independently open. The tier reading as a complete
// guarantee is the failure mode this epic keeps circling, so it is stated here
// as well as in the commit.

// enforcedPredicateNode builds the expression a tier ANDs into every read of
// the concept, or nil when the tier injects nothing.
//
// The error return is the fail-CLOSED arm: an unrecognised tier means the
// engine cannot say what the author declared, and the safe answer for an
// authorization predicate is to refuse the read rather than to serve it
// unfiltered. A tier can only get here by being added to the language without
// being added to this switch, which is a build-time mistake, not a runtime
// condition -- so failing the query is both safe and loud.
func enforcedPredicateNode(decl *langparser.RowAuthzDecl) (ExpressionNode, error) {
	if decl == nil {
		return nil, nil
	}
	switch decl.Tier {
	case langparser.RowAuthzPublic:
		// Declared globally readable. Injects nothing, explicitly -- the one
		// tier where "no predicate" is the declaration rather than a gap.
		return nil, nil

	case langparser.RowAuthzOwned:
		// SELF-OWNED (memql#3029) names the ROW'S OWN identity, so the leaf is
		// the `row.id` intrinsic and not a payload property. Getting this wrong
		// silently compiles to a different SQL path -- a table column versus a
		// JSONB path -- which is why InjectedPredicate spells it `row.id` and
		// isOwnerScopeLeaf matches it with isRowIdName.
		if strings.TrimSpace(decl.Owner) == langparser.RowAuthzSelfOwnedField {
			return &ComparisonExpression{
				Field:    FieldReference{Raw: "row.id", Parts: []string{"row", "id"}},
				Operator: OpEq,
				Value:    &ActorReference{Path: "userId"},
			}, nil
		}
		owner := strings.TrimSpace(decl.Owner)
		if owner == "" {
			return nil, fmt.Errorf("rowAuthz: owned tier declares no owner field")
		}
		return &ComparisonExpression{
			Field:    FieldReference{Raw: owner, Parts: []string{owner}},
			Operator: OpEq,
			Value:    &ActorReference{Path: "userId"},
		}, nil

	case langparser.RowAuthzClusterOwner:
		return &ComparisonExpression{
			Field:    FieldReference{Raw: "actor.isClusterOwner", Parts: []string{"actor", "isClusterOwner"}},
			Operator: OpEq,
			Value:    true,
		}, nil

	case langparser.RowAuthzGranted:
		spec := strings.TrimSpace(decl.Spec)
		if spec == "" {
			return nil, fmt.Errorf("rowAuthz: granted tier declares no spec")
		}
		// The spec reference is injected as-is. Whether an arbitrary filter
		// IMPLIES it is undecidable without relationship semantics (Phase 4) --
		// but injecting it needs no such analysis: ANDing a predicate is sound
		// whether or not the filter already carries it.
		return &SpecReferenceExpression{Name: spec}, nil

	default:
		return nil, fmt.Errorf(
			"rowAuthz: unrecognised tier %q on the read path -- refusing the read rather than "+
				"serving it unfiltered (memql#3076)", decl.Tier)
	}
}

// enforceRowAuthzFilter returns expr with the bound concept's declared tier
// ANDed in, or expr unchanged when the concept declares nothing.
//
// expr must be the expression BEFORE resolveActorReferences runs, for the same
// reason AnalyzeShadow requires it: the injected `actor.userId` is an
// ActorReference that the resolver substitutes on the very next step. Injecting
// after resolution would leave an unresolved reference in the compiled filter.
func enforceRowAuthzFilter(expr ExpressionNode) (ExpressionNode, error) {
	if expr == nil {
		return expr, nil
	}
	decl := rowAuthzDeclFor(extractConceptFromExpression(expr))
	if decl == nil {
		return expr, nil
	}
	predicate, err := enforcedPredicateNode(decl)
	if err != nil {
		return nil, err
	}
	if predicate == nil {
		return expr, nil
	}
	return andIntoFilter(expr, predicate)
}

// andIntoFilter ANDs predicate into the FILTER of a read pipeline.
//
// A loaded query's expression is the whole pipeline --
// shape(paginate(sort(<filter>))) -- not the filter, which is why
// AnalyzeShadow calls unwrapToFilter before analysing. Wrapping the outermost
// node instead would AND the predicate into a projection rather than a
// selection: at best a compile error, at worst a filter that does not filter.
//
// So this walks to the filter the same way the analyzer does and rebuilds the
// wrappers around the conjunction.
// The wrapper set is EXACTLY unwrapToFilter's, and it has to stay exactly
// unwrapToFilter's: a wrapper this function does not know is one the analyzer
// looks through and the enforcer ANDs into, so the term would land in a
// projection or an ordering rather than a selection. TestEnforcerAndAnalyzer-
// AgreeOnWrappers pins the two lists against each other.
func andIntoFilter(expr, predicate ExpressionNode) (ExpressionNode, error) {
	descend := func(target ExpressionNode, rebuild func(ExpressionNode) ExpressionNode) (ExpressionNode, error) {
		inner, err := andIntoFilter(target, predicate)
		if err != nil {
			return nil, err
		}
		return rebuild(inner), nil
	}
	switch n := expr.(type) {
	case *ShapeExpression:
		return descend(n.Target, func(i ExpressionNode) ExpressionNode { out := *n; out.Target = i; return &out })
	case *PaginateExpression:
		return descend(n.Target, func(i ExpressionNode) ExpressionNode { out := *n; out.Target = i; return &out })
	case *SortExpression:
		return descend(n.Target, func(i ExpressionNode) ExpressionNode { out := *n; out.Target = i; return &out })
	case *SelectExpression:
		return descend(n.Target, func(i ExpressionNode) ExpressionNode { out := *n; out.Target = i; return &out })
	case *TimestampExpression:
		return descend(n.Target, func(i ExpressionNode) ExpressionNode { out := *n; out.Target = i; return &out })
	case *DepthExpression:
		return descend(n.Target, func(i ExpressionNode) ExpressionNode { out := *n; out.Target = i; return &out })
	default:
		return &LogicalExpression{Op: LogicalAnd, Left: expr, Right: predicate}, nil
	}
}
