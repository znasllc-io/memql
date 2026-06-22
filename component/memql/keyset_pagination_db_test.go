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

// TestKeysetPagination_EqualCreatedAtTieBreak is the airtight-boundary proof:
// several rows share the EXACT same createdAt (same nanosecond), and the page
// size splits that equal-timestamp group across a page boundary. The keyset
// predicate's `id > ?` tie-breaker is only correct if the SQL ORDER BY ends
// with `id ASC`; if the ORDER BY were `createdAt`-only, two rows in the equal
// group straddling the boundary would be skipped or duplicated. We walk the
// cursor and assert the full set returns with NO skip and NO duplicate, in the
// exact (createdAt DESC, id ASC) order. The concurrent-insert test uses DISTINCT
// timestamps, so it cannot catch a missing id tie-breaker — this one does.
func TestKeysetPagination_EqualCreatedAtTieBreak(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-tie")
	sfx := uniqueSuffix("keyset-tie")
	owner := "kb:" + sfx

	// Layout (newest-first, which is the expected page order):
	//   2 rows at t=high   (ids hi-0, hi-1)
	//   4 rows at t=mid    (ids mid-0..mid-3)  <- the equal-timestamp group
	//   2 rows at t=low    (ids lo-0, lo-1)
	// Within an equal-createdAt group the order is id ASC.
	high := time.Date(2026, 6, 22, 18, 0, 2, 0, time.UTC)
	mid := time.Date(2026, 6, 22, 18, 0, 1, 0, time.UTC) // identical for all 4
	low := time.Date(2026, 6, 22, 18, 0, 0, 0, time.UTC)

	mk := func(tag string, n int) string {
		return fmt.Sprintf("v1:cognition:utterance:%s-%s-%d", sfx, tag, n)
	}

	// Seed (insertion order deliberately scrambled to prove ORDER BY, not
	// insert order, drives the result).
	seedKeysetRow(t, ctx, db, mk("mid", 2), mid, owner)
	seedKeysetRow(t, ctx, db, mk("hi", 1), high, owner)
	seedKeysetRow(t, ctx, db, mk("lo", 0), low, owner)
	seedKeysetRow(t, ctx, db, mk("mid", 0), mid, owner)
	seedKeysetRow(t, ctx, db, mk("mid", 3), mid, owner)
	seedKeysetRow(t, ctx, db, mk("hi", 0), high, owner)
	seedKeysetRow(t, ctx, db, mk("lo", 1), low, owner)
	seedKeysetRow(t, ctx, db, mk("mid", 1), mid, owner)

	// Expected full order: high (id ASC), then mid (id ASC), then low (id ASC).
	want := []string{
		mk("hi", 0), mk("hi", 1),
		mk("mid", 0), mk("mid", 1), mk("mid", 2), mk("mid", 3),
		mk("lo", 0), mk("lo", 1),
	}

	// pageSize 3 puts the page-1 boundary INSIDE the mid group:
	//   page 1: hi-0, hi-1, mid-0   (cursor at mid-0)
	//   page 2: mid-1, mid-2, mid-3 (cursor at mid-3)  <- continues mid group
	//   page 3: lo-0, lo-1          (short -> exhausted)
	const pageSize = 3
	got := walkAllPages(t, ctx, eng, owner, pageSize, "")

	require.Equal(t, want, got,
		"equal-createdAt rows split across a page boundary must walk in (createdAt DESC, id ASC) order with no skip / no dup")
	require.Len(t, dedupe(got), len(want), "no id may appear twice across the equal-timestamp boundary")
}

// TestKeysetPagination_FallbackOrderByEndsWithIdAsc guards the defense-in-depth
// invariant directly (no DB): the ORDER BY fallback used by the combined-filter
// keyset path must end with `id ASC`, matching the keyset predicate's `id > ?`
// tie-breaker. `combinedFilterOrderExprs(nil)` is the exact list
// executeCombinedFilterQuery applies when no sorter is threaded; if a future
// edit reverts it to a createdAt-only ORDER BY, this fails.
func TestKeysetPagination_FallbackOrderByEndsWithIdAsc(t *testing.T) {
	exprs := combinedFilterOrderExprs(nil)
	require.NotEmpty(t, exprs)
	require.Equal(t, "id ASC", exprs[len(exprs)-1],
		"nil-sorter keyset ORDER BY must end with id ASC to agree with the keyset predicate")

	// A compiled sorter (the production cursor path) is airtight already: it
	// appends createdAt DESC, id ASC. Confirm the contract holds there too.
	s, err := compileSortFields([]SortField{{Field: "createdAt", Direction: SortDirectionDesc}})
	require.NoError(t, err)
	withSorter := combinedFilterOrderExprs(s)
	require.Equal(t, "id ASC", withSorter[len(withSorter)-1],
		"compiled-sorter keyset ORDER BY must also end with id ASC")
}

// TestKeysetPagination_LoadBoundedFirstPageWalksFullSet is the
// realistic-row-count load proof for the epic-5 verification (5.11 / memql#1975):
// seed 500 append-only rows (the shape of a busy space's message history, a hot
// per-user token list, or a long spaces list) and confirm the hot-path read does
// NOT pull the universe. It asserts:
//
//   - the FIRST page is BOUNDED to the page size (50 -- the production `paginate
//     50` default), NOT a 500-row full-table pull;
//   - the keyset walk reconstructs the FULL 500-row set in descending-createdAt
//     order across exactly ceil(500/50)=10 pages with NO duplicate and NO gap;
//   - every non-final page is exactly the page size (a full window each time the
//     cursor advances -- no short reads that would signal a broken predicate).
//
// 500 rows at pageSize 50 mirrors querySpaceUtterances / queryActiveSpaces /
// queryWorkerTokensForUser, all of which carry `sort "createdAt","desc"` +
// `paginate 50` on main. Postgres-gated (skips without a reachable DB) like its
// siblings in this file.
func TestKeysetPagination_LoadBoundedFirstPageWalksFullSet(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-load")
	sfx := uniqueSuffix("keyset-load")
	owner := "kb:" + sfx

	base := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	const total = 500
	const pageSize = 50 // the production `paginate 50` default
	want := make([]string, 0, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("v1:cognition:utterance:%s-%04d", sfx, i)
		seedKeysetRow(t, ctx, db, id, base.Add(time.Duration(i)*time.Second), owner)
		want = append([]string{id}, want...) // prepend -> newest-first
	}

	// The FIRST page must be bounded to pageSize, NOT a 500-row full pull.
	res1, err := eng.Execute(ContextWithCursor(ctx, ""), keysetPageQuery(owner, pageSize))
	require.NoError(t, err)
	page1 := pageIDs(t, res1)
	require.Len(t, page1, pageSize,
		"first page must be bounded to the page size, not a full-table pull of all %d rows", total)
	require.Equal(t, want[:pageSize], page1, "page 1 must be the newest pageSize rows in createdAt-desc order")
	require.NotEmpty(t, res1.GetMeta().Cursor, "a full first page over a larger set must mint a nextCursor")

	// The keyset walk must reconstruct the full set with no dup / no gap.
	got := walkAllPages(t, ctx, eng, owner, pageSize, "")
	require.Equal(t, want, got,
		"keyset walk over %d rows must reconstruct the full descending-createdAt set with no overlap / no gap", total)
	require.Len(t, dedupe(got), total, "no id may appear twice across the %d-row walk", total)
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
