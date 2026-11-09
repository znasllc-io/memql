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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	routerlib "github.com/znasllc-io/memql/component/router"
	work "github.com/znasllc-io/memql/component/work"
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

// routingReviewApplier is the routing configuration's half of a decision
// (component/memql's ApplyRoutingReview), found on the engine by assertion
// the way this package finds its other optional engine features.
type routingReviewApplier interface {
	ApplyRoutingReview(context.Context, memql.RoutingReviewChange) (memql.RoutingReviewApplied, error)
}

// pendingRoutingReview is the undecided routing review approvalId names, for
// a caller who may decide it, or nil. A caller who may not change routing gets
// nil, so they meet the ordinary "no pending approval" answer and learn
// nothing about whether the id exists.
func (i *Integration) pendingRoutingReview(ctx context.Context, approvalId string) (map[string]any, error) {
	if _, err := memql.RoutingConfigurationAuthor(ctx); err != nil {
		return nil, nil
	}
	rows, err := i.routingReviews(ctx, approvalId)
	if err != nil || len(rows) != 1 || rowString(rows[0], "decision") != "" {
		return nil, err
	}
	return rows[0], nil
}

// recoverRoutingReviewDecision answers a decision that has already landed
// with the one recorded, so a retried click or a second person deciding the
// same review is told what happened rather than that the review is gone. It
// applies nothing: the change landed with the decision, or the decision was a
// decline that changed nothing.
func (i *Integration) recoverRoutingReviewDecision(ctx context.Context, approvalId, decision string) (map[string]any, error) {
	if _, err := memql.RoutingConfigurationAuthor(ctx); err != nil {
		return nil, nil
	}
	rows, err := i.routingReviews(ctx, approvalId)
	if err != nil || len(rows) != 1 || decision == "" || rowString(rows[0], "decision") != decision {
		return nil, err
	}
	return map[string]any{"approvalId": approvalId, "runId": "", "decision": decision, "runResumed": false}, nil
}

// decideRoutingReview records a person's decision on a routing review, and an
// approval CHANGES ROUTING FIRST (design record section 3).
//
// ORDER IS THE IDEMPOTENCY. The caller holds the approval's decision gate.
// The change is applied BEFORE the decision is recorded: a refusal (a tie, a
// rule something always beats, routing that moved too often) records nothing,
// so the person reads why and can decline instead; a crash between the two
// leaves the review pending, and the retry's apply finds the document already
// holding the change and writes no second revision. Recording first would
// instead spend the approval on a change that never happened.
//
// The decision is written as the deployment (the system actor, at internal
// origin through the store), because the row is the deployment's and a
// person's own authority does not reach it. decidedBy is the person, who
// passed the routing-author check here and, again, inside ApplyRoutingReview.
func (i *Integration) decideRoutingReview(ctx context.Context, approvalId string, approval map[string]any, decision string, answer map[string]any, decidedBy string, now time.Time) ([]memorynodes.MemoryNode, error) {
	if _, err := memql.RoutingConfigurationAuthor(ctx); err != nil {
		return nil, err
	}
	if decision == "answered" || len(answer) > 0 {
		return nil, fmt.Errorf("work: a routing review is decided approved or rejected; it takes no answer")
	}
	result := map[string]any{"approvalId": approvalId, "runId": "", "decision": decision, "runResumed": false}
	if decision == "approved" {
		// The artifact-hash gate, as for every kind: the hash covers the
		// subject, rule text included, so an approval never carries to text
		// edited after the fold proposed it.
		subject := rowMap(approval, "subject")
		storedHash := rowString(approval, "artifactHash")
		if ok, err := work.ResumeAllowed(storedHash, currentArtifactHash(work.ApprovalKindRoutingReview, storedHash, subject), decision); !ok {
			if errors.Is(err, work.ErrArtifactChanged) {
				return nil, fmt.Errorf("work: %w -- approve it again against what it is now", err)
			}
			return nil, fmt.Errorf("work: %w", err)
		}
		change, err := routingReviewChange(subject)
		if err != nil {
			return nil, err
		}
		applier, ok := i.engine.(routingReviewApplier)
		if !ok {
			return nil, fmt.Errorf("work: routing configuration is unavailable on this node, so an approved routing review cannot change routing")
		}
		applied, err := applier.ApplyRoutingReview(ctx, change)
		if err != nil {
			return nil, fmt.Errorf("work: routing was not changed and the review is still waiting: %w", err)
		}
		result["routingRule"], result["routingRevision"], result["routingChanged"] = applied.Rule, applied.Revision, applied.Changed
	}
	if err := i.store().decideApprovalRow(auth.ContextWithSystemActor(ctx, routingReviewActor), approvalId, decision, decidedBy, now, nil, workRowVersionAfter(approval["createdAt"], now)); err != nil {
		return nil, err
	}
	return i.resultNode(result), nil
}

// routingReviewChange is what a review's direction asks of the routing
// configuration. A demotion saves its exact text. A promotion RETIRES the
// demotion it lifts, named the way the fold named it, and never saves its own
// text: that text excludes nothing, so it would send the whole level to its
// policy, and it would tie with the demotion at the same precedence.
func routingReviewChange(subject map[string]any) (memql.RoutingReviewChange, error) {
	switch direction := rowString(subject, "direction"); direction {
	case routerlib.DirectionDemotion:
		return memql.RoutingReviewChange{SaveName: rowString(subject, "ruleName"), SaveSource: rowString(subject, "ruleSource")}, nil
	case routerlib.DirectionPromotion:
		model, level := rowString(subject, "modelId"), rowString(subject, "level")
		if strings.TrimSpace(model) == "" || strings.TrimSpace(level) == "" {
			return memql.RoutingReviewChange{}, fmt.Errorf("work: this promotion names no model or level, so there is no demotion to retire")
		}
		return memql.RoutingReviewChange{RetireName: routerlib.RuleName(routerlib.DirectionDemotion, model, level)}, nil
	default:
		return memql.RoutingReviewChange{}, fmt.Errorf("work: routing review direction %q is neither a demotion nor a promotion", direction)
	}
}
