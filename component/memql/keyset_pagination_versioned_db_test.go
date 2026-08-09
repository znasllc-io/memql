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

// maxWalkPages bounds every cursor walk in this file. It is an assertable
// ceiling, not a convenience: a broken enumeration can hand back a non-empty
// cursor forever (the descending duplicate loop below), and a walk that hits
// this bound is a failure, never a completed walk.
const maxWalkPages = 200

// versionedPageQuery is keysetPageQuery with the sort direction spelled out.
// Both directions are under test here: descending loses termination when the
// enumeration repeats an id, ascending loses rows when it skips one.
func versionedPageQuery(owner string, pageSize int, direction string) string {
	return fmt.Sprintf(
		`sort(paginate(concept==%s;createdBy==%q, %d), "createdAt", %q)`,
		keysetConcept, owner, pageSize, direction)
}

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
	direction string,
) ([]string, []int) {
	t.Helper()
	var all []string
	var sizes []int
	cursor := ""
	q := versionedPageQuery(owner, pageSize, direction)
	for page := 0; page < maxWalkPages; page++ {
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
			return all, sizes
		}
		cursor = next
	}
	t.Fatalf("cursor walk did not terminate in %d pages (pageSize %d, %s); page sizes %v",
		maxWalkPages, pageSize, direction, sizes)
	return all, sizes
}

// unpagedIDs reads the whole set in ONE call, which is the reference the paged
// walk has to reproduce exactly.
func unpagedIDs(
	t *testing.T,
	ctx context.Context,
	eng *MemQLEngine,
	owner string,
	direction string,
) []string {
	t.Helper()
	res, err := eng.Execute(ContextWithCursor(ctx, ""), versionedPageQuery(owner, 500, direction))
	require.NoError(t, err)
	return pageIDs(t, res)
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

	got, sizes := walkPagesByCursor(t, ctx, eng, owner, pageSize, "desc")
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

	got, sizes := walkPagesByCursor(t, ctx, eng, owner, 1, "asc")
	reached := dedupe(got)

	require.ElementsMatch(t, []string{idX, idY, idZ}, reached,
		"the walk must reach every id; a cursor minted from a collapsed row jumps past the scan position (pages %v)",
		sizes)
}

// TestKeysetPagination_MixedUpdateFrequencyDescendingNoDuplicate is the same
// four-row / three-id shape read DESCENDING, which fails the opposite way.
//
// Descending, the collapsed row IS the scan position for the first id (its
// latest version is the newest row overall), so nothing is skipped. What
// happens instead is that X's OLD row at t=1 still sits below the cursor: the
// continuation scans it, resolves it to X's latest, and returns X a SECOND
// time. Every page therefore keeps finding work and the walk does not
// terminate -- reported on memql#3388 as 200 pages of 10 against a 30-id set.
func TestKeysetPagination_MixedUpdateFrequencyDescendingNoDuplicate(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-scanpos-desc")
	sfx := uniqueSuffix("keyset-scanpos-desc")
	owner := "kb:" + sfx
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	idX := fmt.Sprintf("v1:cognition:utterance:%s-X", sfx)
	idY := fmt.Sprintf("v1:cognition:utterance:%s-Y", sfx)
	idZ := fmt.Sprintf("v1:cognition:utterance:%s-Z", sfx)

	seedKeysetRow(t, ctx, db, idX, base.Add(1*time.Second), owner)
	seedKeysetRow(t, ctx, db, idY, base.Add(2*time.Second), owner)
	seedKeysetRow(t, ctx, db, idZ, base.Add(3*time.Second), owner)
	seedKeysetRow(t, ctx, db, idX, base.Add(100*time.Second), owner)

	got, sizes := walkPagesByCursor(t, ctx, eng, owner, 1, "desc")

	require.Equal(t, []string{idX, idZ, idY}, got,
		"the descending walk must emit each id exactly once, newest-latest-version first (pages %v)", sizes)
}

// TestKeysetPagination_DescendingVersionedWalkTerminates is the separate
// descending defect reported on memql#3388: over versioned rows the walk never
// terminated, returning pages long past exhaustion.
//
// 30 distinct ids at 6 clustered versions each, paged at 10, must finish in
// ceil(30/10)+1 = 4 pages. walkPagesByCursor fails the test outright if the
// cursor is still non-empty after maxWalkPages, so non-termination cannot be
// mistaken for a slow pass.
func TestKeysetPagination_DescendingVersionedWalkTerminates(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-desc-terminate")
	sfx := uniqueSuffix("keyset-desc-terminate")
	owner := "kb:" + sfx

	const (
		distinct = 30
		versions = 6
		pageSize = 10
	)

	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	want := make([]string, 0, distinct)
	for i := 0; i < distinct; i++ {
		id := fmt.Sprintf("v1:cognition:utterance:%s-%02d", sfx, i)
		want = append(want, id)
		for v := 0; v < versions; v++ {
			seedKeysetRow(t, ctx, db, id, base.Add(time.Duration(i*versions+v)*time.Second), owner)
		}
	}

	got, sizes := walkPagesByCursor(t, ctx, eng, owner, pageSize, "desc")

	require.Len(t, got, distinct,
		"the descending walk must emit exactly %d rows, one per id; pages were %v", distinct, sizes)
	require.ElementsMatch(t, want, got,
		"the descending walk must reach every distinct id exactly once (pages %v)", sizes)
	require.LessOrEqual(t, len(sizes), distinct/pageSize+1,
		"the descending walk must finish in ceil(distinct/pageSize)+1 pages; pages were %v", sizes)
}

