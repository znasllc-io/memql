package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// THE ASSIGNMENT OF EXISTING ROWS, AGAINST REAL TABLES (account_assignment.go).
//
// Every fixture row is written straight into the node table with no accountId,
// the shape a row written before this release has, and the job is then run the
// way the runner runs it, call after call, until every concept has finished a
// pass. The assertions are the exact account of every fixture row, the conflicts
// recorded, and the rows the job must not touch.
//
// The job walks every engine-managed concept of the database, so rows other
// tests in this package left behind are assigned too. That is the job doing
// what it does; the assertions name only this file's rows. Postgres-gated like
// their neighbours.

type assignmentFixture struct {
	suffix                 string
	acme, beta             string
	alice, bob, olga, nora string
	goal, run, step        string
	file, artifact         string
	store1, store2         string
	product1, product2     string
	channel, resynced      string
	todoAlice, todoBob     string
	todoOlga, todoNora     string
	skill, campaign        string
	goalForeignTag         string
	goalOperatorTag        string
	goalOldAt              time.Time
}

func newAssignmentFixture(t *testing.T, engine *MemQLEngine, db *bun.DB, name string) assignmentFixture {
	t.Helper()
	ctx := context.Background()
	s := uniqueSuffix(name)
	f := assignmentFixture{suffix: s, acme: "acme-" + s, beta: "beta-" + s,
		alice: "alice-" + s, bob: "bob-" + s, olga: "olga-" + s, nora: "nora-" + s}

	// The people. alice is in acme; bob joined beta before acme; olga is a
	// cluster operator; nora is in no group.
	seedGroup(t, engine, "g-"+f.acme, f.acme, "account", f.acme)
	seedGroup(t, engine, "g-"+f.beta, f.beta, "account", f.beta)
	for _, p := range []struct {
		id   string
		role auth.Role
	}{{f.alice, auth.RoleWriter}, {f.bob, auth.RoleWriter}, {f.olga, auth.RoleOwner}, {f.nora, auth.RoleWriter}} {
		seedPrincipal(t, engine, p.id, p.role)
	}
	seedMembership(t, engine, "g-"+f.acme, f.alice, "active")
	seedMembership(t, engine, "g-"+f.beta, f.bob, "active")
	betweenMembershipWrites()
	seedMembership(t, engine, "g-"+f.acme, f.bob, "active")

	now := time.Now().UTC().Truncate(time.Microsecond)
	at := func(back time.Duration) time.Time { return now.Add(-back) }
	raw := func(concept, id string, when time.Time, payload map[string]any) string {
		full := concept + ":" + id
		seedRawRow(t, ctx, db, concept, full, when, "assignment-test", payload)
		return full
	}

	// The work spine: a goal with an older version, its run and a step.
	f.goalOldAt = at(3 * time.Hour)
	f.goal = raw("v1:work:goal", "goal-"+s, f.goalOldAt, map[string]any{"ownerUserId": f.alice, "title": "old"})
	raw("v1:work:goal", "goal-"+s, at(2*time.Hour), map[string]any{"ownerUserId": f.alice, "title": "new", "accountId": ""})
	f.run = raw("v1:work:run", "run-"+s, at(time.Hour), map[string]any{"ownerUserId": f.alice, "goalId": "goal-" + s})
	f.step = raw("v1:work:step", "step-"+s, at(time.Hour), map[string]any{"ownerUserId": f.alice, "runId": f.run})

	// The Library: a file, and the index row tagged for two accounts.
	f.file = raw("v1:library:file", "file-"+s, at(time.Hour), map[string]any{"ownerUserId": f.alice, "name": "contract.pdf"})
	f.artifact = raw("v1:library:artifact", "artifact-"+s, at(time.Hour), map[string]any{
		"ownerUserId": f.alice, "sourceConceptRef": f.file, "accountIds": []any{f.beta, f.acme},
	})

	// Tags are held to the accounts their owner may place a row in. alice is in
	// acme only, so her goal tagged for beta alone is not filed under beta; olga
	// is a cluster operator, so her goal tagged for beta is.
	f.goalForeignTag = raw("v1:work:goal", "goal-foreign-"+s, at(time.Hour), map[string]any{
		"ownerUserId": f.alice, "title": "tagged elsewhere", "accountIds": []any{f.beta},
	})
	f.goalOperatorTag = raw("v1:work:goal", "goal-operator-"+s, at(time.Hour), map[string]any{
		"ownerUserId": f.olga, "title": "tagged for a client", "accountIds": []any{f.beta},
	})

	// Shopify: a store one site binds, and a store sites of two accounts bind.
	f.store1 = raw("v1:shopify:store", "store1-"+s, at(time.Hour), map[string]any{"domain": "one-" + s + ".myshopify.com"})
	f.store2 = raw("v1:shopify:store", "store2-"+s, at(time.Hour), map[string]any{"domain": "two-" + s + ".myshopify.com"})
	raw(conceptPlatformSite, "site1-"+s, at(time.Hour), map[string]any{
		"ownerUserId": f.alice, "accountId": f.acme, "binding": map[string]any{"storeId": "store1-" + s},
		"previewBinding": map[string]any{"storeId": "store2-" + s},
	})
	raw(conceptPlatformSite, "site2-"+s, at(time.Hour), map[string]any{
		"ownerUserId": f.bob, "accountId": f.beta, "binding": map[string]any{"storeId": "store2-" + s},
	})
	f.product1 = raw("v1:shopify:product", "product1-"+s, at(time.Hour), map[string]any{"storeId": "store1-" + s, "title": "Mug"})
	f.product2 = raw("v1:shopify:product", "product2-"+s, at(time.Hour), map[string]any{"storeId": "store2-" + s, "title": "Cup"})
	f.channel = raw(conceptShopifyChannel, "channel-"+s, at(time.Hour), map[string]any{
		"storeId": "store1-" + s, "accountId": "gid-shopify-account-77-" + s,
	})
	// A channel re-synced after the rename: the read-merge kept the old value
	// in accountId beside the new sync's sourceAccountId.
	f.resynced = raw(conceptShopifyChannel, "resynced-"+s, at(time.Hour), map[string]any{
		"storeId": "store1-" + s, "accountId": "gid-shopify-account-88-" + s, "sourceAccountId": "gid-shopify-account-88-new-" + s,
	})
	// The two accounts exist, as every account an id was written for does.
	for _, account := range []string{f.acme, f.beta} {
		raw(conceptAccountsAccount, account, at(4*time.Hour), map[string]any{"name": account, "status": "active"})
	}

	// Notes owned by each kind of person.
	f.todoAlice = raw("v1:todos:todo", "todo-alice-"+s, at(time.Hour), map[string]any{"ownerUserId": f.alice, "title": "a"})
	f.todoBob = raw("v1:todos:todo", "todo-bob-"+s, at(time.Hour), map[string]any{"ownerUserId": f.bob, "title": "b"})
	f.todoOlga = raw("v1:todos:todo", "todo-olga-"+s, at(time.Hour), map[string]any{"ownerUserId": f.olga, "title": "o"})
	f.todoNora = raw("v1:todos:todo", "todo-nora-"+s, at(time.Hour), map[string]any{"ownerUserId": f.nora, "title": "n"})

	// A seeded-style shared row, and a stated concept's row, neither with an
	// account.
	f.skill = raw("v1:skills:skill", "skill-"+s, at(time.Hour), map[string]any{"ownerUserId": "", "slug": "skill-" + s})
	f.campaign = raw("v1:campaigns:campaign", "campaign-"+s, at(time.Hour), map[string]any{"name": "Launch"})
	return f
}

