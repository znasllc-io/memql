package calls

import (
	"context"
	"errors"
	"fmt"
	"sync"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// These cursors are scheduling hints, not authority. Configured Service copies
// share them; a new process starts a fresh traversal. Every attempted effect
// still rereads its rows under the existing cross-replica database gates.
type lifecycleScans struct {
	workflows, resources, legacyResources lifecycleScan
}

type lifecycleScan struct {
	mu     sync.Mutex
	cursor string
	recent []string
}

var errLifecycleContinuation = errors.New("calls: lifecycle inventory continuation is invalid")

// run visits one bounded native keyset page. A held or faulty first row cannot
// pin future passes to the head of the inventory. Cancellation retains the page
// for retry; completing it advances even if individual rows failed. A concurrent
// local pass is already doing this work, so it need not wait on provider I/O.
func (scan *lifecycleScan) run(ctx context.Context, store *Store, query string, limit int, visit func(map[string]any) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !scan.mu.TryLock() {
		return nil
	}
	defer scan.mu.Unlock()
	rows, next, err := store.lifecyclePage(ctx, query, scan.cursor, limit)
	if err != nil {
		// A changed query sort or invalid process-local cursor starts a clean
		// traversal next time. Provider/read errors retain progress for retry.
		if errors.Is(err, memql.ErrCursorMalformed) || errors.Is(err, memql.ErrCursorSortMismatch) || errors.Is(err, errLifecycleContinuation) {
			scan.cursor = ""
			scan.recent = nil
		}
		return err
	}
	for _, prior := range scan.recent {
		if next != "" && next == prior {
			scan.cursor, scan.recent = "", nil
			return errLifecycleContinuation
		}
	}
	var first error
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(row); err != nil && first == nil {
			first = err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scan.cursor = next
	if next == "" {
		scan.recent = nil
	} else {
		scan.recent = append(scan.recent, next)
		if len(scan.recent) > 8 {
			scan.recent = scan.recent[len(scan.recent)-8:]
		}
	}
	return first
}

func (s *Store) lifecyclePage(ctx context.Context, query, cursor string, limit int) ([]map[string]any, string, error) {
	call, err := langparser.RenderCall(query, nil)
	if err != nil {
		return nil, "", fmt.Errorf("calls: render lifecycle inventory: %w", scrubbed(err))
	}
	actor, err := asCluster(ctx)
	if err != nil {
		return nil, "", err
	}
	if s == nil || s.engine == nil {
		return nil, "", errNoEngine
	}
	read := memql.ContextWithCursor(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(actor)), cursor)
	raw, err := s.engine.Execute(read, "query "+call)
	if err != nil {
		return nil, "", quietly("calls: lifecycle inventory", err)
	}
	result, ok := raw.(*memql.ExecuteResult)
	if !ok || result == nil || result.GetMeta() == nil || result.OutputPayload() == nil {
		return nil, "", fmt.Errorf("calls: lifecycle inventory is incomplete")
	}
	rows, meta := memql.MaterializeRows(result.OutputPayload()), result.GetMeta()
	switch payload := result.OutputPayload().(type) {
	case []any:
		if len(payload) != len(rows) {
			return nil, "", fmt.Errorf("calls: lifecycle inventory contains invalid rows")
		}
	case []map[string]any:
	default:
		return nil, "", fmt.Errorf("calls: lifecycle inventory has no row projection")
	}
	if len(rows) > limit || (meta.HasMore && meta.Cursor == "") || (meta.Cursor != "" && (meta.Cursor == cursor || len(rows) == 0)) {
		return nil, "", errLifecycleContinuation
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		id := rowString(row, "id")
		if id != "" && seen[id] {
			return nil, "", errLifecycleContinuation
		}
		seen[id] = true
	}
	return rows, meta.Cursor, nil
}
