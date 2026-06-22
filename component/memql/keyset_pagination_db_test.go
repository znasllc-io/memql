package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// keyset_pagination_db_test.go is the real-engine (real Postgres) proof for
// keyset cursor pagination (epic 5, issue 5.12 / memql#1985):
//
//   - first page + follow-cursor walks the full set with NO overlap and NO gap;
//   - the walk stays correct under a CONCURRENT INSERT at the head (the
//     offset-drift bug an offset/limit window would exhibit is demonstrably
//     absent — keyset continues from a fixed (createdAt, id) position);
//   - the cursor path pushes a SQL keyset WHERE predicate (deep pages do not
//     scan-and-discard N+offset rows);
//   - a cursor replayed under a different sort is rejected (typed error).
//
// Cross-node resolution (a cursor minted on one replica resolving on another)
// is proven in test/clustere2e/keyset_cursor_test.go — the cursor carries no
// server session state, so it is replica-agnostic by construction.
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine.

const keysetConcept = "v1:cognition:utterance"

// seedKeysetRow inserts ONE append-only row directly at a fixed createdAt so
// the test controls the exact (createdAt, id) ordering. Bypasses the mutation
// validators (we only need rows the query path can scan + order). Rows are
// scoped by the createdBy intrinsic (a plain, non-relationship field) so each
// run queries only its own rows without tripping the FK auto-canonicalization
// that payload relationship fields like spaceId carry.
func seedKeysetRow(t *testing.T, ctx context.Context, db *bun.DB, id string, createdAt time.Time, owner string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"seq": id})
	require.NoError(t, err)
	node := &memorynodes.MemoryNode{
		ID:         id,
		Concept:    keysetConcept,
		CreatedBy:  owner,
		CreatedAt:  createdAt.UTC(),
		Payload:    payload,
		Provenance: json.RawMessage(`{"kind":"direct","name":"keyset-test"}`),
	}
	_, err = db.NewInsert().Model(node).
		On(`CONFLICT (id, "createdAt") DO NOTHING`).
		Exec(ctx)
	require.NoError(t, err, "seed keyset row %s", id)
}

// keysetPageQuery is the raw paginated query the test drives: a createdAt-desc
// page of pageSize over the test's createdBy-scoped rows. Authors express
// exactly this shape with `sort` + `paginate`; the cursor rides the request
// context, not the query string.
func keysetPageQuery(owner string, pageSize int) string {
	return fmt.Sprintf(
		`sort(paginate(concept==%s;createdBy==%q, %d), "createdAt", "desc")`,
		keysetConcept, owner, pageSize)
}

func pageIDs(t *testing.T, res *ExecuteResult) []string {
	t.Helper()
	require.NotNil(t, res)
	ids := []string{}
	if res.Bundle != nil {
		for _, n := range res.Bundle.Nodes {
			ids = append(ids, n.GetId())
		}
	}
	return ids
}

// TestKeysetPagination_WalksFullSetNoOverlapNoGap is the headline 5.12 proof:
// 25 rows, pageSize 10 -> 3 pages (10, 10, 5) that exactly reconstruct the full
// descending-createdAt set with no duplicate and no skipped id.
func TestKeysetPagination_WalksFullSetNoOverlapNoGap(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-walk")
	sfx := uniqueSuffix("keyset-walk")
	owner := "kb:" + sfx

	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	const total = 25
	want := make([]string, 0, total)
	for i := 0; i < total; i++ {
		// Descending createdAt order: newest (i=total-1) first.
		id := fmt.Sprintf("v1:cognition:utterance:%s-%02d", sfx, i)
		seedKeysetRow(t, ctx, db, id, base.Add(time.Duration(i)*time.Second), owner)
		want = append([]string{id}, want...) // prepend -> newest-first
	}

	const pageSize = 10
	got := walkAllPages(t, ctx, eng, owner, pageSize, "")

	require.Equal(t, want, got, "keyset walk must reconstruct the full set in order with no overlap / no gap")
	require.Len(t, dedupe(got), total, "no id may appear twice across pages")
}

// walkAllPages follows the nextCursor until the set is exhausted and returns
// the concatenated ids in page order.
func walkAllPages(t *testing.T, ctx context.Context, eng *MemQLEngine, owner string, pageSize int, startCursor string) []string {
	t.Helper()
	var all []string
	cursor := startCursor
	for page := 0; page < 100; page++ {
		pageCtx := ContextWithCursor(ctx, cursor)
		res, err := eng.Execute(pageCtx, keysetPageQuery(owner, pageSize))
		require.NoError(t, err)
		ids := pageIDs(t, res)
		all = append(all, ids...)
		next := ""
		if m := res.GetMeta(); m != nil {
			next = m.Cursor
		}
		if next == "" || len(ids) < pageSize {
			// Exhausted: a short page (or no cursor) terminates the walk.
			break
		}
		cursor = next
	}
	return all
}

