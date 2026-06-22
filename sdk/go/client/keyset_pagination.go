package client

import (
	"context"
	"encoding/json"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/encoding/protojson"
)

// keyset_pagination.go is a documented carve-out (parallel to
// concept_browser.go): a thin SDK surface for KEYSET CURSOR pagination
// (memql#1985). The named-primitive generated methods do not yet thread the
// opaque continuation cursor, so this exposes the request-side cursor field +
// the response-side nextCursor through SDK-owned types. The cursor is opaque to
// callers; mint it from a prior PageResult.NextCursor and pass it back to walk
// the set with no dup / no gap (stable under concurrent inserts) -- and, because
// the cursor carries no server session state, the SAME cursor resolves on ANY
// replica it is replayed against.
//
// Like concept_browser.go, this stays on the SDK side of the wire so consumer
// code never imports memqlv1. It is NOT a raw-DSL escape hatch for application
// code -- the named-primitive contract still holds for everything else.

// PageResult is one keyset page: the projected rows plus the cursor to continue.
type PageResult struct {
	// Rows is the page of projected rows (shape-flattened, like Result.Rows()).
	Rows []Row
	// NextCursor continues from the last row of a FULL page; empty when the set
	// is exhausted. Opaque + bound to the query's sort ordering.
	NextCursor string
	// HasMore mirrors the engine's signal that more rows remain.
	HasMore bool
}

// ExecutePaginated runs a paginated MemQL query and returns one keyset page.
// Pass an empty cursor for the first page; thread PageResult.NextCursor back in
// for each subsequent page. The query should declare `sort` + `paginate`; the
// cursor rides the request, not the query string.
func (qc *QueryClient) ExecutePaginated(ctx context.Context, query, cursor string) (*PageResult, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{
				Query:  query,
				Cursor: cursor,
			},
		},
	}

	resp, err := qc.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("paginated query: %w", err)
	}
	if qErr := resp.GetQueryError(); qErr != nil {
		return nil, fmt.Errorf("paginated query error: %s", qErr.GetError().GetMessage())
	}

	result := resp.GetQueryResult()
	if result == nil || result.GetResult() == nil {
		return &PageResult{}, nil
	}

	page := &PageResult{}
	if meta := result.GetResult().GetMeta(); meta != nil {
		page.NextCursor = meta.GetCursor()
		page.HasMore = meta.GetHasMore()
	}

	jsonBytes, err := protojson.Marshal(result.GetResult())
	if err != nil {
		return nil, fmt.Errorf("marshal paginated result: %w", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		return nil, fmt.Errorf("parse paginated result JSON: %w", err)
	}
	page.Rows = rowsFromEnvelope(parsed)
	return page, nil
}

// rowsFromEnvelope flattens both the bundle-bearing and shape-wrapped engine
// reply envelopes into a row slice, mirroring Result.Rows() but local to this
// carve-out so it does not depend on Result's unexported payload field.
func rowsFromEnvelope(env map[string]any) []Row {
	rows := []Row{}
	if bundle, ok := env["bundle"].(map[string]any); ok {
		if nodes, ok := bundle["nodes"].([]any); ok {
			for _, n := range nodes {
				if row, ok := n.(map[string]any); ok {
					rows = append(rows, row)
				}
			}
		}
	}
	if data, ok := env["data"].([]any); ok {
		for _, d := range data {
			if row, ok := d.(map[string]any); ok {
				rows = append(rows, row)
			}
		}
	}
	return rows
}
