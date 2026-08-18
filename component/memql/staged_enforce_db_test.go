package memql

// staged_enforce_db_test.go -- the staged-DATA read gate (epic memql#3974,
// task memql#3983) against a REAL engine and a REAL Postgres.
//
// The claims here are claims about what a SCAN RETURNS, and a fake store
// cannot vouch for any of them. Three things need a database to be true or
// false:
//
//   - that a staged concept's rows are actually withheld, and that a live
//     concept's rows sitting beside them are actually not;
//   - that the loadLatestNodes SWAP does not leak. This is the one that passes
//     a green SQL-level test suite and leaks in production, so it gets the
//     two-version fixture below and a positive control that proves the fixture
//     can fail;
//   - that the conjunct rides INSIDE the DISTINCT ON collapse, which is what
//     makes the scan yield a candidate at all for the swap fixture.
//
// Postgres-gated via readMergeTestEngine -- the PRIVATE boot, deliberately,
// while most of the package borrows sharedReadMergeEngine (memql#4075): these
// tests toggle concept-data staging on a CORE concept and assert both the
// marked and the unmarked reading. The marker is in-memory engine state, and
// the first test clears it in normal flow rather than in a t.Cleanup, so on a
// shared engine one mid-test assertion failure would strand the mark and hide
// v1:cognition:utterance rows from every borrower after it. Skips when no DB
// is reachable, like every other _db_ test in this package. Each test carries
// a per-process unique createdBy scope so concurrent runs never collide, and
// nothing truncates.

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

// The two concepts the fixtures move rows between. Both are real, registered
// concepts -- the read-isolation check in compileConceptComparison rejects an
// unregistered concept on the `==` arm, so a made-up name could not be queried.
const (
	stagedDBConceptStaged = "v1:cognition:utterance"
	stagedDBConceptLive   = "v1:cognition:participant"
)

// seedStagedRow inserts ONE append-only version directly, so the fixture
// controls the exact (id, createdAt, concept) triple. Bypasses the mutation
// validators deliberately: these tests are about what the READ path returns,
// and the write path is memql#3985.
//
// Rows are scoped by the createdBy intrinsic -- a plain, non-relationship
// column -- so each run queries only its own rows without tripping the FK
// auto-canonicalization a payload relationship field would carry.
func seedStagedRow(t *testing.T, ctx context.Context, db *bun.DB, id, concept string, createdAt time.Time, owner string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"marker": id})
	require.NoError(t, err)
	node := &memorynodes.MemoryNode{
		ID:         id,
		Concept:    concept,
		CreatedBy:  owner,
		CreatedAt:  createdAt.UTC(),
		Payload:    payload,
		Provenance: json.RawMessage(`{"kind":"direct","name":"staged-enforce-test"}`),
	}
	_, err = db.NewInsert().Model(node).
		On(`CONFLICT (id, "createdAt") DO NOTHING`).
		Exec(ctx)
	require.NoError(t, err, "seed staged fixture row %s@%s", id, createdAt)
}

// stagedScopeQuery reads every row this run seeded, WITHOUT naming a concept.
//
// Deliberately concept-agnostic: a `concept==<live>` filter would exclude the
// staged rows on its own and prove nothing about the gate. This is also the
// shape that exercises the gate the way the unbound constructs do -- memql#3981
// measured 115 of 619 binding no concept at all.
func stagedScopeQuery(owner string) string {
	return fmt.Sprintf(`createdBy==%q`, owner)
}

