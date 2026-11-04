package reviewspack

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// notify.go -- "A REVIEW IS WAITING" (reviewsNotifyModerator).
//
// A store's moderator is told when a review arrives and when an edit returns
// one to the queue: one email per review revision to the address the store's
// settings name, through the transactional outbox (the address is the store's
// own staff mailbox, which the outbox's allowlist must admit). It carries the
// product and the admin panel's address -- NO review text and nothing about
// the writer, because an inbox is not where a store's customers' words or
// addresses should travel.
//
// Called by the store's own automation on the review's create and update
// events, which run trusted with internal origin, so it is @serverOnly. It
// reads under a system identity: the automation's own actor owns nothing a
// person can read.

const notifyActor = "reviews-notify-moderator"

func (p *Provider) notifyModerator(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	// FOR THE STORE'S OWN AUTOMATION ONLY. A client that could call this
	// could mail a store's moderator about any review it names; trusted
	// automations call with internal origin, and nothing else may.
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("reviews: reviewsNotifyModerator is called by the store's automation, not by a client")
	}
	reviewID := canonicalReviewID(asString(args["reviewId"]))
	if reviewID == "" {
		return nil, fmt.Errorf("reviews: reviewId is required")
	}
	sys := auth.ContextWithSystemActor(ctx, notifyActor)
	review, err := p.readableReview(sys, reviewID)
	if err != nil {
		return nil, err
	}
	storeID := strings.TrimSpace(asString(review["storeId"]))
	settings, err := p.settingsFor(sys, storeID)
	if err != nil {
		return nil, err
	}
	target := EffectiveSettings(settings).NotifyEmail
	if target == "" {
		return receipt("v1:reviews:notified", reviewID, map[string]any{"status": "skipped", "reason": "the store names no address"})
	}
	decisions, err := p.decisionsForReview(sys, reviewID)
	if err != nil {
		return nil, err
	}
	version := VersionOf(review)
	if StateOf(version, decisions) != StateWaiting {
		return receipt("v1:reviews:notified", reviewID, map[string]any{"status": "skipped", "reason": "the review is not waiting"})
	}
	revision := revisionOf(version.Revision)
	requestID := "reviews-waiting-" + slugOf(strings.TrimPrefix(reviewID, reviewIDPrefix)) + "-" + strconv.Itoa(revision) + "-" + version.ContentHash[:8]
	product := strings.TrimSpace(asString(review["productHandle"]))
	subject := "A review is waiting: " + product
	what := "A new review"
	if revision > 1 {
		what = "An edited review"
	}
	body := what + " of " + product + " is waiting to be read.\n\n"
	if base := p.adminBase(); base != "" {
		body += "Open the store's reviews in the admin panel: " + base + "/\n\n"
	}
	body += "Nothing is published until someone on the store's side publishes it. " +
		"This email carries no review text and nothing about the person who wrote it."
	if err := p.writer.Write(ctx, "stageOutboundRequest", map[string]any{
		"requestId":   requestID,
		"medium":      "email",
		"target":      target,
		"subject":     subject,
		"body":        body,
		"dedupeKey":   requestID,
		"requestedBy": "reviews:notifyModerator",
	}); err != nil {
		return nil, err
	}
	return receipt("v1:reviews:notified", reviewID, map[string]any{"status": "staged", "requestId": requestID})
}

// adminBase is the admin panel's address, from the cluster's domain.
func (p *Provider) adminBase() string {
	if p.osBase != nil {
		return strings.TrimRight(p.osBase(), "/")
	}
	if d := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN")); d != "" {
		return "https://os." + d
	}
	return ""
}