// newestPayload is the newest version's payload of one row.
func newestPayload(t *testing.T, db *bun.DB, id string) map[string]any {
	t.Helper()
	var raw []byte
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT payload FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, id).Scan(&raw))
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func (f assignmentFixture) ids() []string {
	return []string{f.goal, f.run, f.step, f.file, f.artifact, f.store1, f.store2, f.product1, f.product2,
		f.channel, f.resynced, f.todoAlice, f.todoBob, f.todoOlga, f.todoNora, f.skill, f.campaign,
		f.goalForeignTag, f.goalOperatorTag}
}

// assertAssigned holds every fixture row to its exact end state.
func (f assignmentFixture) assertAssigned(t *testing.T, db *bun.DB) {
	t.Helper()
	want := map[string]string{
		f.goal: f.acme, f.run: f.acme, f.step: f.acme, // the owner's, then each parent's
		f.file: f.acme, f.artifact: f.acme, // the owner's account is among the tags
		f.store1: f.acme, f.product1: f.acme, f.channel: f.acme, f.resynced: f.acme, // the binding site's
		f.store2: "", f.product2: "", // never guessed, and its mirror waits with it
		f.todoAlice: f.acme, f.todoBob: f.beta, // one account; the oldest of two
		f.todoOlga: "self", f.todoNora: "self", // an operator; a person in no group
		f.skill: "", f.campaign: "", // shared and nobody's; stated and not ours
		// a tag its owner does not belong to files nothing there, so the owner
		// rule decides; an operator's tag is taken
		f.goalForeignTag: f.acme, f.goalOperatorTag: f.beta,
	}
	for id, account := range want {
		got := newestPayload(t, db, id)
		if account == "" {
			_, has := got["accountId"]
			require.False(t, has, "%s should carry no account, has %v", id, got["accountId"])
			continue
		}
		require.Equal(t, account, got["accountId"], "the account of %s", id)
	}
	channel := newestPayload(t, db, f.channel)
	require.Equal(t, "gid-shopify-account-77-"+f.suffix, channel["sourceAccountId"], "Shopify's value moved aside")
	resynced := newestPayload(t, db, f.resynced)
	require.Equal(t, "gid-shopify-account-88-new-"+f.suffix, resynced["sourceAccountId"], "the newer sync's value stays")

	var kinds []string
	rows, err := db.QueryContext(context.Background(), `SELECT row_id || ' ' || kind FROM account_assignment_conflict
		WHERE row_id LIKE ? AND resolved_at IS NULL ORDER BY 1`, "%"+f.suffix)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		kinds = append(kinds, k)
	}
	require.Equal(t, []string{f.store2 + " store_bound_twice"}, kinds)
}