func executedIDs(t *testing.T, res *ExecuteResult) []string {
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

// TestStagedData_RowsHiddenWhileLiveConceptRowsAreNot is the headline claim of
// the whole epic, stated as an experiment with one variable.
//
// The SAME query runs twice over the SAME rows. The only thing that changes
// between them is the staged marker on ONE concept. Before: everything is
// visible, which is what makes the "after" meaningful -- it rules out the
// reading where the rows were unreachable for some unrelated reason.
func TestStagedData_RowsHiddenWhileLiveConceptRowsAreNot(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-staged-hidden")
	sfx := uniqueSuffix("staged-hidden")
	owner := "sb:" + sfx
	base := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	liveId := fmt.Sprintf("%s:%s-live", stagedDBConceptLive, sfx)
	stagedId := fmt.Sprintf("%s:%s-staged", stagedDBConceptStaged, sfx)
	seedStagedRow(t, ctx, db, liveId, stagedDBConceptLive, base, owner)
	seedStagedRow(t, ctx, db, stagedId, stagedDBConceptStaged, base.Add(time.Second), owner)

	before, err := eng.Execute(ctx, stagedScopeQuery(owner))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{liveId, stagedId}, executedIDs(t, before),
		"both rows must be visible BEFORE staging, or the after-assertion proves nothing")

	eng.markConceptDataStaged(stagedDBConceptStaged)

	after, err := eng.Execute(ctx, stagedScopeQuery(owner))
	require.NoError(t, err)
	require.Equal(t, []string{liveId}, executedIDs(t, after),
		"the staged concept's rows must be withheld and the live concept's rows must NOT be -- an untrained-but-live concept is unaffected by another concept's staging")

	// One-way in the tier, but the marker is in-memory state and clearing it
	// must restore visibility on the NEXT read rather than at the next restart:
	// there is no plan cache, so nothing can hold a stale conjunct.
	eng.clearConceptDataStaging(stagedDBConceptStaged)
	restored, err := eng.Execute(ctx, stagedScopeQuery(owner))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{liveId, stagedId}, executedIDs(t, restored),
		"a concept whose data goes live must be readable on the next read, with no restart")
}

// TestStagedData_LoadLatestNodesSwapDoesNotLeak is the trap, pinned.
//
// THE FIXTURE. Row X carries two versions: an older one under a LIVE concept
// and a newer one under the STAGED concept. That is what makes the swap
// observable, because it decouples "the version the scan selected" from "the
// version the reload returns":
//
//	v1 @ t1  concept = live      <- what the SQL scan selects, because the
//	                                injected conjunct excludes v2 INSIDE the
//	                                DISTINCT ON collapse
//	v2 @ t2  concept = staged    <- what loadLatestNodes returns, because it
//	                                filters on `id IN (?)` and NOTHING else,
//	                                and what latestMatchingNodes then ASSIGNS
//	                                over the candidate
//
// A gate that lives only in the emitted SQL is green here and returns v2 -- a
// staged row -- because the SQL was never consulted about the row that was
// actually handed back. The subtest below drives latestMatchingNodes directly
// with a conjunct-FREE expression, which is exactly that "SQL-only gate"
// situation, and includes a positive control proving the call DOES return v2
// when nothing is staged.
func TestStagedData_LoadLatestNodesSwapDoesNotLeak(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-staged-swap")
	sfx := uniqueSuffix("staged-swap")
	owner := "sb:" + sfx
	base := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)

	// The two-version row. Its id is spelled under the LIVE concept because an
	// id is `{concept}:{shortId}`; the point of the fixture is that the CONCEPT
	// COLUMN of its newest version says otherwise.
	swapId := fmt.Sprintf("%s:%s-swap", stagedDBConceptLive, sfx)
	seedStagedRow(t, ctx, db, swapId, stagedDBConceptLive, base, owner)
	seedStagedRow(t, ctx, db, swapId, stagedDBConceptStaged, base.Add(time.Minute), owner)

	// A control row that is live in every version and must survive everything.
	controlId := fmt.Sprintf("%s:%s-control", stagedDBConceptLive, sfx)
	seedStagedRow(t, ctx, db, controlId, stagedDBConceptLive, base, owner)

	t.Run("before staging the newest version is what the read returns", func(t *testing.T) {
		res, err := eng.Execute(ctx, stagedScopeQuery(owner))
		require.NoError(t, err)
		require.ElementsMatch(t, []string{swapId, controlId}, executedIDs(t, res))
	})

	eng.markConceptDataStaged(stagedDBConceptStaged)
	t.Cleanup(func() { eng.clearConceptDataStaging(stagedDBConceptStaged) })

	t.Run("end to end, the swapped-in staged version does not reach the caller", func(t *testing.T) {
		res, err := eng.Execute(ctx, stagedScopeQuery(owner))
		require.NoError(t, err)
		require.Equal(t, []string{controlId}, executedIDs(t, res),
			"the row whose CURRENT version is staged must be absent; returning it would be the loadLatestNodes swap leaking")
	})

	// The isolation. `expr` here carries NO staged conjunct -- it is the
	// unbound-plan case, and the case a reviewer would assume is covered by
	// the injection. If the swap co-gate in latestMatchingNodes were removed,
	// this returns the staged v2.
	conjunctFree := &ComparisonExpression{
		Field:    FieldReference{Raw: "createdBy", Parts: []string{"createdBy"}},
		Operator: OpEq,
		Value:    owner,
	}
	scanned := []memorynodes.MemoryNode{{ID: swapId, Concept: stagedDBConceptLive, CreatedBy: owner, CreatedAt: base}}

	t.Run("the co-gate holds with no conjunct in the expression", func(t *testing.T) {
		got, err := eng.latestMatchingNodes(ctx, scanned, conjunctFree, nil, 0)
		require.NoError(t, err)
		require.Empty(t, got,
			"latestMatchingNodes reloaded the staged newest version and swapped it in; with no conjunct in `expr` the row gate is the ONLY thing that can drop it")
	})

	t.Run("positive control: the same call returns the staged version when nothing is staged", func(t *testing.T) {
		eng.clearConceptDataStaging(stagedDBConceptStaged)
		defer eng.markConceptDataStaged(stagedDBConceptStaged)

		got, err := eng.latestMatchingNodes(ctx, scanned, conjunctFree, nil, 0)
		require.NoError(t, err)
		require.Len(t, got, 1, "the fixture must be capable of returning the swapped-in version, or the assertion above passes vacuously")
		require.Equal(t, stagedDBConceptStaged, got[0].Concept,
			"the reload really does replace the scanned live version with the newest one -- this is the swap the co-gate exists for")
	})
}

