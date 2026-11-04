package reviewspack

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// decide.go -- RECORDING A MODERATION DECISION (reviewsDecide).
//
// One call appends one v1:reviews:reviewDecision row. Everything a caller
// could lie about is read rather than taken: the decider is the calling
// user, the store and the organization are the review's own, and the state
// the decision is judged against is derived from the log (state.go).
//
// # Who may decide
//
// A person on the STORE'S side. The cluster's own operators -- owner,
// developer and admin ranks, the people who host the store's reviews and can
// read every one of them -- never record a decision: the store holds the
// moderation decision, and the people who run the platform must not be able
// to make it for them. Being able to read the review is the second half: the
// review is read under the caller's own actor, so a caller outside the
// store's organization reads nothing and cannot decide.
//
// # On the words the moderator read
//
// The call names the revision the moderator was looking at. If the writer has
// edited the review since, the decision is refused and the moderator reads it
// again; a decision always applies to text a person actually read.
//
// # Removal belongs to one person
//
// The store's settings name the owner who approves removals. Only that person
// can remove a published review, and with no owner named nobody can.

// decide records one decision.
func (p *Provider) decide(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	decidedBy, err := moderatorOf(ctx)
	if err != nil {
		return nil, err
	}
	reviewID := canonicalReviewID(asString(args["reviewId"]))
	action := strings.TrimSpace(asString(args["action"]))
	reason := strings.TrimSpace(asString(args["reason"]))
	note := strings.TrimSpace(asString(args["note"]))
	if reviewID == "" || action == "" {
		return nil, fmt.Errorf("reviews: reviewId and action are required")
	}
	if action == ActionErase {
		return nil, fmt.Errorf("reviews: erasing a reviewer's data is reviewsEraseReviewer, which also blanks every saved version")
	}
	review, err := p.readableReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	version := VersionOf(review)
	current := revisionOf(version.Revision)
	if seen := asInt(args["revision"]); revisionOf(seen) != current {
		return nil, fmt.Errorf("reviews: the review changed while it was open (now revision %d); read it again before deciding", current)
	}
	// THE WORDS, NOT ONLY THEIR NUMBER: two edits sent at once can both
	// arrive as one revision, so a caller names the fingerprint of the words
	// it showed (reviewsList answers it), and a decision on words the review
	// no longer holds is refused.
	if seen := strings.TrimSpace(asString(args["contentHash"])); seen != "" && seen != version.ContentHash {
		return nil, fmt.Errorf("reviews: the review's words changed while it was open; read it again before deciding")
	}
	storeID := strings.TrimSpace(asString(review["storeId"]))
	// THE ROW BELONGS TO THE REVIEW'S MERCHANT, not to the person deciding:
	// the public read runs under the site owner's borrowed authority, and a
	// decision only its author could read would publish or hide nothing.
	owner := strings.TrimSpace(asString(review["ownerUserId"]))
	if owner == "" {
		return nil, fmt.Errorf("reviews: review %q carries no owner", reviewID)
	}
	decisions, err := p.decisionsForReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	approver, err := p.isRemovalApprover(ctx, storeID, decidedBy)
	if err != nil {
		return nil, err
	}
	if err := ValidateDecision(action, reason, StateOf(version, decisions), approver); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	decisionID := "decision-" + slugOf(strings.TrimPrefix(reviewID, reviewIDPrefix)) + "-" + strconv.FormatInt(now.UnixNano(), 36)
	write := map[string]any{
		"decisionId":  decisionID,
		"ownerUserId": owner,
		"reviewId":    reviewID,
		"storeId":     storeID,
		"action":      action,
		"revision":    current,
		"contentHash": version.ContentHash,
		"decidedBy":   decidedBy,
	}
	// OPTIONAL FIELDS ARE SENT ONLY WITH SOMETHING IN THEM: the mutation
	// checks reason against the closed set, and "" is not a member.
	setIfPresent(write, "accountId", strings.TrimSpace(asString(review["accountId"])))
	setIfPresent(write, "reason", reason)
	setIfPresent(write, "note", note)
	if err := p.writer.Write(ctx, "appendReviewDecision", write); err != nil {
		return nil, err
	}
	next := StateOf(version, append(decisions, Decision{ID: decisionID, Action: action, Revision: current, ContentHash: version.ContentHash, At: now}))
	return receipt("v1:reviews:decided", decisionID, map[string]any{
		"decisionId": decisionID, "reviewId": reviewID, "action": action, "state": string(next),
	})
}

// receipt is the one node a capability answers with once its write is done:
// what happened, never the row itself.
func receipt(concept, id string, body map[string]any) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("reviews: marshal receipt: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        id,
		Concept:   concept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// moderatorOf is the calling user, or a refusal when the caller may not
// moderate at all.
func moderatorOf(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || strings.TrimSpace(ac.UserId) == "" {
		return "", fmt.Errorf("reviews: a decision needs a signed-in person")
	}
	if auth.IsClusterOperator(ac.Role) {
		return "", fmt.Errorf("reviews: the cluster's operators host the store's reviews and do not moderate them; a person on the store's side decides")
	}
	return strings.TrimSpace(ac.UserId), nil
}

// readableReview reads one review under the caller's own actor. A review the
// caller cannot read cannot be decided.
func (p *Provider) readableReview(ctx context.Context, reviewID string) (map[string]any, error) {
	rows, err := p.rowsFor(ctx, "reviewById", map[string]string{"reviewId": reviewID})
	if err != nil {
		return nil, fmt.Errorf("reviews: reading the review: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("reviews: review %q is not readable by this caller", reviewID)
	}
	if strings.TrimSpace(asString(rows[0]["storeId"])) == "" {
		return nil, fmt.Errorf("reviews: review %q carries no storeId", reviewID)
	}
	return rows[0], nil
}

// decisionsForReview reads one review's whole decision log.
func (p *Provider) decisionsForReview(ctx context.Context, reviewID string) ([]Decision, error) {
	rows, err := p.rowsFor(ctx, "reviewDecisionsForReview", map[string]string{"reviewId": reviewID})
	if err != nil {
		return nil, fmt.Errorf("reviews: reading the decision log: %w", err)
	}
	out := make([]Decision, 0, len(rows))
	for _, row := range rows {
		out = append(out, decisionFromRow(row))
	}
	return out, nil
}

// isRemovalApprover reports whether userID is the owner this store's settings
// name. No settings, or no owner named, is false: nobody may remove.
func (p *Provider) isRemovalApprover(ctx context.Context, storeID, userID string) (bool, error) {
	settings, err := p.settingsFor(ctx, storeID)
	if err != nil {
		return false, err
	}
	approver := strings.TrimSpace(asString(settings["removalApproverUserId"]))
	return approver != "" && approver == userID, nil
}

// settingsFor reads one store's settings row; an absent row is an empty map.
func (p *Provider) settingsFor(ctx context.Context, storeID string) (map[string]any, error) {
	rows, err := p.rowsFor(ctx, "reviewSettingsForStore", map[string]string{"storeId": storeID})
	if err != nil {
		return nil, fmt.Errorf("reviews: reading the store's settings: %w", err)
	}
	if len(rows) == 0 {
		return map[string]any{}, nil
	}
	return rows[0], nil
}

// setIfPresent sets key only when value carries something.
func setIfPresent(args map[string]any, key, value string) {
	if value != "" {
		args[key] = value
	}
}
