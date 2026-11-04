package reviewspack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// erase.go -- ERASING A WRITER'S DATA ON REQUEST (reviewsEraseReviewer).
//
// The store's named owner erases one review's writer data: name, email,
// headline and text are blanked in EVERY saved version of the review row,
// and the buyer's email on the link the review came through. A row's history
// is its versions -- MemoryNodes is keyed (id, createdAt) -- so blanking the
// newest version alone would leave the words in the history, where a
// point-in-time read still returns them.
//
// The decision log keeps that it happened: an `erase` decision with who and
// when, and no personal data. The decision is written FIRST, so the review is
// hidden (erased is terminal) before anything is blanked: a blanked review is
// never shown as an empty published one. A run that stopped after the
// decision is finished by running it again, which rewrites the rows without
// recording a second decision.

// redactor rewrites named fields in every saved version of one row.
type redactor interface {
	Redact(ctx context.Context, concept, id string, fields map[string]string) (int64, error)
}

// sqlRedactor is the production redactor: the row's versions, in place.
type sqlRedactor struct {
	db func() *bun.DB
}

// Redact sets each field to its value in every version of one row. A field
// the version does not carry is left absent (jsonb_set create_missing=false),
// except erasedAt, which is the record that the erasure happened.
//
// EVERY VERSION, STAGED OR NOT: a version skipped here KEEPS THE WRITER'S
// WORDS. This is an erasure on request: the obligation is that the person's
// data is gone wherever it sits, and a visibility gate would leave the one
// copy nobody can see. integrations/shopify/compliance.go makes the same
// argument for customers/redact. (A write, not a read, so it carries no
// staged-read verdict.)
func (r sqlRedactor) Redact(ctx context.Context, concept, id string, fields map[string]string) (int64, error) {
	if r.db == nil || r.db() == nil {
		return 0, fmt.Errorf("reviews: erasing needs a database handle")
	}
	expr := "payload"
	args := []any{concept, id}
	for _, field := range sortedFields(fields) {
		missing := "false"
		if field == "erasedAt" {
			missing = "true"
		}
		args = append(args, "{"+field+"}", fields[field])
		expr = fmt.Sprintf("jsonb_set(%s, $%d::text[], to_jsonb($%d::text), %s)", expr, len(args)-1, len(args), missing)
	}
	// The underlying database/sql handle, with the driver's own $n
	// placeholders, as integrations/shopify/compliance.go does: bun's
	// formatter would read them as text.
	res, err := r.db().DB.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload = `+expr+` WHERE concept = $1 AND id = $2`, args...)
	if err != nil {
		return 0, fmt.Errorf("reviews: erasing %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func sortedFields(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// eraseReviewer answers reviewsEraseReviewer.
func (p *Provider) eraseReviewer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	decidedBy, err := moderatorOf(ctx)
	if err != nil {
		return nil, err
	}
	reviewID := canonicalReviewID(asString(args["reviewId"]))
	if reviewID == "" {
		return nil, fmt.Errorf("reviews: reviewId is required")
	}
	review, err := p.readableReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	storeID := strings.TrimSpace(asString(review["storeId"]))
	decisions, err := p.decisionsForReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	approver, err := p.isRemovalApprover(ctx, storeID, decidedBy)
	if err != nil {
		return nil, err
	}
	if !approver {
		return nil, fmt.Errorf("reviews: only the store's owner can erase a reviewer's data")
	}
	state := StateOf(VersionOf(review), decisions)
	if state != StateErased {
		// THE DECISION FIRST: from here the review is hidden, whatever happens
		// to the rewrite below.
		if err := ValidateDecision(ActionErase, "", state, approver); err != nil {
			return nil, err
		}
		write := map[string]any{
			"decisionId":  "decision-" + slugOf(strings.TrimPrefix(reviewID, reviewIDPrefix)) + "-erase",
			"ownerUserId": strings.TrimSpace(asString(review["ownerUserId"])),
			"reviewId":    reviewID,
			"storeId":     storeID,
			"action":      ActionErase,
			"revision":    revisionOf(asInt(review["revision"])),
			"decidedBy":   decidedBy,
		}
		setIfPresent(write, "accountId", strings.TrimSpace(asString(review["accountId"])))
		setIfPresent(write, "note", strings.TrimSpace(asString(args["note"])))
		if err := p.writer.Write(ctx, "appendReviewDecision", write); err != nil {
			return nil, err
		}
	}
	if p.redactor == nil {
		return nil, fmt.Errorf("reviews: erasing needs a database handle")
	}
	now := p.clock().Format(time.RFC3339Nano)
	versions, err := p.redactor.Redact(ctx, "v1:reviews:review", reviewID, map[string]string{
		"authorName": "", "authorEmail": "", "title": "", "body": "", "erasedAt": now,
	})
	if err != nil {
		return nil, err
	}
	if requestID := strings.TrimSpace(asString(review["requestId"])); requestID != "" {
		n, err := p.redactor.Redact(ctx, "v1:reviews:reviewRequest", "v1:reviews:reviewRequest:"+bareID(requestID, "v1:reviews:reviewRequest:"),
			map[string]string{"email": ""})
		if err != nil {
			return nil, err
		}
		versions += n
	}
	return receipt("v1:reviews:erased", reviewID, map[string]any{"reviewId": reviewID, "state": string(StateErased), "versionsRewritten": versions})
}