// TestStagedData_ConjunctRidesInsideTheCollapse pins the position of the
// injected conjunct relative to the DISTINCT ON (id) collapse.
//
// With the conjunct INSIDE the subquery (where the author's filter and the
// asOf timestamp already ride), the collapse runs over rows the conjunct
// already admitted, so for the two-version fixture it yields the older LIVE
// version as the scan candidate. That is observable: the scan produces a
// candidate for the row, which the reload then swaps and the gate then drops.
//
// The assertion is on the SQL the conjunct compiles into and on where the
// executor puts it, because the end-to-end answer alone cannot distinguish the
// positions -- both end with the row absent, by different routes.
func TestStagedData_ConjunctRidesInsideTheCollapse(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-staged-collapse")
	sfx := uniqueSuffix("staged-collapse")
	owner := "sb:" + sfx
	base := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)

	rowId := fmt.Sprintf("%s:%s-two-version", stagedDBConceptLive, sfx)
	seedStagedRow(t, ctx, db, rowId, stagedDBConceptLive, base, owner)
	seedStagedRow(t, ctx, db, rowId, stagedDBConceptStaged, base.Add(time.Minute), owner)

	eng.markConceptDataStaged(stagedDBConceptStaged)
	t.Cleanup(func() { eng.clearConceptDataStaging(stagedDBConceptStaged) })

	// The conjunct is part of plan.Root, and executeCombinedFilterQuery puts
	// the compiled root INSIDE the DISTINCT ON subquery alongside the asOf
	// predicate. Compiling the plan is what proves it is a filter conjunct
	// rather than a post-pass.
	plan, err := eng.parseWithFunctions(stagedScopeQuery(owner), nil, nil, false)
	require.NoError(t, err)

	conjunction, ok := plan.Root.(*LogicalExpression)
	require.True(t, ok, "the plan root must be the author's filter ANDed with the staged conjunct")
	require.Equal(t, LogicalAnd, conjunction.Op)

	combined, ok := eng.tryCompileCombinedFilter(ctx, plan.Root, "")
	require.True(t, ok, "the whole root including the conjunct must compile to ONE SQL filter; if it does not, the executor falls back to per-branch queries intersected in Go and the pushdown is gone")
	require.Contains(t, combined.sql, "IS DISTINCT FROM",
		"the staged conjunct must be part of the single compiled filter that rides inside the DISTINCT ON collapse")

	// And the end-to-end answer is still the correct one.
	res, err := eng.Execute(ctx, stagedScopeQuery(owner))
	require.NoError(t, err)
	require.Empty(t, executedIDs(t, res),
		"the row's current version is staged, so it is withheld regardless of the older live version the collapse surfaced")
}
