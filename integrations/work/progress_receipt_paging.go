package work

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Walk owned receipts in bounded pages until one page supplies fresh credit.
// Successes with no substantive evidence and previously consumed facts cannot
// hide older work behind the first page. No scan-time watermark is persisted:
// concurrent arrivals remain eligible, and immutable credits provide deduplication
// across replicas. Existing feedback/legacy epoch cutoffs remain authoritative.
// A timeout or unreadable page refuses renewal; it never means "no progress".
func (i *Integration) claimProgressReceiptPage(ctx context.Context, rc common.RunContext, state map[string]any, next int) ([]string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	query := "query " + call("workProgressReceiptsForOwnerRun", map[string]any{"runId": rc.RunId})
	cursor, emptyPages := "", 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, next, err
		}
		result, err := i.store().engine.Execute(memql.ContextWithCursor(memql.ContextWithFreshRead(ctx), cursor), query)
		if err != nil {
			return nil, next, err
		}
		if result == nil {
			return nil, next, fmt.Errorf("work progress returned no receipt page")
		}
		rows := memqlRows(result)
		if len(rows) == 0 {
			emptyPages++
		} else {
			emptyPages = 0
		}
		if emptyPages > 32 {
			return nil, next, fmt.Errorf("work progress pagination made no progress")
		}
		var candidates []string
		for _, row := range rows {
			if memql.BareShortId(rowString(row, "ownerUserId")) != memql.BareShortId(rc.OwnerUserId) || memql.BareShortId(rowString(row, "runId")) != memql.BareShortId(rc.RunId) {
				return nil, next, fmt.Errorf("work progress receipt attribution changed")
			}
			if after, ok := rowTime(state, "receiptAfter"); ok {
				observed, valid := rowTime(row, "createdAt")
				if !valid || !observed.After(after) {
					continue
				}
			}
			for _, key := range progressReceiptKeys(row) {
				if !slices.Contains(candidates, key) {
					candidates = append(candidates, key)
				}
			}
		}
		seen, target, err := i.claimProgressCredits(ctx, rc, state, candidates, next)
		if err != nil || len(seen) > 0 {
			return seen, target, err
		}
		nextCursor := ""
		if meta := result.GetMeta(); meta != nil {
			nextCursor = meta.Cursor
			if meta.HasMore && nextCursor == "" {
				return nil, next, fmt.Errorf("work progress has more receipts without a cursor")
			}
		}
		if nextCursor == "" {
			return nil, next, nil
		}
		if nextCursor == cursor {
			return nil, next, fmt.Errorf("work progress receipt cursor did not advance")
		}
		cursor = nextCursor
	}
}