// TestKeysetPagination_PagedWalkEqualsUnpagedRead is the property the whole
// issue reduces to: paging to exhaustion returns exactly the same row set as
// ONE unpaged read, for EVERY page size, with no overlap and no gap.
//
// Both directions and page sizes from 1 (every boundary is a page boundary) up
// past the distinct count (no pagination at all) run against the same
// clustered, mixed-frequency set. A page size of 1 is the sharpest case: it
// puts a cursor between every pair of adjacent rows, so any enumeration that
// does not agree with the result set shows up immediately.
func TestKeysetPagination_PagedWalkEqualsUnpagedRead(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-keyset-parity")
	sfx := uniqueSuffix("keyset-parity")
	owner := "kb:" + sfx

	const distinct = 24

	// Clustered versions, and deliberately UNEVEN: id i carries i%5+1 versions,
	// so update frequency varies across the set the way it does in production
	// (a status writer next to a write-once row). Every version of an id stays
	// contiguous in createdAt.
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	tick := 0
	for i := 0; i < distinct; i++ {
		id := fmt.Sprintf("v1:cognition:utterance:%s-%02d", sfx, i)
		for v := 0; v <= i%5; v++ {
			seedKeysetRow(t, ctx, db, id, base.Add(time.Duration(tick)*time.Second), owner)
			tick++
		}
	}

	for _, direction := range []string{"asc", "desc"} {
		reference := unpagedIDs(t, ctx, eng, owner, direction)
		require.Len(t, reference, distinct,
			"the unpaged %s read must return every distinct id", direction)

		for _, pageSize := range []int{1, 2, 3, 5, 7, 10, 24, 50} {
			t.Run(fmt.Sprintf("%s/pageSize=%d", direction, pageSize), func(t *testing.T) {
				got, sizes := walkPagesByCursor(t, ctx, eng, owner, pageSize, direction)

				require.Equal(t, reference, got,
					"the paged %s walk at pageSize %d must reproduce the unpaged read exactly -- same rows, same order, no overlap, no gap (pages %v)",
					direction, pageSize, sizes)
				require.Len(t, dedupe(got), len(got),
					"no id may appear twice across the walk (pages %v)", sizes)
			})
		}
	}
}

// TestKeysetPagination_SortPaginateNestingIsOrderIndependent answers the last
// open item on memql#3388: whether `paginate` inside `sort` is the intended
// chain, since the SDK builds `sort(paginate(...), ...)` while the DSL struct
// form compiles to `shape(paginate(sort(<filter>), 50), ...)` -- the opposite
// nesting.
//
// Neither is more correct, because the nesting carries no meaning.
// applyDirectiveWrappers peels the directive wrappers in a loop and lifts each
// one onto a FLAT field of the QueryPlan (plan.Sort, plan.Limit), so both
// spellings produce the same plan and the same execution. The ordering is
// applied once, in SQL, and the window is applied once, as the LIMIT over it.
//
// This is a plan-level assertion rather than a comment because the property is
// worth keeping: if directive nesting ever becomes significant, the SDK's
// documented chain and the DSL's compiled chain would start to disagree
// silently, and this fails first.
func TestKeysetPagination_SortPaginateNestingIsOrderIndependent(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)

	// The SDK's conceptBrowser chain, and the DSL struct form's chain.
	sdkChain := fmt.Sprintf(`sort(paginate(concept==%s, 10), "createdAt", "asc")`, keysetConcept)
	dslChain := fmt.Sprintf(`paginate(sort(concept==%s, "createdAt", "asc"), 10)`, keysetConcept)

	sdkPlan, err := eng.Parse(sdkChain)
	require.NoError(t, err)
	dslPlan, err := eng.Parse(dslChain)
	require.NoError(t, err)

	require.Equal(t, dslPlan.Sort, sdkPlan.Sort, "both nestings must lift the same sort onto the plan")
	require.Equal(t, dslPlan.Limit, sdkPlan.Limit, "both nestings must lift the same window onto the plan")
	require.NotNil(t, sdkPlan.Limit)
	require.Equal(t, 10, *sdkPlan.Limit)

	// And the sort signature -- which the cursor is bound to -- is identical,
	// so a cursor minted through one chain replays through the other.
	sdkSorter, err := compileSortFields(sdkPlan.Sort)
	require.NoError(t, err)
	dslSorter, err := compileSortFields(dslPlan.Sort)
	require.NoError(t, err)
	require.Equal(t, dslSorter.signatureValue(), sdkSorter.signatureValue(),
		"a cursor minted under one nesting must replay under the other")
}
