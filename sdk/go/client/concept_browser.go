package client

import (
	"context"
	"fmt"
)

// DefaultConceptBrowsePageSize is the keyset page size BrowseConcept uses
// when the caller doesn't specify one. A concept registry can hold far more
// than the engine's implicit unmarked-list backstop (50 rows, the
// MEMORY_ENGINE_DEFAULT_LIST_CAP), so the concept-browse query MUST declare
// sort + paginate to opt into a keyset window + a continuation cursor --
// otherwise it silently truncates at the backstop with no way to page past
// it (memql#2008). 200 is a single comfortable page for the cockpit's row
// list; larger registries continue via the cursor.
const DefaultConceptBrowsePageSize = 200

// BrowseConcept returns every row stored under the given concept id.
//
// Bypasses the named-primitive surface intentionally. The Cockpit's
// concept-browser tab is the canonical caller: it iterates the
// concept registry from ListConcepts and lets an operator click into
// any concept's rows without knowing its name at compile time. That
// use case is concept-name-agnostic by definition, so there's no
// equivalent named primitive in the DSL tree.
//
// Every OTHER caller should reach for a typed generated method on
// QueryClient. Direct concept browsing is reserved for admin /
// debug surfaces -- the named-primitive rule (see sdk/go/CLAUDE.md)
// applies to product code without exception.
//
// Deprecated for large registries: returns only the first
// DefaultConceptBrowsePageSize rows with no way to continue. Prefer
// BrowseConceptPage, which exposes the keyset cursor so the caller can walk
// the whole concept (memql#2008). Retained because the single-page form is
// still the common case for small concepts.
func (qc *QueryClient) BrowseConcept(ctx context.Context, conceptId string) (*Result, error) {
	page, err := qc.BrowseConceptPage(ctx, conceptId, "", 0)
	if err != nil {
		return nil, err
	}
	// Re-wrap the page rows in a bundle envelope so the legacy Result
	// accessors (RawNodes / Rows) keep working unchanged.
	nodes := make([]any, 0, len(page.Rows))
	for _, r := range page.Rows {
		nodes = append(nodes, map[string]any(r))
	}
	return &Result{payload: map[string]any{
		"bundle": map[string]any{"nodes": nodes},
	}}, nil
}

// BrowseConceptPage is the keyset-paginated form of BrowseConcept. It returns
// one page of rows under the given concept id, oldest-first (createdAt ASC,
// with the id ASC tie-breaker the engine appends under equal createdAt), plus
// the continuation cursor for the next page.
//
// cursor   -- opaque continuation token from a prior page's
//
//	PageResult.NextCursor; "" for the first page.
//
// pageSize -- rows per page; <= 0 uses DefaultConceptBrowsePageSize.
//
// Walking the full registry:
//
//	cursor := ""
//	for {
//	    page, err := qc.BrowseConceptPage(ctx, id, cursor, 0)
//	    if err != nil { return err }
//	    rows = append(rows, page.Rows...)
//	    cursor = page.NextCursor
//	    if cursor == "" { break } // exhausted
//	}
//
// PageResult.Rows preserves the full nested wire shape (payload / metadata /
// provenance / intrinsics) for the bundle path, exactly like
// Result.RawNodes() -- so the cockpit's generic concept inspector renders
// unchanged.
//
// The query declares `sort` + `paginate` so it opts into a bounded keyset
// window + a next-page cursor instead of hitting the engine's implicit
// unmarked-list backstop (the MEMORY_ENGINE_DEFAULT_LIST_CAP, 50 rows) with
// no continuation (memql#2008). The cursor rides ExecuteQueryMsg.cursor and
// is bound to this query's sort ordering; it resolves on any replica.
func (qc *QueryClient) BrowseConceptPage(ctx context.Context, conceptId, cursor string, pageSize int) (*PageResult, error) {
	if conceptId == "" {
		return nil, fmt.Errorf("BrowseConcept: conceptId is required")
	}
	if pageSize <= 0 {
		pageSize = DefaultConceptBrowsePageSize
	}
	// sort(paginate(concept==X, N), "createdAt", "asc") -- the canonical
	// keyset-eligible directive chain (leading createdAt sort + a paginate
	// window), mirroring keysetPageQuery in the engine's keyset DB test. The
	// engine appends `id ASC` as the tie-breaker under equal createdAt; see
	// component/memql/cursor.go.
	query := fmt.Sprintf(`sort(paginate(concept==%s, %d), "createdAt", "asc")`, conceptId, pageSize)
	page, err := qc.ExecutePaginated(ctx, query, cursor)
	if err != nil {
		return nil, fmt.Errorf("BrowseConcept(%s): %w", conceptId, err)
	}
	return page, nil
}

// GetRowByConceptAndId returns the single row matching (conceptId,
// rowId). Same admin-surface caveat as BrowseConcept -- product code
// should use a typed lookup primitive (userById, agentById,
// etc.) instead.
func (qc *QueryClient) GetRowByConceptAndId(ctx context.Context, conceptId, rowId string) (*Result, error) {
	if conceptId == "" {
		return nil, fmt.Errorf("GetRowByConceptAndId: conceptId is required")
	}
	if rowId == "" {
		return nil, fmt.Errorf("GetRowByConceptAndId: rowId is required")
	}
	payload, err := qc.executeRaw(ctx, fmt.Sprintf("concept==%s;id==%s", conceptId, rowId))
	if err != nil {
		return nil, fmt.Errorf("GetRowByConceptAndId(%s, %s): %w", conceptId, rowId, err)
	}
	return &Result{payload: payload}, nil
}