// resetAssignmentProgress starts the job from nothing, as a fresh install does.
func resetAssignmentProgress(t *testing.T, db *bun.DB) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `DELETE FROM account_assignment_progress`)
	require.NoError(t, err)
}

// runAssignmentUntilSettled calls the job as the runner does until every
// engine-managed concept has finished a pass that began after since.
func runAssignmentUntilSettled(t *testing.T, engine *MemQLEngine, db *bun.DB, since time.Time) {
	t.Helper()
	_, order, err := assignmentConcepts()
	require.NoError(t, err)
	for i := 0; i < 2000; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), accountOwnershipDeadline)
		err := engine.runAccountAssignmentBatch(ctx)
		cancel()
		require.NoError(t, err)
		var open int
		require.NoError(t, db.QueryRowContext(context.Background(), `SELECT count(*) FROM unnest(?::text[]) c(concept)
			LEFT JOIN account_assignment_progress p USING (concept)
			WHERE p.pass_finished_at IS NULL OR p.pass_finished_at < ? OR p.pass_finished_at < p.pass_started_at`,
			pq.Array(order), since).Scan(&open))
		if open == 0 {
			return
		}
	}
	var open []string
	rows, err := db.QueryContext(context.Background(), `SELECT c.concept || ' started=' || COALESCE(p.pass_started_at::text, '-') ||
		' finished=' || COALESCE(p.pass_finished_at::text, '-') || ' next=' || COALESCE(p.next_at::text, '-')
		FROM unnest(?::text[]) c(concept) LEFT JOIN account_assignment_progress p USING (concept)
		WHERE p.pass_finished_at IS NULL OR p.pass_finished_at < ? OR p.pass_finished_at < p.pass_started_at`, pq.Array(order), since)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		open = append(open, line)
	}
	t.Fatalf("the assignment did not settle; still open:\n  %s", strings.Join(open, "\n  "))
}