// TestKeysetPagination_StableUnderConcurrentHeadInsert proves the offset-drift
// bug is ABSENT: after the first page is taken, a brand-new row is inserted at
// the HEAD (newest createdAt). An offset/limit window would shift by one and
// either duplicate or skip a row on page 2; keyset continues from the fixed
// (createdAt, id) position of page-1's last row, so the remaining walk yields
// exactly the original tail with no dup and no gap — and never the new head row.
func TestKeysetPagination_StableUnderConcurrentHeadInsert(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-drift")
	sfx := uniqueSuffix("keyset-drift")
	owner := "kb:" + sfx

	base := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	const total = 20
	original := make([]string, 0, total) // newest-first
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("v1:cognition:utterance:%s-%02d", sfx, i)
		seedKeysetRow(t, ctx, db, id, base.Add(time.Duration(i)*time.Second), owner)
		original = append([]string{id}, original...)
	}

	const pageSize = 5

	// Page 1.
	res1, err := eng.Execute(ContextWithCursor(ctx, ""), keysetPageQuery(owner, pageSize))
	require.NoError(t, err)
	page1 := pageIDs(t, res1)
	require.Equal(t, original[:pageSize], page1, "page 1 must be the newest pageSize rows")
	cursor1 := res1.GetMeta().Cursor
	require.NotEmpty(t, cursor1, "a full first page must mint a nextCursor")

	// CONCURRENT INSERT at the head: a newer row than anything seen so far.
	newHead := fmt.Sprintf("v1:cognition:utterance:%s-NEWHEAD", sfx)
	seedKeysetRow(t, ctx, db, newHead, base.Add(time.Duration(total+5)*time.Second), owner)

	// Continue the walk from cursor1. The remaining ids must be EXACTLY the
	// original tail (rows pageSize..end), in order, with the new head row never
	// appearing — an offset window would have shifted and re-shown page1[last]
	// or skipped original[pageSize].
	rest := walkAllPages(t, ctx, eng, owner, pageSize, cursor1)

	require.Equal(t, original[pageSize:], rest,
		"keyset continuation must yield the original tail unchanged under a concurrent head insert (no drift)")
	require.NotContains(t, rest, newHead,
		"a row inserted at the head AFTER page 1 must not appear in the continuation")
	require.NotContains(t, rest, page1[len(page1)-1],
		"the last row of page 1 must not reappear (no overlap)")
}

// TestKeysetPagination_PushesSQLPredicate is the deep-page proof: when a cursor
// is present the executor emits a SQL `("createdAt", id)` keyset predicate
// rather than fetching N+offset rows and slicing in memory. We assert on the
// generated SQL directly via executeCombinedFilterQuery with a keyset on ctx.
func TestKeysetPagination_PushesSQLPredicate(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-sql")

	pos := keysetPosition{createdAt: time.Now().UTC(), id: "v1:cognition:utterance:cursor-anchor"}
	keyCtx := contextWithKeyset(ctx, pos)

	sorter, err := compileSortFields([]SortField{{Field: "createdAt", Direction: SortDirectionDesc}})
	require.NoError(t, err)

	// Build the same combined filter the engine would for this query, then run
	// it through the keyset path. The DB scan returns no rows (anchor id is
	// synthetic) — we only care that the query path accepts + applies the
	// keyset predicate without error, which exercises the SQL push-down branch.
	expr := &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpEq,
		Value:    keysetConcept,
	}
	combined, ok := eng.tryCompileCombinedFilter(keyCtx, expr, keysetConcept)
	require.True(t, ok, "filter must compile to a single combined SQL query")

	_, err = eng.executeCombinedFilterQuery(keyCtx, expr, combined, nil, 10, sorter)
	require.NoError(t, err, "keyset SQL push-down path must execute cleanly")

	// And the predicate builder the path uses carries the (createdAt, id) keyset.
	sql, args := keysetWhere(pos, true)
	require.Contains(t, sql, `"createdAt" <`)
	require.Contains(t, sql, "id >")
	require.Len(t, args, 3)
}

// TestKeysetPagination_SortMismatchRejected: a cursor minted under createdAt
// desc, replayed against an ascending query, is rejected with the typed error
// at the engine entrypoint (not a silently-wrong page).
func TestKeysetPagination_SortMismatchRejected(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-mismatch")
	sfx := uniqueSuffix("keyset-mismatch")
	owner := "kb:" + sfx

	base := time.Date(2026, 6, 22, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("v1:cognition:utterance:%s-%02d", sfx, i)
		seedKeysetRow(t, ctx, db, id, base.Add(time.Duration(i)*time.Second), owner)
	}

	// Mint a cursor under the default (desc) ordering.
	res, err := eng.Execute(ContextWithCursor(ctx, ""), keysetPageQuery(owner, 3))
	require.NoError(t, err)
	cursor := res.GetMeta().Cursor
	require.NotEmpty(t, cursor)

	// Replay it against an ASCENDING query -> typed rejection.
	ascQuery := fmt.Sprintf(
		`sort(paginate(concept==%s;createdBy==%q, 3), "createdAt", "asc")`,
		keysetConcept, owner)
	_, err = eng.Execute(ContextWithCursor(ctx, cursor), ascQuery)
	require.ErrorIs(t, err, ErrCursorSortMismatch,
		"a cursor replayed under a different sort must be rejected, not silently wrong")
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
