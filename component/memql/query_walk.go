package memql

import (
	"context"
	"fmt"
)

// WalkQueryPages traverses a cursor-capable named read without increasing the
// engine's per-read limits or changing caller authority. The caller owns the
// query and any snapshot timestamp in its arguments. The callback sees one
// bounded page at a time; a full traversal either reaches exhaustion or returns
// an error, never a truncated success. Empty refined pages can still continue.
func WalkQueryPages(ctx context.Context, execute func(context.Context, string) (*ExecuteResult, error), query string, maxPages int, visit func(*ExecuteResult) error) error {
	if execute == nil || visit == nil || maxPages < 1 {
		return fmt.Errorf("query walk requires an executor, visitor and positive page budget")
	}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Override even an inherited nonempty cursor: a walk starts at page one.
		pageCtx := context.WithValue(ContextWithFreshRead(ctx), inboundCursorKey{}, cursor)
		result, err := execute(pageCtx, query)
		if err != nil {
			return err
		}
		if result == nil {
			return fmt.Errorf("query walk received no result")
		}
		next := ""
		if meta := result.GetMeta(); meta != nil {
			next = meta.Cursor
			if meta.HasMore && next == "" {
				return fmt.Errorf("query walk cannot continue: the result declares more rows without a cursor")
			}
		}
		if next != "" && (next == cursor || seen[next]) {
			return fmt.Errorf("query walk cursor did not advance")
		}
		if err = visit(result); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		seen[next] = true
		cursor = next
	}
	return fmt.Errorf("query walk exceeded its %d-page budget before exhaustion", maxPages)
}