// forceFullPasses makes every concept due for a full pass now.
func forceFullPasses(t *testing.T, db *bun.DB) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT clock_timestamp()`).Scan(&now))
	_, err := db.ExecContext(context.Background(), `UPDATE account_assignment_progress SET next_at = ?, full_pass_at = NULL`, now)
	require.NoError(t, err)
	return now
}

func snapshotPayloads(t *testing.T, db *bun.DB, ids []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := db.QueryContext(context.Background(), `SELECT id || '@' || "createdAt"::text, payload::text FROM "MemoryNodes"
		WHERE id = ANY(?::text[])`, pq.Array(ids))
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var k, v string
		require.NoError(t, rows.Scan(&k, &v))
		out[k] = v
	}
	return out
}

func TestAccountAssignmentAssignsLegacyRows(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	bus := events.NewBus()
	engine.SetEventBus(bus)
	t.Cleanup(bus.Close)

	f := newAssignmentFixture(t, engine, db, t.Name())
	goalOld := snapshotPayloads(t, db, []string{f.goal})

	// Nothing the job does is news: no graph event for any row.
	var published atomic.Int64
	var seen atomic.Value
	t.Cleanup(bus.Subscribe("graph.node.*", func(ev events.Event) {
		published.Add(1)
		seen.Store(ev.Topic)
	}))

	resetAssignmentProgress(t, db)
	start := forceFullPasses(t, db).Add(-time.Hour)
	runAssignmentUntilSettled(t, engine, db, start)
	f.assertAssigned(t, db)
	time.Sleep(100 * time.Millisecond) // the bus delivers on goroutines
	require.Zero(t, published.Load(), "the job published %v", seen.Load())

	t.Run("only the newest version changed", func(t *testing.T) {
		after := snapshotPayloads(t, db, []string{f.goal})
		require.Len(t, after, 2)
		for key, payload := range goalOld {
			if strings.HasPrefix(key, f.goal+"@") && !strings.Contains(payload, `"new"`) {
				require.Equal(t, payload, after[key], "the older version of the goal was rewritten")
				require.NotContains(t, after[key], "accountId")
			}
		}
	})

	t.Run("a second run to completion changes nothing", func(t *testing.T) {
		before := snapshotPayloads(t, db, f.ids())
		since := forceFullPasses(t, db)
		runAssignmentUntilSettled(t, engine, db, since)
		require.Equal(t, before, snapshotPayloads(t, db, f.ids()))
		f.assertAssigned(t, db)
	})
}

func TestAccountAssignmentResumesAfterAnInterruptedRun(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	f := newAssignmentFixture(t, engine, db, t.Name())
	resetAssignmentProgress(t, db)
	start := forceFullPasses(t, db).Add(-time.Hour)

	// One batch of one row, then a call whose deadline has already passed:
	// the second loses only its own transaction.
	_, order, err := assignmentConcepts()
	require.NoError(t, err)
	require.NoError(t, ensureAssignmentProgressRows(context.Background(), db, order))
	_, err = engine.assignOneBatch(context.Background(), db, order, 1)
	require.NoError(t, err)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	_ = engine.runAccountAssignmentBatch(gone)

	runAssignmentUntilSettled(t, engine, db, start)
	f.assertAssigned(t, db)
}

// The Library retirement fence refuses the update of a file whose bytes were
// permanently retired; the job leaves that row alone, records why, and assigns
// the rest of the batch.
func TestAccountAssignmentRecordsALibraryFenceRefusal(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	s := uniqueSuffix(t.Name())
	seedPrincipal(t, engine, "fence-owner-"+s, auth.RoleOwner)
	retired := "v1:library:file:retired-" + s
	kept := "v1:library:file:kept-" + s
	for _, id := range []string{retired, kept} {
		seedRawRow(t, ctx, db, "v1:library:file", id, time.Now().Add(-time.Hour), "assignment-test",
			map[string]any{"ownerUserId": "fence-owner-" + s, "name": id})
	}
	require.NoError(t, db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Scaffolding: the upload row's own admission trigger wants a whole
		// pipeline behind it, which says nothing about the fence.
		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO pipeline_artifact_uploads
			(intent_id, owner_user_id, identity, file_id, object_key, size_bytes, generation, state)
			VALUES (?, 'fence-owner', '{}'::jsonb, ?, ?, 1, 1, 'retired')`,
			"intent-"+s, "retired-"+s, "object-"+s)
		return err
	}))

	resetAssignmentProgress(t, db)
	start := forceFullPasses(t, db).Add(-time.Hour)
	runAssignmentUntilSettled(t, engine, db, start)

	require.Equal(t, "self", newestPayload(t, db, kept)["accountId"])
	_, has := newestPayload(t, db, retired)["accountId"]
	require.False(t, has, "the refused row was stamped")
	var kind string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT kind FROM account_assignment_conflict WHERE row_id = ?`, retired).Scan(&kind))
	require.Equal(t, conflictLibraryFenceRefused, kind)
	require.Equal(t, []string{"self"}, conflictOf(t, db, retired, conflictLibraryFenceRefused).candidates,
		"a refused row records the account the rules chose")
}

func TestAccountAssignmentIndexHasItsDefinition(t *testing.T) {
	_, db, _ := readMergeTestEngine(t)
	var def string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'idx_memorynodes_account'`).Scan(&def))
	require.Equal(t, fmt.Sprintf(`CREATE INDEX idx_memorynodes_account ON public."MemoryNodes" USING btree (((payload ->> 'accountId'::text)), concept) WHERE %s`,
		`(((payload ->> 'accountId'::text) IS NOT NULL) AND ((payload ->> 'accountId'::text) <> ''::text))`), def)
}

// assignmentProgressOf reads the fields of one progress row a test asserts on.
type assignmentProgressRow struct {
	passFull  bool
	waiting   int
	batchSize int
	nextAt    time.Time
}

func assignmentProgressOf(t *testing.T, db *bun.DB, concept string) assignmentProgressRow {
	t.Helper()
	var r assignmentProgressRow
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT pass_full, waiting, batch_size, next_at
		FROM account_assignment_progress WHERE concept = ?`, concept).Scan(&r.passFull, &r.waiting, &r.batchSize, &r.nextAt))
	return r
}

// ageLastFullPass makes a concept due now with its last full pass two days old,
// which is what a day passing looks like to the scheduler.
func ageLastFullPass(t *testing.T, db *bun.DB, concept string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE account_assignment_progress
		SET full_pass_at = full_pass_at - interval '2 days', next_at = clock_timestamp() WHERE concept = ?`, concept)
	require.NoError(t, err)
}

// runOnePassOf works one concept until its current pass finishes, and reports
// the ids the pass examined.
func runOnePassOf(t *testing.T, engine *MemQLEngine, db *bun.DB, concept string) int {
	t.Helper()
	examined := 0
	for i := 0; i < 1000; i++ {
		result, err := engine.assignOneBatch(context.Background(), db, []string{concept}, assignmentBatchIds)
		require.NoError(t, err)
		require.Equal(t, concept, result.concept, "the concept was not claimable")
		examined += result.examined
		var open bool
		require.NoError(t, db.QueryRowContext(context.Background(), `SELECT pass_finished_at IS NULL OR pass_finished_at < pass_started_at
			FROM account_assignment_progress WHERE concept = ?`, concept).Scan(&open))
		if !open {
			return examined
		}
	}
	t.Fatal("the pass did not finish")
	return examined
}

