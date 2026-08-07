package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// seed_sweep_userlist_db_test.go -- memql#3217, the half that needs Postgres.
//
// The unit test next door (seed_materializer_rowids_test.go) pins the
// extraction seam: extractRowIds now reads the *ExecuteResult that
// engine.Execute actually returns instead of falling through to nil. That is
// the defect, but it is not the deliverable -- the deliverable is a startup
// sweep that enumerates the users it is supposed to enumerate, and no
// in-memory assertion can establish that, because everything interesting
// happens inside the query engine.
//
// So this drives the real listUserIds against a real database, over a user set
// shaped like the one that breaks it:
//
//	60 active users, older than "now", one of them carrying extra versions.
//
// Every part of that shape is load-bearing:
//
//   - 60 > the `paginate 50` on activeUsers, which listUserIds used to call.
//   - OLDER, because activeUsers sorts `row.createdAt desc` -- so the users a
//     paged read drops are the OLDEST, and the oldest users are exactly the
//     population the sweep exists to serve. A user created after a seed was
//     added already gets its rows from the
//     graph.node.created.v1:identity:user subscription.
//   - The extra VERSIONS, because the engine fills a page from `target*2`
//     PHYSICAL version rows and dedupes to logical rows afterwards. Under
//     churn a paged read returns a short page that looks exhausted, which is
//     the failure memql#3209 hit on allAgents and the reason following the
//     cursor is not a fix either. v1:identity:user churns on every login
//     (lastSeenAt), so this is the normal state of the concept, not a corner.
//
// Postgres-gated via readMergeTestEngine: skips when no DB is reachable, and
// MEMQL_REQUIRE_DB=1 in the db-tests lane converts that skip into a failure.

const seedSweepUserConcept = "v1:identity:user"

// seedActiveUserRow inserts one v1:identity:user version directly, bypassing
// the mutation validators -- the sweep only reads, and what is under test is
// which rows the READ returns. createdBy carries the run's suffix so the test
// can find its own rows in a shared database.
func seedActiveUserRow(t *testing.T, ctx context.Context, db *bun.DB, id string, createdAt time.Time, marker string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"id":           id,
		"displayName":  "Sweep Probe " + id,
		"primaryEmail": id + "@example.invalid",
		"role":         "reader",
		"active":       true,
		"lastSeenAt":   createdAt.UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	node := &memorynodes.MemoryNode{
		ID:         id,
		Concept:    seedSweepUserConcept,
		CreatedBy:  marker,
		CreatedAt:  createdAt.UTC(),
		Payload:    payload,
		Provenance: json.RawMessage(`{"kind":"direct","name":"seed-sweep-test"}`),
	}
	_, err = db.NewInsert().Model(node).
		On(`CONFLICT (id, "createdAt") DO NOTHING`).
		Exec(ctx)
	require.NoError(t, err, "seed user row %s", id)
}

// TestSeedSweepListUserIdsSeesEveryActiveUser is the memql#3217 deliverable.
//
// Against untouched main it fails on the FIRST assertion with zero ids --
// listUserIds handed extractRowIds an *ExecuteResult, which fell to
// `default: return nil`, so the startup per-user seed sweep materialized
// nothing on every boot and "no users exist" was indistinguishable from
// "we could not read the answer".
//
// With only the type-switch arm fixed it fails on the SECOND assertion, which
// is why this change is not one line: activeUsers is a newest-first page of 50
// and cannot answer this question.
func TestSeedSweepListUserIdsSeesEveryActiveUser(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := context.Background()

	sfx := uniqueSuffix("seed-sweep")
	marker := "ssweep:" + sfx

	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("concept = ?", seedSweepUserConcept).
			Where(`"createdBy" = ?`, marker).
			Exec(context.Background())
	})

	// Deliberately in the PAST: a paged, newest-first read reaches the most
	// recently created users, and these are not those.
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 60
	want := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("v1:identity:user:%s-%02d", sfx, i)
		seedActiveUserRow(t, ctx, db, id, base.Add(time.Duration(i)*time.Minute), marker)
		want[id] = true
	}

	// Version churn on one row, the shape a login writes: same logical user,
	// several physical versions. The engine's page is filled from physical
	// rows and deduped afterwards, so churn shortens a page.
	churned := fmt.Sprintf("v1:identity:user:%s-00", sfx)
	for v := 1; v <= 20; v++ {
		seedActiveUserRow(t, ctx, db, churned, base.Add(time.Duration(v)*time.Hour), marker)
	}

	sm := eng.SeedMaterializer()
	require.NotNil(t, sm, "engine exposes no seed materializer")

	ids, err := sm.listUserIds(ctx)
	require.NoError(t, err, "listUserIds")

	// FIRST assertion: the extraction seam. Zero ids here is memql#3217 in its
	// original form -- the sweep reading nothing at all.
	require.NotEmpty(t, ids,
		"listUserIds returned NO users against a database that has them. engine.Execute "+
			"returns *ExecuteResult and extractRowIds must have an arm for it; without one the "+
			"startup per-user seed sweep is a silent no-op on every boot. memql#3217.")

	got := make(map[string]bool, len(ids))
	for _, id := range ids {
		got[id] = true
	}

	// SECOND assertion: completeness. Every seeded user, not the newest page
	// of them.
	missing := make([]string, 0)
	for id := range want {
		if !got[id] {
			missing = append(missing, id)
		}
	}
	require.Emptyf(t, missing,
		"listUserIds returned %d ids and missed %d of the %d users this test seeded.\n\n"+
			"A sweep set is not a UI page. activeUsers is `sort row.createdAt desc` + "+
			"`paginate 50`, so it drops the OLDEST users -- precisely the ones the sweep exists "+
			"to backfill, since newer users get their perUser seeds from the "+
			"graph.node.created.v1:identity:user subscription instead. Version churn shortens "+
			"that page further, because the engine fills it from physical rows and dedupes "+
			"after. usersForSeedSweep is the complete-set sibling. memql#3217.",
		len(ids), len(missing), total)

	// The churned user must appear ONCE. A sweep that materialized a user's
	// per-user seeds N times for N versions would be a different bug wearing
	// this one's clothes.
	occurrences := 0
	for _, id := range ids {
		if id == churned {
			occurrences++
		}
	}
	require.Equalf(t, 1, occurrences,
		"the churned user appears %d times in the sweep set; a logical row must be swept once "+
			"regardless of how many versions it carries", occurrences)
}

