package memql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// keyset_pagination_versioned_db_test.go covers keyset pagination when
// CONSECUTIVE RAW ROWS SHARE AN ID (memql#3388).
//
// MemoryNodes is append-only, so a read that lists "the latest row per id"
// scans RAW rows and collapses them. The SQL LIMIT bounds the RAW window, so a
// window of N rows yields N distinct results only when those N rows happen to
// belong to N different ids. When an id is updated repeatedly -- a node status
// writer, a counter, anything that rewrites the same row -- consecutive raw
// rows share an id, the window collapses to far fewer results, and the page
// comes back short while rows remain.
//
// The caller then reads that short page as "the set is exhausted" (see the
// nextCursor block in engine.go) and withdraws the cursor, so everything past
// the first window becomes unreachable with no error and no cursor.
//
// The existing keyset coverage (keyset_pagination_db_test.go) seeds ONE row per
// id, so its raw window and its result set are the same thing and the collapse
// is a no-op. That is the blind spot these tests fill.
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine.

// CLUSTERED SEEDING, and why it matters: both tests below write every version
// of an id in a contiguous createdAt run, so consecutive raw rows share an id.
//
// That is the shape a repeatedly-updated row actually has. Interleaving instead
// (id0.v0, id1.v0, ... id0.v1) hands consecutive raw rows to DIFFERENT ids,
// which lets the raw window collapse to a full page by luck and hides the
// defect completely -- an earlier draft of this test interleaved and passed
// against the broken engine, which is what sent the first fix attempt at the
// wrong function.

