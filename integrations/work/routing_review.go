package work

// routing_review.go -- the nightly routing review, seen and decided by a
// person (epic memql#5146 D5; design record
// docs/superpowers/specs/2026-11-08-routing-review-apply-design.md).
//
// THE ONE APPROVAL NOBODY OWNS. The evidence fold raises a routingReview as
// the cluster's maintenance principal, and the engine blanks a synthetic
// actor's owner stamp (memql#4817), so the row is the deployment's. Every
// other approval read in this package resolves through
// `row.ownerUserId == actor.userId` -- the person's own -- and until this file
// existed nobody could list a routing review or decide one.
//
// WHO MAY: an owner or developer, the people routingRuleSave admits
// (memql.RoutingConfigurationAuthor). The read runs under the system actor
// (the deployment reading its own row), and ONLY after that check, because the
// row tier would admit a cluster owner and refuse a developer, and the
// decision D1 is that both decide.
//
// WHY THE RESULT IS A PROJECTION. The engine gates every builtin's result
// through the caller's row admission (filterRowAuthzBuiltinNodes), so a real
// `v1:work:approval` row nobody owns would reach an owner and be stripped for
// a developer. The handler returns the same fields under routingReviewConcept,
// a projection no row tier is declared on -- the pattern routingRules uses for
// v1:router:ruleCatalog -- and its own author check is the gate. Which rows can
// come back is fixed by the query: routing reviews and nothing else.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// routingReviewConcept is the projection a routing review is returned under.
// Never persisted; see the header for why it is not v1:work:approval.
const routingReviewConcept = "v1:work:routingReview"

// routingReviewActor names the deployment acting on its own routing reviews:
// the cluster read here, and the decision write.
const routingReviewActor = "routingReview"

// routingReviews reads routing reviews: every pending one, or one by id
// whatever its decision. The CALLER must already have passed
// memql.RoutingConfigurationAuthor; this reads as the deployment.
func (i *Integration) routingReviews(ctx context.Context, approvalId string) ([]map[string]any, error) {
	return i.store().routingReviewsForCluster(auth.ContextWithSystemActor(memql.ContextWithFreshRead(ctx), routingReviewActor), approvalId)
}

// handleRoutingReviews serves workRoutingReviews.
//
// A signed-in person who may not change routing gets NO ROWS rather than an
// error: they have no routing review to decide, and an inbox asking on their
// behalf must not fail for it. An anonymous, connector or synthetic caller is
// refused before anything is read.
func (i *Integration) handleRoutingReviews(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if ac.Synthetic {
		return nil, fmt.Errorf("work: routing reviews are decided by a person, not by a job")
	}
	if _, err := memql.RoutingConfigurationAuthor(ctx); err != nil {
		return nil, nil
	}
	rows, err := i.routingReviews(ctx, argString(args, "approvalId"))
	if err != nil {
		return nil, err
	}
	nodes := make([]memorynodes.MemoryNode, 0, len(rows))
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("work: encode routing review %s: %w", rowString(row, "id"), err)
		}
		nodes = append(nodes, memorynodes.MemoryNode{
			ID:      rowString(row, "id"),
			Concept: routingReviewConcept,
			Type:    memorynodes.NodeTypeObject,
			Payload: raw,
		})
	}
	return nodes, nil
}
