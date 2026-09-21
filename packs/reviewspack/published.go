package reviewspack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// published.go -- THE PUBLIC READ THE PACK LACKED (epic memql#5532, issue
// memql#5553).
//
// # Why a builtin and not a query
//
// The read the issue asks for is "only reviews no moderation decision
// hides, only while publicDisplay is true, scoped by storeId". That is
// THREE READS WITH A GATE BETWEEN THEM, and a filter cannot make a
// cross-concept read: whether a review has been moderated is a fact about a
// different concept's rows, and whether this store shows reviews at all is
// a fact about a third.
//
// The alternative -- a plain query plus a rule that every caller must
// check publicDisplay and subtract the moderated ones first -- is exactly
// how a storefront ends up rendering reviews a merchant had switched off.
// Putting the gate in the one place that answers the question is the whole
// argument for this being Go.
//
// # It runs under the caller's own actor, and that is what keeps it honest
//
// The shopper path reaches this under the SITE OWNER'S borrowed authority,
// so the three reads below are that merchant's own rows and the composite
// owner tier decides each one. Nothing here bypasses row authorization or
// stamps internal origin -- a public read that could see rows its caller
// could not would be a far worse thing than the missing query it replaces.
//
// # The projection is narrower than the row
//
// authorEmail is NOT projected. It exists so a merchant can reply to a
// shopper, not so the internet can harvest it, and this is the one read
// whose output reaches an unauthenticated browser.

// defaultPublishedLimit bounds the answer when the caller names none.
//
// FIFTY, which is a product page's worth. A public read with no bound would
// let one request ask for every review a store has ever received.
const defaultPublishedLimit = 50

// maxPublishedLimit bounds it however large the caller asks.
const maxPublishedLimit = 200

// PublishedReview is one row as a storefront may render it.
type PublishedReview struct {
	ID              string `json:"id"`
	ProductHandle   string `json:"productHandle"`
	Title           string `json:"title,omitempty"`
	Body            string `json:"body"`
	Rating          int    `json:"rating,omitempty"`
	AuthorName      string `json:"authorName,omitempty"`
	CreatedAt       string `json:"createdAt,omitempty"`
	CreatedAtClient string `json:"createdAtClient,omitempty"`
}

// publishedForProduct answers the public read.
func (p *Provider) publishedForProduct(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	handle := strings.TrimSpace(asString(args["productHandle"]))
	if storeID == "" || handle == "" {
		return nil, fmt.Errorf("reviews: storeId and productHandle are required")
	}
	// GATE FIRST, so a store with reviews switched off costs one read and
	// returns nothing rather than reading every review and then discarding
	// them.
	on, err := p.publicDisplayOn(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if !on {
		return p.publishedNode(storeID, handle, []PublishedReview{})
	}

	rows, err := p.rowsFor(ctx, "reviewsForProduct",
		map[string]string{"storeId": storeID, "productHandle": handle})
	if err != nil {
		return nil, err
	}

	hidden, err := p.hiddenReviewIDs(ctx, storeID)
	if err != nil {
		return nil, err
	}

	limit := clampLimit(args["limit"])
	out := make([]PublishedReview, 0, len(rows))
	for _, row := range rows {
		id := strings.TrimSpace(asString(row["id"]))
		if id == "" {
			// A row with no readable id cannot be excluded by id either, so
			// it is DROPPED rather than published. Fail toward showing less.
			continue
		}
		if _, isHidden := hidden[id]; isHidden {
			continue
		}
		out = append(out, PublishedReview{
			ID:              id,
			ProductHandle:   asString(row["productHandle"]),
			Title:           asString(row["title"]),
			Body:            asString(row["body"]),
			Rating:          asInt(row["rating"]),
			AuthorName:      asString(row["authorName"]),
			CreatedAt:       asString(row["createdAt"]),
			CreatedAtClient: asString(row["createdAtClient"]),
		})
		if len(out) >= limit {
			break
		}
	}
	return p.publishedNode(storeID, handle, out)
}

// publicDisplayOn reads this store's toggle.
//
// ABSENT IS OFF, and that is the fail-closed reading rather than the
// convenient one: a store whose merchant has never touched the setting has
// not asked for reviews to be public, and a missing row must not publish
// them. Turning them on is one write.
func (p *Provider) publicDisplayOn(ctx context.Context, storeID string) (bool, error) {
	rows, err := p.rowsFor(ctx, "reviewSettingsForStore", map[string]string{"storeId": storeID})
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	on, _ := rows[0]["publicDisplay"].(bool)
	return on, nil
}

// hiddenReviewIDs is every review on this store carrying a moderation
// decision.
//
// ANY DECISION HIDES, and that follows from the closed criterion set rather
// than from a preference: every one of spam, profanity, harassment,
// off_topic, illegal and copyright is a reason to take a review down, and
// the pack deliberately has no "approve" criterion to weigh against them.
// If a client ever needs reinstatement it is a new criterion and a new
// decision, which is what append-only means.
func (p *Provider) hiddenReviewIDs(ctx context.Context, storeID string) (map[string]struct{}, error) {
	rows, err := p.rowsFor(ctx, "moderationActionsForStore", map[string]string{"storeId": storeID})
	if err != nil {
		return nil, err
	}
	hidden := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if id := strings.TrimSpace(asString(row["reviewId"])); id != "" {
			hidden[id] = struct{}{}
		}
	}
	return hidden, nil
}