// walkPagesByCursor follows nextCursor until the engine reports exhaustion with
// an EMPTY cursor, returning every id seen plus the per-page row counts.
//
// Deliberately NOT walkAllPages from the sibling file: that helper also stops
// on `len(ids) < pageSize`, which is the very inference under test. A short
// page may only end the walk if the engine also withdrew the cursor; otherwise
// the defect would terminate its own test and the test would pass.
func walkPagesByCursor(
	t *testing.T,
	ctx context.Context,
	eng *MemQLEngine,
	owner string,
	pageSize int,
) ([]string, []int) {
	t.Helper()
	var all []string
	var sizes []int
	cursor := ""
	for page := 0; page < 200; page++ {
		res, err := eng.Execute(ContextWithCursor(ctx, cursor), keysetPageQuery(owner, pageSize))
		require.NoError(t, err)
		ids := pageIDs(t, res)
		all = append(all, ids...)
		sizes = append(sizes, len(ids))
		next := ""
		if m := res.GetMeta(); m != nil {
			next = m.Cursor
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return all, sizes
}

// TestKeysetPagination_ClusteredVersionsFirstPageIsFull pins the narrower half:
// a FIRST page must not come back short while rows remain.
//
// Measured on a live cluster, `v1:cluster:node` (17 distinct ids, ~14 versions
// each) answered a pageSize=10 request with 4 rows and no cursor -- the first
// page both under-filled and declared itself the complete set. This is
// separable from the continuation bug and would survive a fix aimed only at
// the cursor.
func TestKeysetPagination_ClusteredVersionsFirstPageIsFull(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-clustered-first")
	sfx := uniqueSuffix("keyset-clustered-first")
	owner := "kb:" + sfx

	const (
		distinct = 20
		versions = 10
		pageSize = 10
	)

	base := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i < distinct; i++ {
		id := fmt.Sprintf("v1:cognition:utterance:%s-%02d", sfx, i)
		for v := 0; v < versions; v++ {
			// Clustered: all versions of id i occupy a contiguous createdAt run.
			seedKeysetRow(t, ctx, db, id, base.Add(time.Duration(i*versions+v)*time.Second), owner)
		}
	}

	res, err := eng.Execute(ContextWithCursor(ctx, ""), keysetPageQuery(owner, pageSize))
	require.NoError(t, err)
	ids := pageIDs(t, res)

	require.Len(t, ids, pageSize,
		"a first page must be full while %d distinct rows remain; the raw window collapsed to %d",
		distinct, len(ids))
}

// TestKeysetPagination_ClusteredVersionsWalkFullSet is the headline memql#3388
// regression: paging to exhaustion must reach every distinct id.
func TestKeysetPagination_ClusteredVersionsWalkFullSet(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-clustered-walk")
	sfx := uniqueSuffix("keyset-clustered-walk")
	owner := "kb:" + sfx

	const (
		distinct = 20
		versions = 10
		pageSize = 5
	)

	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	want := make([]string, 0, distinct)
	for i := 0; i < distinct; i++ {
		id := fmt.Sprintf("v1:cognition:utterance:%s-%02d", sfx, i)
		want = append(want, id)
		for v := 0; v < versions; v++ {
			seedKeysetRow(t, ctx, db, id, base.Add(time.Duration(i*versions+v)*time.Second), owner)
		}
	}

	got, sizes := walkPagesByCursor(t, ctx, eng, owner, pageSize)
	reached := dedupe(got)

	require.ElementsMatch(t, want, reached,
		"paging must reach every distinct id; pages were %v, reached %d of %d",
		sizes, len(reached), distinct)
}

// TestKeysetPagination_CursorResumesFromScanPosition is the memql#3388
// Defect B regression: the continuation cursor must resume from the SCAN
// POSITION, not from the latest version of the last row returned.
//
// The engine mints the cursor from the last row of the PAGE, but every row in
// that page has already been replaced by its latest version. When an id's
// latest version is newer than rows of OTHER ids that the scan had not reached
// yet, the cursor jumps past them and they become unreachable.
//
// Four rows, three ids, ascending:
//
//	X @ t=1 and t=100     -- written first, and again last
//	Y @ t=2
//	Z @ t=3
//
// Page 1 (size 1) returns X collapsed to its latest (t=100). Minting the cursor
// from that row resumes at `createdAt > 100`, which matches nothing -- so Y and
// Z are silently lost even though the scan had only consumed the row at t=1.
func TestKeysetPagination_CursorResumesFromScanPosition(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-scanpos")
	sfx := uniqueSuffix("keyset-scanpos")
	owner := "kb:" + sfx
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	idX := fmt.Sprintf("v1:cognition:utterance:%s-X", sfx)
	idY := fmt.Sprintf("v1:cognition:utterance:%s-Y", sfx)
	idZ := fmt.Sprintf("v1:cognition:utterance:%s-Z", sfx)

	seedKeysetRow(t, ctx, db, idX, base.Add(1*time.Second), owner)
	seedKeysetRow(t, ctx, db, idY, base.Add(2*time.Second), owner)
	seedKeysetRow(t, ctx, db, idZ, base.Add(3*time.Second), owner)
	// X again, far in the future -- this is the version the collapse returns.
	seedKeysetRow(t, ctx, db, idX, base.Add(100*time.Second), owner)

	got, sizes := walkPagesByCursorAsc(t, ctx, eng, owner, 1)
	reached := dedupe(got)

	require.ElementsMatch(t, []string{idX, idY, idZ}, reached,
		"the walk must reach every id; a cursor minted from a collapsed row jumps past the scan position (pages %v)",
		sizes)
}

// walkPagesByCursorAsc is walkPagesByCursor with an ASCENDING sort, which is
// the direction the concept browser uses and the direction in which a
// latest-version timestamp can overshoot the scan position.
func walkPagesByCursorAsc(
	t *testing.T,
	ctx context.Context,
	eng *MemQLEngine,
	owner string,
	pageSize int,
) ([]string, []int) {
	t.Helper()
	q := fmt.Sprintf(`sort(paginate(concept==%s;createdBy==%q, %d), "createdAt", "asc")`,
		keysetConcept, owner, pageSize)
	var all []string
	var sizes []int
	cursor := ""
	for page := 0; page < 200; page++ {
		res, err := eng.Execute(ContextWithCursor(ctx, cursor), q)
		require.NoError(t, err)
		ids := pageIDs(t, res)
		all = append(all, ids...)
		sizes = append(sizes, len(ids))
		next := ""
		if m := res.GetMeta(); m != nil {
			next = m.Cursor
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return all, sizes
}
