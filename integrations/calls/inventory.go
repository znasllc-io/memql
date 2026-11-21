package calls

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/component/memql"
)

var errIncompleteInventory = errors.New("calls: incomplete accounting inventory")

// readCompleteClusterInventory is for admission/accounting that must know the
// entire bounded inventory. Its named private query must sort by intrinsic row
// id and paginate at most 200. Operator MaxResults may lower that window, so
// only the actual engine continuation proves exhaustion. The row threshold is
// exclusive, retaining existing refusal at 1,000 (or a smaller caller bound).
// Native callers own the admission gate; this read does not widen that gate or
// turn a partial page into authority to spend. Reconciliation uses fair cursors
// separately so overflow can block new commitments without blocking cleanup.
func (s *Store) readCompleteClusterInventory(ctx context.Context, what, call string, rendered error, maxRows int) ([]map[string]any, error) {
	if rendered != nil {
		return nil, quietly("calls: render "+what, rendered)
	}
	if maxRows < 1 || maxRows > 1000 {
		return nil, errIncompleteInventory
	}
	cluster, err := asCluster(ctx)
	if err != nil {
		return nil, err
	}
	if s == nil || s.engine == nil {
		return nil, errNoEngine
	}
	base := memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(cluster))
	rows := make([]map[string]any, 0)
	ids := make(map[string]bool)
	cursors := make(map[string]bool)
	cursor := ""
	for range (maxRows + 199) / 200 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := s.engine.Execute(memql.ContextWithCursor(base, cursor), "query "+call)
		if err != nil {
			return nil, quietly("calls: read "+what, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		executed, page, err := accountingInventoryPage(result)
		if err != nil {
			return nil, err
		}

		if len(rows)+len(page) >= maxRows {
			return nil, errIncompleteInventory
		}
		for _, row := range page {
			id := rowString(row, "id")
			if id == "" || ids[id] {
				return nil, errIncompleteInventory
			}
			ids[id] = true
			rows = append(rows, row)
		}
		next, err := memql.ResultPageCursor(executed)
		if err != nil {
			return nil, errIncompleteInventory
		}
		if next == "" {
			return rows, nil
		}
		if len(page) == 0 || cursors[next] {
			return nil, errIncompleteInventory
		}
		cursors[next] = true
		cursor = next
	}
	return nil, errIncompleteInventory
}

// A shaped query is an explicit list, including an explicit empty list. The
// generic materializer accepts many envelopes and drops malformed elements;
// accounting cannot interpret those fallbacks as a complete zero inventory.
func accountingInventoryPage(result any) (*memql.ExecuteResult, []map[string]any, error) {
	executed, ok := result.(*memql.ExecuteResult)
	if !ok || executed == nil || executed.GetMeta() == nil || executed.OutputPayload() == nil {
		return nil, nil, errIncompleteInventory
	}
	rows := memql.MaterializeRows(executed.OutputPayload())
	switch payload := executed.OutputPayload().(type) {
	case []any:
		if len(rows) != len(payload) {
			return nil, nil, errIncompleteInventory
		}
	case []map[string]any:
		if len(rows) != len(payload) {
			return nil, nil, errIncompleteInventory
		}
	default:
		return nil, nil, errIncompleteInventory
	}
	return executed, rows, nil
}
