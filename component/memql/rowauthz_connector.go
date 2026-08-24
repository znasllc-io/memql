package memql

// CONNECTOR ROW ADMISSION (epic memql#4378, D4).
//
// A connector is an internal writer that fills a mirror from its origin,
// or reads a MemQL-origin concept in order to push it out. It needs to
// reach rows that every declared tier would refuse it -- and it must
// reach NOTHING ELSE.
//
// THE RULE, stated once: a connector actor is admitted to a concept
// whose @origin or @mirroredTo names it, regardless of that concept's
// declared tier; and to no other concept, whatever its tier. The Shopify
// connector reads and writes shopifyProduct because shopifyProduct says
// @origin("shopify"); it cannot read a campaign, an identity, or another
// origin's mirror, and no tier anywhere grants it anything.
//
// # Why this is not the internal-origin escape
//
// rowauthz_write_guard.go already lets trusted server-side Go past a
// tier by stamping auth.ContextWithInternalOrigin for one write. That
// stamp says "the engine is doing this", which is TRUE of a connector
// and decides nothing: it would admit the Shopify connector to every
// concept in the tree. The declaration is what makes this targeted --
// the concept names the connector, so the concept decides, and a
// connector gains reach only where an author wrote its name.
//
// # Why the read side needs two seams and not one
//
// Row-authz enforcement has two mechanisms and a connector has to get
// past both:
//
//   - The ROW GATE (rowAuthzAdmits), which decides one row from its own
//     concept's declaration. Handled below by answering before the tier
//     switch runs.
//   - FILTER INJECTION (enforceRowAuthzOnPlan), which ANDs the tier's
//     predicate into the plan. That function takes no context -- ruled
//     deliberately in memql#3976 -- so it cannot know who is asking, and
//     it injects `actor.isClusterOwner==true` for a clusterOwner-tier
//     mirror like shopifyProduct. A connector is not a cluster owner, so
//     the predicate matches nothing and the connector's own reconcile
//     read returns zero rows: not an error, an EMPTY RESULT, which reads
//     exactly like "the mirror is empty" and would have the reconciler
//     conclude there is nothing to reconcile.
//
// So the injected term is removed at the later seam that does carry the
// context, using the fact that enforceRowAuthzOnPlan records precisely
// what it did (RowAuthzInjected / RowAuthzConcept) and ANDs it at the
// ROOT. Undoing a known, stamped, single-node transformation is safe in
// a way that re-deriving the tier here would not be.

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// connectorNamedByConcept reports whether the concept's data-origins
// declaration names this connector -- as the origin it mirrors, or as a
// target it propagates to.
//
// An unregistered or unknown concept answers false: a connector's reach
// comes from a declaration, and a concept the registry cannot produce
// has made none.
func connectorNamedByConcept(conceptName, connector string) bool {
	conceptName = strings.TrimSpace(conceptName)
	connector = strings.TrimSpace(connector)
	if conceptName == "" || connector == "" {
		return false
	}
	c, err := memorynodes.Get(conceptName)
	if err != nil || c == nil {
		return false
	}
	for _, name := range c.DeclaredConnectors() {
		if name == connector {
			return true
		}
	}
	return false
}

// connectorAdmission decides a row (or a plan) for a CONNECTOR actor,
// and reports whether the caller is a connector at all.
//
// The two return values are deliberately separate. "Not a connector"
// must fall through to the ordinary tier rules; "a connector this
// concept does not name" must DENY and never fall through, or a
// connector would inherit whatever the tier grants a stranger --
// which for an undeclared concept is everything.
func connectorAdmission(ctx context.Context, conceptName string) (admitted bool, isConnector bool) {
	name, ok := auth.ConnectorFromContext(ctx)
	if !ok {
		return false, false
	}
	return connectorNamedByConcept(conceptName, name), true
}

// relaxRowAuthzForConnector removes the tier predicate
// enforceRowAuthzOnPlan injected, when the caller is a connector the
// plan's concept names.
//
// It is a no-op for every other caller and for every plan that carried
// no injection, so the ordinary path is untouched.
//
// The removal is safe because it undoes a transformation this package
// performed and recorded: enforceRowAuthzOnPlan sets RowAuthzInjected,
// records the concept, and ANDs the predicate as the RIGHT child of a
// new root whose LEFT child is the plan the author wrote. Restoring
// that left child is the exact inverse. Anything else -- rewriting the
// predicate, or re-deriving the tier here -- would be a second opinion
// about the declaration, which is what this file's header refuses to be.
func relaxRowAuthzForConnector(ctx context.Context, plan *QueryPlan) {
	if plan == nil || !plan.RowAuthzInjected {
		return
	}
	admitted, isConnector := connectorAdmission(ctx, plan.RowAuthzConcept)
	if !isConnector || !admitted {
		// A connector the concept does NOT name keeps the injected term
		// and is refused by it, exactly like any other caller who fails
		// the tier. There is nothing to relax and nothing to widen.
		return
	}
	root, ok := plan.Root.(*LogicalExpression)
	if !ok || root == nil || root.Op != LogicalAnd || root.Left == nil {
		// The shape enforceRowAuthzOnPlan builds is not there. Leave the
		// plan alone: a connector reading nothing is a visible failure,
		// while a plan this function guessed at is a silent one.
		return
	}
	plan.Root = root.Left
	plan.RowAuthzRelaxedForConnector = true
	// RowAuthzInjected and RowAuthzConcept are DELIBERATELY LEFT SET.
	//
	// The name says "a predicate was ANDed into Root", which is no longer
	// literally true -- but the field's job is the RESULT-CACHE KEY
	// (engine_types.go says so): it is what folds the caller identity
	// into planCacheSignature. Clearing it here would drop the actor from
	// the signature at the exact moment the plan became actor-dependent
	// in the most dangerous way available, because canonicalExpression of
	// the relaxed root is now BYTE-IDENTICAL to the plan an ordinary
	// caller writes over the same concept. The connector's tier-free
	// result would be cached under that signature and served to the next
	// caller -- every gate correct, a whole mirror published anyway. It
	// is the memql#4040 collision shape exactly.
	//
	// planCacheSignature also reads RowAuthzRelaxedForConnector, so the
	// property survives someone later "tidying" this field to false.
}