// A concept is walked in full again only while its last full pass left rows
// waiting on a parent, a day after that pass; one with nothing waiting is never
// re-walked, and its later passes look only at what was written since.
func TestAccountAssignmentRewalksOnlyWhileRowsWait(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	s := uniqueSuffix(t.Name())
	const todo, grant = "v1:todos:todo", "v1:platform:sitePreviewGrant"
	seedPrincipal(t, engine, "rewalk-owner-"+s, auth.RoleOwner)
	seedRawRow(t, ctx, db, todo, todo+":rewalk-"+s, time.Now().Add(-time.Hour), "assignment-test",
		map[string]any{"ownerUserId": "rewalk-owner-" + s, "title": "t"})
	// A site of a stated concept with no account yet, and a grant that follows it.
	site := conceptPlatformSite + ":rewalk-site-" + s
	seedRawRow(t, ctx, db, conceptPlatformSite, site, time.Now().Add(-time.Hour), "assignment-test",
		map[string]any{"ownerUserId": "rewalk-owner-" + s})
	child := grant + ":rewalk-grant-" + s
	seedRawRow(t, ctx, db, grant, child, time.Now().Add(-time.Hour), "assignment-test",
		map[string]any{"siteId": "rewalk-site-" + s})

	resetAssignmentProgress(t, db)
	runAssignmentUntilSettled(t, engine, db, forceFullPasses(t, db).Add(-time.Hour))
	require.Equal(t, "self", newestPayload(t, db, todo+":rewalk-"+s)["accountId"])
	_, has := newestPayload(t, db, child)["accountId"]
	require.False(t, has, "the child of an unassigned parent was assigned")

	t.Run("nothing waiting: a day later the pass is incremental", func(t *testing.T) {
		require.Zero(t, assignmentProgressOf(t, db, todo).waiting)
		ageLastFullPass(t, db, todo)
		// "Nothing written since": the verified point is moved past the slack,
		// so a to-do another test of the package wrote a moment ago is not
		// counted as this pass's work.
		_, err := db.ExecContext(ctx, `UPDATE account_assignment_progress
			SET verified_through = clock_timestamp() + interval '2 minutes' WHERE concept = ?`, todo)
		require.NoError(t, err)
		examined := runOnePassOf(t, engine, db, todo)
		require.False(t, assignmentProgressOf(t, db, todo).passFull, "a concept with nothing waiting was walked in full")
		require.Zero(t, examined, "nothing was written since, so nothing is examined")
	})

	t.Run("a row waiting: a day later the pass is full and assigns it", func(t *testing.T) {
		require.Positive(t, assignmentProgressOf(t, db, grant).waiting)
		// An owner places the parent in between, a write the child never sees.
		_, err := db.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{accountId}', '"rewalk-acct"')
			WHERE id = ?`, site)
		require.NoError(t, err)

		// Before the day is up the later pass does not reach the child.
		_, err = db.ExecContext(ctx, `UPDATE account_assignment_progress SET next_at = clock_timestamp() WHERE concept = ?`, grant)
		require.NoError(t, err)
		runOnePassOf(t, engine, db, grant)
		require.False(t, assignmentProgressOf(t, db, grant).passFull)
		_, has := newestPayload(t, db, child)["accountId"]
		require.False(t, has)

		ageLastFullPass(t, db, grant)
		runOnePassOf(t, engine, db, grant)
		require.True(t, assignmentProgressOf(t, db, grant).passFull, "rows were waiting, so the pass should be full")
		require.Equal(t, "rewalk-acct", newestPayload(t, db, child)["accountId"])
	})
}

// makeDue makes concepts claimable now.
func makeDue(t *testing.T, db *bun.DB, concepts ...string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE account_assignment_progress SET next_at = clock_timestamp()
		WHERE concept = ANY(?::text[])`, pq.Array(concepts))
	require.NoError(t, err)
}

// A row written without an account inside a transaction that stays open across
// a later pass is invisible to that pass. The pass must not then advance past
// the start of that transaction, or the pass after the commit would begin
// beyond the row and never see it. The slack is set to zero so the test does
// not have to hold the transaction open for longer than the slack.
func TestAccountAssignmentCatchesALateCommit(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	s := uniqueSuffix(t.Name())
	const todo = "v1:todos:todo"
	seedPrincipal(t, engine, "late-owner-"+s, auth.RoleOwner)
	saved := assignmentLateCommitSlack
	assignmentLateCommitSlack = 0
	t.Cleanup(func() { assignmentLateCommitSlack = saved })

	_, order, err := assignmentConcepts()
	require.NoError(t, err)
	require.NoError(t, ensureAssignmentProgressRows(ctx, db, order))
	makeDue(t, db, todo)
	runOnePassOf(t, engine, db, todo)

	late := todo + ":late-" + s
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var createdAt time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&createdAt))
	_, err = tx.NewInsert().Model(&memorynodes.MemoryNode{
		ID: late, Concept: todo, CreatedBy: "assignment-test", CreatedAt: createdAt.UTC(),
		Payload:    json.RawMessage(`{"ownerUserId":"late-owner-` + s + `","title":"late"}`),
		Provenance: json.RawMessage(`{"kind":"direct","name":"late-commit-test"}`),
	}).Exec(ctx)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)

	makeDue(t, db, todo)
	runOnePassOf(t, engine, db, todo)
	require.NoError(t, tx.Commit())

	makeDue(t, db, todo)
	runOnePassOf(t, engine, db, todo)
	require.Equal(t, "self", newestPayload(t, db, late)["accountId"], "the row whose transaction committed late was never assigned")
}