// The sweep must not enumerate soft-deleted users. usersForSeedSweep filters
// isActiveRecord, same as activeUsers -- widening the read was not the point
// of memql#3217 and an inactive user acquiring per-user seed rows at boot
// would be a regression, not a fix.
func TestSeedSweepListUserIdsSkipsInactiveUsers(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := context.Background()

	sfx := uniqueSuffix("seed-sweep-inactive")
	marker := "ssweep:" + sfx

	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("concept = ?", seedSweepUserConcept).
			Where(`"createdBy" = ?`, marker).
			Exec(context.Background())
	})

	base := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	activeId := "v1:identity:user:" + sfx + "-active"
	inactiveId := "v1:identity:user:" + sfx + "-inactive"

	seedActiveUserRow(t, ctx, db, activeId, base, marker)

	payload, err := json.Marshal(map[string]any{
		"id":           inactiveId,
		"displayName":  "Sweep Probe Inactive",
		"primaryEmail": "inactive@example.invalid",
		"role":         "reader",
		"active":       false,
	})
	require.NoError(t, err)
	_, err = db.NewInsert().Model(&memorynodes.MemoryNode{
		ID:         inactiveId,
		Concept:    seedSweepUserConcept,
		CreatedBy:  marker,
		CreatedAt:  base.UTC(),
		Payload:    payload,
		Provenance: json.RawMessage(`{"kind":"direct","name":"seed-sweep-test"}`),
	}).On(`CONFLICT (id, "createdAt") DO NOTHING`).Exec(ctx)
	require.NoError(t, err)

	ids, err := eng.SeedMaterializer().listUserIds(ctx)
	require.NoError(t, err)

	// POSITIVE CONTROL. Without it, "the inactive user is absent" is satisfied
	// by a sweep that returned nothing at all -- which is the exact defect
	// this file exists to catch.
	require.Contains(t, ids, activeId,
		"the ACTIVE user is missing from the sweep set, so the negative assertion below "+
			"would pass vacuously")
	require.NotContains(t, ids, inactiveId,
		"usersForSeedSweep filters isActiveRecord; a soft-deleted user must not acquire "+
			"per-user seed rows at boot")
}

// listUserIds runs under the seed materializer's synthetic system actor, at
// boot, before any request exists. usersForSeedSweep is @serverOnly, so the
// call must also stamp an internal origin -- without it the read is refused and
// the sweep fails loudly rather than silently, but it still fails.
func TestSeedSweepListUserIdsNeedsNoRequestActor(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := context.Background()

	sfx := uniqueSuffix("seed-sweep-actor")
	marker := "ssweep:" + sfx
	probeId := "v1:identity:user:" + sfx + "-probe"

	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("concept = ?", seedSweepUserConcept).
			Where(`"createdBy" = ?`, marker).
			Exec(context.Background())
	})
	seedActiveUserRow(t, ctx, db, probeId, time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), marker)

	// Deliberately bare: no AccessContext, no TokenInfo. This is what the
	// startup goroutine actually has.
	bare, err := eng.SeedMaterializer().listUserIds(context.Background())
	require.NoError(t, err,
		"listUserIds must work from a bare startup context -- it supplies its own system actor "+
			"and internal origin. If this errors, the sweep never runs on any boot.")
	require.Contains(t, bare, probeId,
		"a user seeded by this test is absent from a bare-context sweep")

	// The same call under a request-shaped context must not behave
	// differently: the sweep is not caller-scoped and must not become so.
	// Asserted per-row rather than by count -- the database is shared with
	// the other db-gated packages, so a global count is not stable.
	userCtx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:not-the-sweeper",
		Role:   auth.RoleReader,
	})
	underUser, err := eng.SeedMaterializer().listUserIds(userCtx)
	require.NoError(t, err)
	require.Contains(t, underUser, probeId,
		"the sweep dropped a user when a reader's AccessContext happened to be on the context. "+
			"It must be caller-independent: it runs before any caller exists, and scoping it to "+
			"actor.userId would evaluate against an empty actor and sweep nobody.")
}
