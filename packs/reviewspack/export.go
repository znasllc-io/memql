package reviewspack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// export.go -- ONE STORE'S WHOLE REVIEWS RECORD (reviewsExportStore).
//
// The store's reviews belong to the store, and leave with it: this answers
// one JSON file holding every review (each with the state its decision log
// gives it), every decision, every buyer link and the settings, so a store can
// keep its record or take it elsewhere. A link's token hash is left out: it is
// a credential's digest, not the store's data.
//
// Read under the caller's own actor: what a caller cannot read is not in
// their export.

// exportFormat names the file's shape, so a later reader knows what it holds.
const exportFormat = "memql.reviews.export/v1"

func (p *Provider) exportStore(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	if storeID == "" {
		return nil, fmt.Errorf("reviews: storeId is required")
	}
	if err := p.requireSiteForStore(ctx, asString(args["siteId"]), storeID); err != nil {
		return nil, err
	}
	reviews, err := p.rowsFor(ctx, "reviewsInStore", map[string]string{"storeId": storeID})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(reviews))
	for _, r := range reviews {
		ids = append(ids, canonicalReviewID(asString(r["id"])))
	}
	decisionRows := []map[string]any{}
	byReview := map[string][]Decision{}
	if len(ids) > 0 {
		decisionRows, err = p.reader.RowsWithList(ctx, "reviewDecisionsForReviews", map[string]string{"storeId": storeID}, "reviewIds", ids)
		if err != nil {
			return nil, err
		}
		for _, row := range decisionRows {
			d := decisionFromRow(row)
			byReview[d.ReviewID] = append(byReview[d.ReviewID], d)
		}
	}
	for _, r := range reviews {
		r["state"] = string(StateOf(VersionOf(r), byReview[canonicalReviewID(asString(r["id"]))]))
	}
	requests, err := p.rowsFor(ctx, "reviewRequestsForStore", map[string]string{"storeId": storeID})
	if err != nil {
		return nil, err
	}
	for _, r := range requests {
		delete(r, "tokenHash")
	}
	settings, err := p.settingsFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	now := p.clock()
	doc, err := json.MarshalIndent(map[string]any{
		"format":     exportFormat,
		"exportedAt": now.Format(time.RFC3339Nano),
		"storeId":    storeID,
		"reviews":    reviews,
		"decisions":  decisionRows,
		"requests":   requests,
		"settings":   settings,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("reviews: marshal export: %w", err)
	}
	name := "reviews-" + slugOf(storeID) + "-" + now.Format("2006-01-02") + ".json"
	return receipt("v1:reviews:export", "export-"+slugOf(storeID), map[string]any{"fileName": name, "json": string(doc)})
}