// A full pass asked for is the next pass of that concept, whatever its last
// full pass's age; a concept nobody asked about keeps its schedule; and a
// requested pass of a concept waits for the requested pass of the concept it
// takes its account from, as a first pass does.
func TestAccountAssignmentFullPassOnRequest(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	s := uniqueSuffix(t.Name())
	const todo, grant = "v1:todos:todo", "v1:platform:sitePreviewGrant"
	seedPrincipal(t, engine, "request-owner-"+s, auth.RoleOwner)
	site := conceptPlatformSite + ":request-site-" + s
	seedRawRow(t, ctx, db, conceptPlatformSite, site, time.Now().Add(-time.Hour), "assignment-test",
		map[string]any{"ownerUserId": "request-owner-" + s})
	child := grant + ":request-grant-" + s
	seedRawRow(t, ctx, db, grant, child, time.Now().Add(-time.Hour), "assignment-test",
		map[string]any{"siteId": "request-site-" + s})

	resetAssignmentProgress(t, db)
	runAssignmentUntilSettled(t, engine, db, forceFullPasses(t, db).Add(-time.Hour))
	require.Positive(t, assignmentProgressOf(t, db, grant).waiting)
	_, err := db.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{accountId}', '"request-acct"')
		WHERE id = ?`, site)
	require.NoError(t, err)

	// Both have rows waiting and a full pass an hour old: neither is due a
	// full pass by the schedule alone.
	_, err = db.ExecContext(ctx, `UPDATE account_assignment_progress
		SET full_pass_at = clock_timestamp() - interval '1 hour', next_at = clock_timestamp(),
		    waiting = GREATEST(waiting, 1) WHERE concept = ANY(?::text[])`, pq.Array([]string{grant, todo}))
	require.NoError(t, err)
	require.NoError(t, RequestAccountFullPass(ctx, db, []string{grant}))

	runOnePassOf(t, engine, db, grant)
	require.True(t, assignmentProgressOf(t, db, grant).passFull, "the requested pass was not full")
	require.Equal(t, "request-acct", newestPayload(t, db, child)["accountId"])

	runOnePassOf(t, engine, db, todo)
	require.False(t, assignmentProgressOf(t, db, todo).passFull, "a concept nobody asked about was walked in full")

	makeDue(t, db, grant)
	runOnePassOf(t, engine, db, grant)
	require.False(t, assignmentProgressOf(t, db, grant).passFull, "a request already honored was honored again")

	t.Run("parents first", func(t *testing.T) {
		const goal, run = "v1:work:goal", "v1:work:run"
		require.NoError(t, RequestAccountFullPass(ctx, db, []string{goal, run}))
		managed, order, err := assignmentConcepts()
		require.NoError(t, err)
		candidates, err := assignmentCandidates(ctx, db, managed, order)
		require.NoError(t, err)
		require.Contains(t, candidates, goal)
		require.NotContains(t, candidates, run, "a child's requested pass started before its parent's")

		runOnePassOf(t, engine, db, goal)
		candidates, err = assignmentCandidates(ctx, db, managed, order)
		require.NoError(t, err)
		require.Contains(t, candidates, run)
		runOnePassOf(t, engine, db, run)
		require.True(t, assignmentProgressOf(t, db, run).passFull)
	})
}

// conflictOf reads one conflict row of a test.
type conflictRow struct {
	candidates []string
	resolved   bool
	leftOut    bool
}

func conflictOf(t *testing.T, db *bun.DB, rowID, kind string) conflictRow {
	t.Helper()
	var r conflictRow
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT candidates, resolved_at IS NOT NULL, left_out_at IS NOT NULL
		FROM account_assignment_conflict WHERE row_id = ? AND kind = ?`, rowID, kind).
		Scan(pq.Array(&r.candidates), &r.resolved, &r.leftOut))
	return r
}