// rowReader is the narrow read the pack's Go half needs.
//
// AN INTERFACE RATHER THAN THE ENGINE, and the reason is testability of the
// part that matters: the gate, the exclusion and the bound are decisions
// over rows, and a test that has to build an *ExecuteResult to exercise
// them is testing the engine's envelope instead. Behind this seam they are
// a function over values.
type rowReader interface {
	Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error)
}

// engineRowReader is the production implementation.
type engineRowReader struct {
	engine memql.IntegrationEngineAccess
}

func (e *engineRowReader) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	if e == nil || e.engine == nil {
		return nil, fmt.Errorf("reviews: the pack has no engine handle")
	}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("query ")
	b.WriteString(query)
	b.WriteByte('(')
	for i, k := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		// QuoteString, never Go quoting: it is the engine's own literal
		// escaping, and a product handle is caller-supplied text.
		b.WriteString(langparser.QuoteString(args[k]))
	}
	b.WriteByte(')')

	result, err := e.engine.Execute(ctx, b.String())
	if err != nil {
		return nil, fmt.Errorf("reviews: %s: %w", query, err)
	}
	return memql.MaterializeRows(result), nil
}

// rowsFor runs one of the pack's own queries through the seam.
func (p *Provider) rowsFor(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	if p.reader == nil {
		return nil, fmt.Errorf("reviews: the pack has no reader; the public read cannot run")
	}
	return p.reader.Rows(ctx, query, args)
}

// publishedNode wraps the answer as the single node a builtin replies with.
func (p *Provider) publishedNode(storeID, handle string, reviews []PublishedReview) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(map[string]any{
		"storeId":       storeID,
		"productHandle": handle,
		"reviews":       reviews,
		"count":         len(reviews),
	})
	if err != nil {
		return nil, fmt.Errorf("reviews: marshal published: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("reviews:published:%s:%s", storeID, handle),
		Concept:   "v1:reviews:published",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

func clampLimit(v any) int {
	n := asInt(v)
	if n <= 0 {
		return defaultPublishedLimit
	}
	if n > maxPublishedLimit {
		return maxPublishedLimit
	}
	return n
}

// asInt narrows a decoded payload number.
//
// It answers ZERO for anything it cannot read, and every caller here treats
// zero as "not given" -- clampLimit falls back to the default and a rating
// of zero is omitted from the projection. Stated rather than left implicit,
// because core/num exists precisely so a bare int() conversion in a float64
// arm does not become an implementation-defined answer out of range.
func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		if t > 1<<31 || t < -(1<<31) {
			return 0
		}
		return int(t)
	case float64:
		if t > 1<<31 || t < -(1<<31) || t != t {
			return 0
		}
		return int(t)
	case json.Number:
		n, err := t.Int64()
		if err != nil || n > 1<<31 || n < -(1<<31) {
			return 0
		}
		return int(n)
	default:
		return 0
	}
}