// A conflict records the accounts its row could belong to; one recorded before
// candidates existed gains them on the next pass; one found again takes the
// candidates found; and one an owner left out is never reopened, even when the
// row would now be assigned.
func TestAccountAssignmentConflictsRecordTheirCandidates(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	f := newAssignmentFixture(t, engine, db, t.Name())
	const store = "v1:shopify:store"
	resetAssignmentProgress(t, db)
	runAssignmentUntilSettled(t, engine, db, forceFullPasses(t, db).Add(-time.Hour))

	require.Equal(t, []string{f.acme, f.beta}, conflictOf(t, db, f.store2, conflictStoreBoundTwice).candidates)
	n, err := ConflictsNaming(ctx, db.DB, f.beta)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	n, err = ConflictsNaming(ctx, db.DB, "v1:accounts:account:"+f.acme)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "the canonical spelling of an account names it too")

	t.Run("a conflict recorded before candidates gains them on a later pass", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `UPDATE account_assignment_conflict SET candidates = '{}' WHERE row_id = ?`, f.store2)
		require.NoError(t, err)
		makeDue(t, db, store)
		runOnePassOf(t, engine, db, store)
		require.False(t, assignmentProgressOf(t, db, store).passFull)
		require.Equal(t, []string{f.acme, f.beta}, conflictOf(t, db, f.store2, conflictStoreBoundTwice).candidates)
	})

	gamma := "gamma-" + f.suffix
	t.Run("a conflict found again takes the candidates found", func(t *testing.T) {
		seedRawRow(t, ctx, db, conceptPlatformSite, conceptPlatformSite+":site3-"+f.suffix, time.Now().Add(-time.Hour), "assignment-test",
			map[string]any{"ownerUserId": f.bob, "accountId": gamma, "binding": map[string]any{"storeId": "store2-" + f.suffix}})
		require.NoError(t, RequestAccountFullPass(ctx, db, []string{store}))
		runOnePassOf(t, engine, db, store)
		require.Equal(t, []string{f.acme, f.beta, gamma}, conflictOf(t, db, f.store2, conflictStoreBoundTwice).candidates)
		n, err := ConflictsNaming(ctx, db.DB, gamma)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	})

	t.Run("a conflict left out is not reopened", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `UPDATE account_assignment_conflict
			SET left_out_at = clock_timestamp(), left_out_by = 'an-owner', resolved_at = clock_timestamp() WHERE row_id = ?`, f.store2)
		require.NoError(t, err)
		// What made the store ambiguous goes away: only acme's site binds it now.
		_, err = db.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload = payload - 'binding'
			WHERE id = ANY(?::text[])`, pq.Array([]string{conceptPlatformSite + ":site2-" + f.suffix, conceptPlatformSite + ":site3-" + f.suffix}))
		require.NoError(t, err)
		require.NoError(t, RequestAccountFullPass(ctx, db, []string{store}))
		runOnePassOf(t, engine, db, store)

		c := conflictOf(t, db, f.store2, conflictStoreBoundTwice)
		require.True(t, c.leftOut)
		require.True(t, c.resolved, "a conflict left out was reopened")
		_, has := newestPayload(t, db, f.store2)["accountId"]
		require.False(t, has, "a row left out of every account was assigned")
		n, err := ConflictsNaming(ctx, db.DB, gamma)
		require.NoError(t, err)
		require.Zero(t, n, "a conflict left out still names its candidates")
	})
}

// The export's judge counts, without writing, the recent rows the job would
// still assign or would leave waiting on a parent that can still get one, and
// leaves out the rows the job leaves alone on purpose.
func TestAccountAssignmentRowsStillAssignable(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	s := uniqueSuffix(t.Name())
	const goal, run, todo, grant, skill = "v1:work:goal", "v1:work:run", "v1:todos:todo", "v1:platform:sitePreviewGrant", "v1:skills:skill"
	owner := "judge-owner-" + s
	seedPrincipal(t, engine, owner, auth.RoleOwner)

	var since time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&since))
	at := since.Add(time.Second)
	raw := func(concept, id string, payload map[string]any) string {
		full := concept + ":" + id
		seedRawRow(t, ctx, db, concept, full, at, "assignment-test", payload)
		return full
	}
	// A goal the owner rule assigns, and a run waiting on it: the goal can
	// still get an account, so the run counts.
	g := raw(goal, "judge-goal-"+s, map[string]any{"ownerUserId": owner, "title": "g"})
	raw(run, "judge-run-"+s, map[string]any{"ownerUserId": owner, "goalId": g})
	// A grant following a site that states no account: left alone.
	raw(conceptPlatformSite, "judge-site-"+s, map[string]any{"ownerUserId": owner})
	raw(grant, "judge-grant-"+s, map[string]any{"siteId": "judge-site-" + s})
	// A cluster-provided row of a shared kind: left alone.
	raw(skill, "judge-skill-"+s, map[string]any{"ownerUserId": "", "slug": "judge-skill-" + s})
	// Two to-dos the owner rule would assign, one of them with a recorded
	// conflict: only the other counts.
	raw(todo, "judge-todo-"+s, map[string]any{"ownerUserId": owner, "title": "t"})
	conflicted := raw(todo, "judge-todo-conflicted-"+s, map[string]any{"ownerUserId": owner, "title": "c"})
	_, err := db.ExecContext(ctx, `INSERT INTO account_assignment_conflict (concept, row_id, kind, detail, candidates)
		VALUES (?, ?, 'store_bound_twice', 'test', '{self}')`, todo, conflicted)
	require.NoError(t, err)

	counts, err := engine.AccountRowsStillAssignable(ctx, db.DB, since, []string{goal, run, todo, grant, skill, conceptPlatformSite})
	require.NoError(t, err)
	require.Equal(t, map[string]int64{goal: 1, run: 1, todo: 1}, counts)
	_, has := newestPayload(t, db, g)["accountId"]
	require.False(t, has, "the judge wrote")

	t.Run("an assigned row is no longer counted", func(t *testing.T) {
		_, err := db.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{accountId}', '"self"')
			WHERE id = ANY(?::text[])`, pq.Array([]string{g, todo + ":judge-todo-" + s}))
		require.NoError(t, err)
		counts, err := engine.AccountRowsStillAssignable(ctx, db.DB, since, []string{goal, run, todo})
		require.NoError(t, err)
		require.Equal(t, map[string]int64{run: 1}, counts, "the run now takes its goal's account")
	})
}

func TestAccountAssignmentStates(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	_, order, err := assignmentConcepts()
	require.NoError(t, err)

	resetAssignmentProgress(t, db)
	states, err := engine.AccountAssignmentStates(ctx, db.DB)
	require.NoError(t, err)
	require.Len(t, states, len(order))
	for i, st := range states {
		require.Equal(t, order[i], st.Concept, "the states are in assignment order")
		require.False(t, st.FirstPassFinished, "%s has no progress row and reads finished", st.Concept)
		require.True(t, st.VerifiedThrough.IsZero())
	}

	start := forceFullPasses(t, db).Add(-time.Hour)
	runAssignmentUntilSettled(t, engine, db, start)
	_, err = db.ExecContext(ctx, `UPDATE account_assignment_progress SET waiting = 3 WHERE concept = 'v1:todos:todo'`)
	require.NoError(t, err)
	states, err = engine.AccountAssignmentStates(ctx, db.DB)
	require.NoError(t, err)
	for _, st := range states {
		require.True(t, st.FirstPassFinished, st.Concept)
		require.False(t, st.VerifiedThrough.IsZero(), st.Concept)
		if st.Concept == "v1:todos:todo" {
			require.EqualValues(t, 3, st.Waiting)
		}
	}
}

// A batch that fails halves its concept's batch size and steps it back, so the
// next call works on another concept; once the cause is gone the concept
// finishes in its smaller batches.
func TestAccountAssignmentFailedBatchShrinksAndStepsAside(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	s := uniqueSuffix(t.Name())
	const todo, folder = "v1:todos:todo", "v1:library:folder"
	seedPrincipal(t, engine, "shrink-owner-"+s, auth.RoleOwner)
	row := todo + ":shrink-" + s
	seedRawRow(t, ctx, db, todo, row, time.Now().Add(-time.Hour), "assignment-test",
		map[string]any{"ownerUserId": "shrink-owner-" + s, "title": "t"})
	seedRawRow(t, ctx, db, folder, folder+":shrink-"+s, time.Now().Add(-time.Hour), "assignment-test",
		map[string]any{"ownerUserId": "shrink-owner-" + s, "name": "f"})

	resetAssignmentProgress(t, db)
	_, order, err := assignmentConcepts()
	require.NoError(t, err)
	require.NoError(t, ensureAssignmentProgressRows(ctx, db, order))

	assignmentWriteFault = func(concept string) error {
		if concept == todo {
			return fmt.Errorf("a statement timeout, staged")
		}
		return nil
	}
	t.Cleanup(func() { assignmentWriteFault = nil })

	before := time.Now()
	_, err = engine.assignOneBatch(ctx, db, []string{todo, folder}, assignmentBatchIds)
	require.Error(t, err)
	failed := assignmentProgressOf(t, db, todo)
	require.Equal(t, assignmentBatchIds/2, failed.batchSize)
	require.True(t, failed.nextAt.After(before.Add(20*time.Second)), "next_at %v was not pushed out", failed.nextAt)

	next, err := engine.assignOneBatch(ctx, db, []string{todo, folder}, assignmentBatchIds)
	require.NoError(t, err)
	require.Equal(t, folder, next.concept, "the next call should work on another concept")

	assignmentWriteFault = nil
	_, err = db.ExecContext(ctx, `UPDATE account_assignment_progress SET next_at = clock_timestamp() WHERE concept = ?`, todo)
	require.NoError(t, err)
	for i := 0; i < 1000; i++ {
		size := assignmentProgressOf(t, db, todo).batchSize
		result, err := engine.assignOneBatch(ctx, db, []string{todo}, assignmentBatchIds)
		require.NoError(t, err)
		require.Equal(t, todo, result.concept)
		require.LessOrEqual(t, result.examined, size, "a batch was larger than the concept's batch size")
		var open bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT pass_finished_at IS NULL OR pass_finished_at < pass_started_at
			FROM account_assignment_progress WHERE concept = ?`, todo).Scan(&open))
		if !open {
			break
		}
	}
	require.Equal(t, "self", newestPayload(t, db, row)["accountId"])
	require.Greater(t, assignmentProgressOf(t, db, todo).batchSize, assignmentBatchIds/2, "a batch that succeeds grows the size back")
}
