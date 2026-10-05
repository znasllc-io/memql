package database

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
)

// THE READINESS COLLAPSE, BOUNDED AND RESUMABLE, AGAINST A REAL DATABASE
// (memql#5604).
//
// Each case gets a scratch schema (migrationLockDB) holding a "MemoryNodes"
// shaped like the real one where the collapse reads it -- the (id,
// "createdAt") primary key, the latest-row index, a hypertable in one-day
// chunks when TimescaleDB is installed -- and a one-connection pool whose
// search_path is that schema, like the migration pool. The collapse's
// unqualified names land there and never on the shared table where other
// packages' tests write readiness rows.
//
// A statement-level DELETE trigger logs how many rows each DELETE statement
// removed. The per-statement bound is read off what the DATABASE did rather
// than off what the code reports, which is what lets one assertion hold the
// SQL form this replaced to the same standard: run against it, the registered
// migration's single statement removes every doomed version at once.

const (
	readinessCollapseTestConcept = readinessCollapseConcept
	readinessCollapseTestBigId   = "v1:platform:moduleReadiness:ai--node-a"
)

// readinessCollapseFixture is one seeded scratch schema.
type readinessCollapseFixture struct {
	db     *bun.DB
	admin  *bun.DB
	schema string
	// total is how many readiness versions were seeded, ids how many ids they
	// belong to, and doomed every version that is not its id's newest.
	total, ids, doomed int64
}

// newReadinessCollapseFixture seeds the history every case starts from. The
// first readiness id carries bigHistory versions two minutes apart; the rest
// are small, three hours apart, so the history crosses chunk boundaries. Beside
// them: the same id as one readiness row under ANOTHER concept, with versions
// newer than every readiness version of it, and a cluster node's history. The
// collapse may move none of those, and the readiness newest must be the newest
// among readiness rows alone.
func newReadinessCollapseFixture(t *testing.T, bigHistory int) *readinessCollapseFixture {
	t.Helper()
	db, admin, schema := migrationLockDB(t, 10*time.Second)
	return seedReadinessCollapseFixture(t, db, admin, schema, bigHistory)
}

// seedReadinessCollapseFixture seeds a scratch schema migrationLockDB made, for
// a case that needs the pool's read deadline to be other than the driver's 10 s.
func seedReadinessCollapseFixture(t *testing.T, db, admin *bun.DB, schema string, bigHistory int) *readinessCollapseFixture {
	t.Helper()
	db.DB.SetMaxOpenConns(1)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(query, "\n", 2)[0], err)
		}
	}

	exec(`CREATE TABLE "MemoryNodes" (
  id          text NOT NULL,
  "createdAt" timestamptz NOT NULL,
  concept     text NOT NULL,
  payload     jsonb NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (id, "createdAt")
)`)
	exec(`CREATE INDEX memory_nodes_concept_id_created_at_desc_idx ON "MemoryNodes" (concept, id, "createdAt" DESC)`)
	var extension sql.NullString
	if err := admin.QueryRowContext(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
 WHERE e.extname = 'timescaledb'`).Scan(&extension); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if extension.Valid {
		exec(`SELECT ` + quoteIdentifier(extension.String) + `.create_hypertable('"MemoryNodes"', 'createdAt', chunk_time_interval => interval '1 day')`)
	}
	exec(`CREATE TABLE readiness_collapse_deletes (n bigint NOT NULL)`)
	// SET search_path FROM CURRENT: the log is found from any session that
	// deletes, including one that has not set the scratch schema's path.
	exec(`CREATE FUNCTION readiness_collapse_log_deletes() RETURNS trigger LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
  INSERT INTO readiness_collapse_deletes SELECT count(*) FROM old_rows;
  RETURN NULL;
END
$$`)
	exec(`CREATE TRIGGER readiness_collapse_log_deletes AFTER DELETE ON "MemoryNodes"
  REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION readiness_collapse_log_deletes()`)

	exec(`INSERT INTO "MemoryNodes" (id, "createdAt", concept, payload)
SELECT s.id, timestamptz '2026-09-01 00:00:00+00' + s.lead + k * s.step, s.concept, jsonb_build_object('k', k)
  FROM (VALUES
    (?, ?, ?::int, interval '0', interval '2 minutes'),
    ('v1:platform:moduleReadiness:storage--node-a', ?, 11, interval '0', interval '3 hours'),
    ('v1:platform:moduleReadiness:githubApp--node-c', ?, 6, interval '0', interval '3 hours'),
    ('v1:platform:moduleReadiness:ai--node-c', ?, 2, interval '0', interval '3 hours'),
    ('v1:platform:moduleReadiness:email--node-b', ?, 1, interval '0', interval '3 hours'),
    ('v1:platform:moduleReadiness:workbench--node-d', ?, 4, interval '0', interval '3 hours'),
    ('v1:platform:moduleReadiness:workbench--node-d', 'v1:product:moduleReadiness', 5, interval '7 minutes', interval '3 hours'),
    ('v1:cluster:node:node-a', 'v1:cluster:node', 4, interval '0', interval '3 hours')
  ) AS s(id, concept, n, lead, step)
 CROSS JOIN LATERAL generate_series(0, s.n - 1) AS k`,
		readinessCollapseTestBigId, readinessCollapseTestConcept, bigHistory,
		readinessCollapseTestConcept, readinessCollapseTestConcept, readinessCollapseTestConcept,
		readinessCollapseTestConcept, readinessCollapseTestConcept)

	// The expected result, computed independently of the collapse: every row of
	// every other concept, plus the newest version of each readiness id among
	// readiness rows. Restating the collapse's own predicate would only prove it
	// equals itself.
	exec(`CREATE TABLE readiness_collapse_expected AS
SELECT id, "createdAt", concept, payload FROM "MemoryNodes" m
 WHERE m.concept <> ?
    OR m."createdAt" = (SELECT max(x."createdAt") FROM "MemoryNodes" x WHERE x.concept = m.concept AND x.id = m.id)`,
		readinessCollapseTestConcept)

	f := &readinessCollapseFixture{db: db, admin: admin, schema: schema}
	if err := db.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT id) FROM "MemoryNodes" WHERE concept = ?`,
		readinessCollapseTestConcept).Scan(&f.total, &f.ids); err != nil {
		t.Fatal(err)
	}
	f.doomed = f.total - f.ids
	if f.doomed < 2 || f.ids != 6 {
		t.Fatalf("seeded %d readiness versions over %d ids; the fixture needs a history to collapse", f.total, f.ids)
	}
	if extension.Valid {
		var chunks int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+quoteIdentifier(extension.String)+`.show_chunks('"MemoryNodes"')`).Scan(&chunks); err != nil {
			t.Fatal(err)
		}
		if chunks < 2 {
			t.Fatalf("the seeded hypertable has %d chunk(s); the history must cross a chunk boundary", chunks)
		}
	}
	return f
}

func (f *readinessCollapseFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// readinessVersions is how many readiness versions the table holds now.
func (f *readinessCollapseFixture) readinessVersions(t *testing.T) int64 {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM "MemoryNodes" WHERE concept = ?`, readinessCollapseTestConcept)
}

// assertNewestIntact fails when any id's newest readiness version is gone. A
// collapse interrupted anywhere must never have taken one.
func (f *readinessCollapseFixture) assertNewestIntact(t *testing.T) {
	t.Helper()
	if lost := f.count(t, `SELECT count(*) FROM readiness_collapse_expected e
 WHERE e.concept = ?
   AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" m WHERE m.concept = e.concept AND m.id = e.id AND m."createdAt" = e."createdAt")`,
		readinessCollapseTestConcept); lost != 0 {
		t.Fatalf("%d ids lost their newest readiness version", lost)
	}
}

// assertCollapsed fails unless the table holds exactly the expected rows: the
// newest version of each readiness id and every row of every other concept.
func (f *readinessCollapseFixture) assertCollapsed(t *testing.T) {
	t.Helper()
	f.assertNewestIntact(t)
	if diff := f.count(t, `SELECT count(*) FROM (
  (SELECT id, "createdAt", concept, payload FROM "MemoryNodes"
   EXCEPT ALL SELECT id, "createdAt", concept, payload FROM readiness_collapse_expected)
  UNION ALL
  (SELECT id, "createdAt", concept, payload FROM readiness_collapse_expected
   EXCEPT ALL SELECT id, "createdAt", concept, payload FROM "MemoryNodes")) d`); diff != 0 {
		t.Fatalf("%d rows differ from the expected collapse: it must keep exactly the newest version of each readiness id and every row of every other concept", diff)
	}
	if got := f.readinessVersions(t); got != f.ids {
		t.Fatalf("%d readiness versions over %d ids; exactly one each must survive", got, f.ids)
	}
}

// deletes reads the trigger's log: how many DELETE statements committed, the
// most rows one of them removed, and how many they removed in all.
func (f *readinessCollapseFixture) deletes(t *testing.T) (statements, largest, removed int64) {
	t.Helper()
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT count(*), COALESCE(max(n), 0), COALESCE(sum(n), 0) FROM readiness_collapse_deletes`).
		Scan(&statements, &largest, &removed); err != nil {
		t.Fatal(err)
	}
	return statements, largest, removed
}

// progress reads the progress row, or exists=false when the table is gone.
func (f *readinessCollapseFixture) progress(t *testing.T) (cursor string, collapsed int64, exists bool) {
	t.Helper()
	ctx := context.Background()
	var table sql.NullString
	if err := f.db.QueryRowContext(ctx, `SELECT to_regclass(?)::text`, readinessCollapseProgressTable).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if !table.Valid {
		return "", 0, false
	}
	if err := f.db.QueryRowContext(ctx, `SELECT cursor_id, collapsed FROM `+readinessCollapseProgressTable+` WHERE singleton`).
		Scan(&cursor, &collapsed); err != nil {
		t.Fatal(err)
	}
	return cursor, collapsed, true
}

// readinessCollapseRecorder watches what a run sends and can end the run's
// context at a chosen batch: AFTER its COMMIT returns, which interrupts the
// run between statements, or just BEFORE its COMMIT is sent, which cancels the
// attempt with a batch midway. before, when set, sees every statement just
// before it is sent, which is where a case acts as a second walk would.
type readinessCollapseRecorder struct {
	mu                 sync.Mutex
	statements         []string
	inBatch            bool
	batchCommits       int
	cancelAfterCommit  int
	cancelBeforeCommit int
	cancel             context.CancelFunc
	before             func(query string)
}

func (r *readinessCollapseRecorder) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.before != nil {
		r.before(e.Query)
	}
	if e.Query == "COMMIT" && r.inBatch && r.cancelBeforeCommit == r.batchCommits+1 {
		r.cancel()
	}
	return ctx
}

func (r *readinessCollapseRecorder) AfterQuery(_ context.Context, e *bun.QueryEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, e.Query)
	switch {
	case strings.Contains(e.Query, `DELETE FROM "MemoryNodes"`):
		r.inBatch = true
	case e.Query == "COMMIT" && r.inBatch:
		r.inBatch = false
		if e.Err == nil {
			r.batchCommits++
			if r.cancelAfterCommit > 0 && r.batchCommits == r.cancelAfterCommit {
				r.cancel()
			}
		}
	case e.Query == "ROLLBACK":
		r.inBatch = false
	}
}

// THE PAIR IS GONE AND THE GO MIGRATION IS REGISTERED ONCE, UNDER ITS FILE'S
// NAME. Were the SQL pair still embedded, Discover would give version
// 20260921000000 two answers and bun would run both; a registration moved to
// another file would run under that file's version, possibly one a cluster
// has already recorded as applied.
func TestTheReadinessCollapseIsAGoMigrationRegisteredOnceUnderItsFileName(t *testing.T) {
	m := migrate.NewMigrations()
	registerTimescaleMigrations(m, nil)
	registerTimescaleMigrations(m, nil)

	version, comment, _ := strings.Cut(readinessCollapseMigrationName, "_")
	var got []migrate.Migration
	for _, mig := range m.Sorted() {
		if mig.Name == version {
			got = append(got, mig)
		}
	}
	if len(got) != 1 {
		t.Fatalf("migration %s is registered %d times, want exactly once", version, len(got))
	}
	if got[0].Comment != comment {
		t.Errorf("migration %s carries comment %q, want %q: bun names a Go migration after the file its Register "+
			"call sits in, and this one no longer sits in %s.go", version, got[0].Comment, comment, readinessCollapseMigrationName)
	}
	if got[0].Up == nil || got[0].Down == nil {
		t.Errorf("migration %s: Up set = %v, Down set = %v; both must be functions (the down is a deliberate no-op)",
			version, got[0].Up != nil, got[0].Down != nil)
	}
	leftover, err := fs.Glob(timescaleMigrationsFS, "memory-nodes/migrations/"+version+"_*")
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Errorf("the embedded migrations still carry %v; %s is a Go migration now", leftover, version)
	}
}

// THE REGISTERED MIGRATION, AS THE RUNNER RUNS IT, NEVER DELETES MORE THAN ONE
// BATCH IN A STATEMENT. One id carries 2500 versions, so the bound can only
// hold if the work is split. The SQL form this replaced fails here: its one
// statement removes every doomed version of every id at once.
func TestTheReadinessCollapseMigrationDeletesInBoundedStatements(t *testing.T) {
	f := newReadinessCollapseFixture(t, 2500)

	all := migrate.NewMigrations()
	registerTimescaleMigrations(all, nil)
	only := migrate.NewMigrations()
	for _, mig := range all.Sorted() {
		if mig.Name == "20260921000000" {
			only.Add(mig)
		}
	}
	if len(only.Sorted()) != 1 {
		t.Fatalf("the registered set carries %d migrations named 20260921000000, want 1", len(only.Sorted()))
	}
	runner := lockedMigrationRunner(t, only)
	runner.runMigrations(context.Background(), f.db)
	if err := runner.MigrationError(); err != nil {
		t.Fatalf("the collapse migration failed: %v", err)
	}

	statements, largest, removed := f.deletes(t)
	t.Logf("%d DELETE statements removed %d versions, the largest %d", statements, removed, largest)
	if largest > readinessCollapseBatch {
		t.Fatalf("one DELETE statement removed %d versions; none may remove more than %d, or a production-sized "+
			"history is one statement again and outlives the driver's read deadline (memql#5604)", largest, readinessCollapseBatch)
	}
	if want := (f.doomed + readinessCollapseBatch - 1) / readinessCollapseBatch; statements < want {
		t.Fatalf("%d DELETE statements removed %d versions; at most %d each needs at least %d", statements, removed, readinessCollapseBatch, want)
	}
	if removed != f.doomed {
		t.Fatalf("the statements removed %d versions; %d are not their id's newest", removed, f.doomed)
	}
	f.assertCollapsed(t)
	if n := f.count(t, `SELECT count(*) FROM bun_migrations WHERE name = '20260921000000'`); n != 1 {
		t.Fatalf("the migration is recorded %d times, want once", n)
	}
	if _, _, exists := f.progress(t); exists {
		t.Fatal("a finished walk left its progress table behind")
	}
}

// AN INTERRUPTED WALK RESUMES FROM ITS CURSOR, AND NOTHING IS DELETED TWICE OR
// LOST. Three runs over one history larger than one batch: the first is ended
// between statements after three batches, the second is cancelled with a batch
// midway, the third completes. What each run reports, what the progress row
// records and what the table holds must agree at every step.
func TestReadinessCollapseResumesFromItsCursor(t *testing.T) {
	const batch = 5
	f := newReadinessCollapseFixture(t, 23)

	// Ended BETWEEN statements: the attempt's context ends after the third
	// batch commits, and the collapse stops at its next statement.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	r1, err := collapseModuleReadinessHistory(ctx1, f.db.WithQueryHook(&readinessCollapseRecorder{cancel: cancel1, cancelAfterCommit: 3}), nil, batch)
	if !errors.Is(err, errReadinessCollapseDeferred) {
		t.Fatalf("an attempt ended between statements must defer, got %v", err)
	}
	if migrationMayStillBeRunning(err) {
		t.Fatalf("the runner would keep its lock for an operator over a run with nothing in flight: %v", err)
	}
	if r1.Done || r1.Batches != 3 || r1.Collapsed != 3*batch {
		t.Fatalf("first run: %+v, want three full batches of %d and not done", r1, batch)
	}
	if got := f.readinessVersions(t); got != f.total-r1.Collapsed {
		t.Fatalf("after the first run the table holds %d readiness versions, want %d", got, f.total-r1.Collapsed)
	}
	f.assertNewestIntact(t)
	cursor, collapsed, exists := f.progress(t)
	if !exists || cursor != readinessCollapseTestBigId || collapsed != r1.Collapsed {
		t.Fatalf("progress after the first run: cursor %q collapsed %d exists %v, want %q, %d, true",
			cursor, collapsed, exists, readinessCollapseTestBigId, r1.Collapsed)
	}

	// Ended MIDWAY THROUGH A STEP: the attempt's context ends after the second
	// batch's DELETE and before its COMMIT is sent, as a SIGTERM at a node's
	// restart would end it. A step runs on a context of its own, so the cancel
	// cannot cut it off: the batch commits whole, and the walk stops at its next
	// statement with the deferral. On the attempt's context, database/sql would
	// answer with context.Canceled and a ROLLBACK -- an error the runner keeps
	// its lock for although nothing is left running.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	r2, err := collapseModuleReadinessHistory(ctx2, f.db.WithQueryHook(&readinessCollapseRecorder{cancel: cancel2, cancelBeforeCommit: 2}), nil, batch)
	if !errors.Is(err, errReadinessCollapseDeferred) || migrationMayStillBeRunning(err) {
		t.Fatalf("an attempt cancelled midway through a step must defer once the step ends, got %v", err)
	}
	// The big id's 22 restatements: 15 went in the first run, 5 in this one's
	// first batch, and the last 2 in the batch the cancel arrived during.
	if !r2.Resumed || r2.Batches != 2 || r2.Collapsed != batch+2 {
		t.Fatalf("second run: %+v, want it resumed with two committed batches, of %d and 2", r2, batch)
	}
	if got, want := f.readinessVersions(t), f.total-r1.Collapsed-r2.Collapsed; got != want {
		t.Fatalf("after the cancelled attempt the table holds %d readiness versions, want %d: the step the cancel arrived during must have finished", got, want)
	}
	f.assertNewestIntact(t)
	if _, collapsed, _ := f.progress(t); collapsed != r1.Collapsed+r2.Collapsed {
		t.Fatalf("the progress row records %d versions collapsed, the table %d: a batch and its cursor move must commit together",
			collapsed, r1.Collapsed+r2.Collapsed)
	}

	// Completes.
	r3, err := collapseModuleReadinessHistory(context.Background(), f.db, nil, batch)
	if err != nil {
		t.Fatalf("the resumed walk failed: %v", err)
	}
	if !r3.Done || !r3.Resumed {
		t.Fatalf("third run: %+v, want it resumed and done", r3)
	}
	if sum := r1.Collapsed + r2.Collapsed + r3.Collapsed; sum != f.doomed {
		t.Fatalf("the three runs collapsed %d versions; %d are not their id's newest -- a difference is a version deleted twice or missed", sum, f.doomed)
	}
	f.assertCollapsed(t)
	statements, largest, removed := f.deletes(t)
	if largest > batch || removed != f.doomed || statements != int64(r1.Batches+r2.Batches+r3.Batches) {
		t.Fatalf("the trigger saw %d committed DELETE statements removing %d versions, the largest %d; "+
			"want %d statements of at most %d removing %d", statements, removed, largest, r1.Batches+r2.Batches+r3.Batches, batch, f.doomed)
	}
	if _, _, exists := f.progress(t); exists {
		t.Fatal("a finished walk left its progress table behind")
	}

	// And again: nothing left to do, nothing deleted.
	r4, err := collapseModuleReadinessHistory(context.Background(), f.db, nil, batch)
	if err != nil || !r4.Done || r4.Collapsed != 0 || r4.Resumed {
		t.Fatalf("a run over a collapsed table: %+v, %v; want a fresh walk that deletes nothing", r4, err)
	}
	f.assertCollapsed(t)
}

// A BATCH ANOTHER DELETER RACED DOES NOT END ITS ID EARLY. A version inside the
// second batch's range is deleted by another session that holds its row lock
// until the batch is waiting on it: the batch's floor counted that version,
// its DELETE then skips it, and the batch removes one fewer than a batch. The
// id is finished only when a batch finds no floor, so the walk carries on below
// it; a walk that took "fewer than a batch" for "finished" would leave every
// older version of the id behind.
func TestReadinessCollapseKeepsGoingWhenAnotherDeleterRacesABatch(t *testing.T) {
	const batch = 5
	f := newReadinessCollapseFixture(t, 23)
	ctx := context.Background()
	table := quoteIdentifier(f.schema) + `."MemoryNodes"`

	// Below the id's newest, batches run newest first: the first takes the 2nd
	// to 6th newest versions, the second the 7th to 11th. The 8th sits inside
	// the second.
	var victim time.Time
	if err := f.admin.QueryRowContext(ctx, `SELECT "createdAt" FROM `+table+`
 WHERE concept = ? AND id = ? ORDER BY "createdAt" DESC OFFSET 7 LIMIT 1`,
		readinessCollapseTestConcept, readinessCollapseTestBigId).Scan(&victim); err != nil {
		t.Fatal(err)
	}
	racer, err := f.admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer racer.Rollback() //nolint:errcheck // a no-op once committed
	if _, err := racer.ExecContext(ctx, `DELETE FROM `+table+` WHERE concept = ? AND id = ? AND "createdAt" = ?`,
		readinessCollapseTestConcept, readinessCollapseTestBigId, victim); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		report readinessCollapseReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := collapseModuleReadinessHistory(ctx, f.db, nil, batch)
		done <- outcome{r, err}
	}()

	// The collapse's statements run on the scratch pool, whose backends carry
	// the schema as their application_name. Commit the racing delete only once
	// one of them is waiting on its lock -- well inside the statement bound.
	deadline := time.Now().Add(readinessCollapseStatementTimeout - time.Second)
	for {
		var waiting int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE application_name = ? AND wait_event_type = 'Lock'`, f.schema).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the collapse never waited on the racing delete's lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := racer.Commit(); err != nil {
		t.Fatal(err)
	}

	got := <-done
	if got.err != nil || !got.report.Done {
		t.Fatalf("the raced walk: %+v, %v; want it done", got.report, got.err)
	}
	if got.report.Collapsed != f.doomed-1 {
		t.Fatalf("the walk collapsed %d versions; with one taken by the racer it should have taken the other %d",
			got.report.Collapsed, f.doomed-1)
	}
	f.assertCollapsed(t)
}

// A STATEMENT THE DRIVER'S READ DEADLINE CUTS OFF IS WAITED OUT, NOT HANDED TO
// THE RUNNER. The first batch blocks on a row lock another session holds, past
// a read deadline shortened to two seconds, and the driver gives up on it with
// the backend still running. The collapse waits for that backend to exit --
// it does once the lock is released and its statement ends, since its
// connection is gone and no COMMIT can follow -- and then defers, which the
// runner answers by releasing its lock. The batch never committed, and the
// next run collapses everything.
func TestReadinessCollapseWaitsOutAStatementTheReadDeadlineCutOff(t *testing.T) {
	const readTimeout = 2 * time.Second
	db, admin, schema := migrationLockDB(t, readTimeout)
	f := seedReadinessCollapseFixture(t, db, admin, schema, 23)
	ctx := context.Background()
	table := quoteIdentifier(f.schema) + `."MemoryNodes"`

	// Lock the version just below the big id's newest: the first batch's range.
	var victim time.Time
	if err := f.admin.QueryRowContext(ctx, `SELECT "createdAt" FROM `+table+`
 WHERE concept = ? AND id = ? ORDER BY "createdAt" DESC OFFSET 1 LIMIT 1`,
		readinessCollapseTestConcept, readinessCollapseTestBigId).Scan(&victim); err != nil {
		t.Fatal(err)
	}
	holder, err := f.admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback() //nolint:errcheck // the release, below or here
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM `+table+` WHERE concept = ? AND id = ? AND "createdAt" = ? FOR UPDATE`,
		readinessCollapseTestConcept, readinessCollapseTestBigId, victim); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		report readinessCollapseReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := collapseModuleReadinessHistory(ctx, f.db, nil, 5)
		done <- outcome{r, err}
	}()

	// Hold the lock until the driver has given up on the blocked batch, then
	// release it so the orphan's statement can end.
	deadline := time.Now().Add(readinessCollapseStatementTimeout)
	for {
		var waiting int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE application_name = ? AND wait_event_type = 'Lock'`, f.schema).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the collapse never waited on the held row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(readTimeout + 500*time.Millisecond)
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}

	got := <-done
	if !errors.Is(got.err, errReadinessCollapseDeferred) || !strings.Contains(got.err.Error(), "without a reply") {
		t.Fatalf("a statement the read deadline cut off must end in a deferral once its backend exits, got %v", got.err)
	}
	if migrationMayStillBeRunning(got.err) {
		t.Fatalf("the runner would keep its lock for an operator although the backend has exited: %v", got.err)
	}
	if got.report.Collapsed != 0 {
		t.Fatalf("the cut-off batch is reported collapsed (%d); its COMMIT was never sent", got.report.Collapsed)
	}
	if n := f.readinessVersions(t); n != f.total {
		t.Fatalf("the table holds %d readiness versions after the cut-off batch, want all %d: it must have rolled back", n, f.total)
	}

	r, err := collapseModuleReadinessHistory(ctx, f.db, nil, 5)
	if err != nil || !r.Done || r.Collapsed != f.doomed {
		t.Fatalf("the next run: %+v, %v; want it done, collapsing all %d", r, err, f.doomed)
	}
	f.assertCollapsed(t)
}

// A SHORT ATTEMPT STILL MAKES PROGRESS. Five seconds is less than the reserve
// a full attempt leaves the migrations after the collapse, and an attempt that
// kept the full reserve whatever its budget would defer at once, every time,
// with MEMORY_NODES_DATABASE_MIGRATION_TIMEOUT_MS at 5000. The reserve scales
// to the budget instead, so every attempt collapses something until the walk
// is done.
func TestReadinessCollapseMakesProgressInAShortAttempt(t *testing.T) {
	f := newReadinessCollapseFixture(t, 23)
	var collapsed int64
	for attempt := 1; ; attempt++ {
		if attempt > 10 {
			t.Fatalf("the walk was still not done after %d five-second attempts", attempt-1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		r, err := collapseModuleReadinessHistory(ctx, f.db, nil, 5)
		cancel()
		collapsed += r.Collapsed
		if err == nil && r.Done {
			break
		}
		if !errors.Is(err, errReadinessCollapseDeferred) || r.Collapsed == 0 {
			t.Fatalf("attempt %d of five seconds: %+v, %v; every attempt must collapse something until the walk is done", attempt, r, err)
		}
	}
	if collapsed != f.doomed {
		t.Fatalf("the attempts collapsed %d versions in all, want %d", collapsed, f.doomed)
	}
	f.assertCollapsed(t)
}

// AN ATTEMPT TOO SHORT FOR ONE STEP SENDS NOTHING, AND SAYS WHAT IT NEEDS. Its
// error is the deferral, which the runner answers by releasing its lock, and it
// names the least an attempt must have left and the setting that gives it --
// an attempt that never has that much never runs the collapse, and an operator
// reading the log has to be told which knob that is.
func TestReadinessCollapseNamesTheMinimumAnAttemptNeeds(t *testing.T) {
	f := newReadinessCollapseFixture(t, 23)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rec := &readinessCollapseRecorder{}
	r, err := collapseModuleReadinessHistory(ctx, f.db.WithQueryHook(rec), nil, 5)
	if !errors.Is(err, errReadinessCollapseDeferred) || migrationMayStillBeRunning(err) {
		t.Fatalf("an attempt too short for one step must defer without a context error, got %v", err)
	}
	minimum := (readinessCollapseBookkeeping + readinessCollapseMinStep).String()
	if !strings.Contains(err.Error(), "MEMORY_NODES_DATABASE_MIGRATION_TIMEOUT_MS") || !strings.Contains(err.Error(), minimum) {
		t.Fatalf("the deferral must name the setting and the %s minimum, got %v", minimum, err)
	}
	if len(rec.statements) != 0 || r.Collapsed != 0 {
		t.Fatalf("an attempt too short for one step sent %d statements and collapsed %d versions; it must send none",
			len(rec.statements), r.Collapsed)
	}
	if got := f.readinessVersions(t); got != f.total {
		t.Fatalf("the table holds %d readiness versions, want all %d", got, f.total)
	}
	if _, _, exists := f.progress(t); exists {
		t.Fatal("a run that sent nothing created the progress table")
	}
}

// THROUGH THE RUNNER: A DEFERRED ATTEMPT RELEASES THE LOCK, RECORDS NOTHING,
// AND THE NEXT ATTEMPT RESUMES AND RECORDS THE MIGRATION ONCE. This is the
// half memql#5640 could not give the original statement: an attempt that ends
// with work outstanding and no orphan, so no operator has to clear anything.
func TestReadinessCollapseDeferralReleasesTheRunnersLock(t *testing.T) {
	f := newReadinessCollapseFixture(t, 23)
	migrations := migrate.NewMigrations()
	registerModuleReadinessHistoryCollapse(migrations, nil)
	runner := lockedMigrationRunner(t, migrations)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.runMigrations(ctx, f.db.WithQueryHook(&readinessCollapseRecorder{cancel: cancel, cancelAfterCommit: 2}))
	if err := runner.MigrationError(); !errors.Is(err, errReadinessCollapseDeferred) {
		t.Fatalf("the first attempt must end deferred, got %v", err)
	}
	if !runner.migrationsPending() {
		t.Fatal("a deferred attempt must leave migrations pending, so the monitor tick retries")
	}
	if held := f.count(t, `SELECT count(*) FROM bun_migration_locks`); held != 0 {
		t.Fatalf("a deferred attempt left %d migration lock row(s); with nothing in flight it must release the lock", held)
	}
	if n := f.count(t, `SELECT count(*) FROM bun_migrations WHERE name = '20260921000000'`); n != 0 {
		t.Fatalf("a deferred attempt recorded the migration %d time(s)", n)
	}
	cursor, collapsed, exists := f.progress(t)
	if !exists || collapsed == 0 || collapsed >= f.doomed || cursor == "" {
		t.Fatalf("progress after the deferred attempt: cursor %q collapsed %d exists %v; want part of %d done",
			cursor, collapsed, exists, f.doomed)
	}

	runner.runMigrations(context.Background(), f.db)
	if err := runner.MigrationError(); err != nil {
		t.Fatalf("the second attempt failed: %v", err)
	}
	if runner.migrationsPending() {
		t.Fatal("migrations still pending after the attempt that completed the walk")
	}
	if n := f.count(t, `SELECT count(*) FROM bun_migrations WHERE name = '20260921000000'`); n != 1 {
		t.Fatalf("the migration is recorded %d times, want once", n)
	}
	f.assertCollapsed(t)
	if _, _, removed := f.deletes(t); removed != f.doomed {
		t.Fatalf("the two attempts removed %d versions, want %d", removed, f.doomed)
	}
}

// THROUGH THE RUNNER: AN ATTEMPT CANCELLED MIDWAY THROUGH A STEP RELEASES THE
// LOCK. The attempt's context ends after the first batch's DELETE and before
// its COMMIT -- a SIGTERM during a node's restart. On the attempt's context the
// step would end in context.Canceled and a ROLLBACK, and the runner, reading a
// context error as a statement that may still be running, would keep its lock:
// every later attempt "migration lock held" until an operator cleared it, with
// nothing committed and nothing running. The step finishes on its own context
// instead, and the walk defers.
func TestReadinessCollapseCancelledMidStepReleasesTheRunnersLock(t *testing.T) {
	f := newReadinessCollapseFixture(t, 23)
	migrations := migrate.NewMigrations()
	registerModuleReadinessHistoryCollapse(migrations, nil)
	runner := lockedMigrationRunner(t, migrations)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.runMigrations(ctx, f.db.WithQueryHook(&readinessCollapseRecorder{cancel: cancel, cancelBeforeCommit: 1}))
	if err := runner.MigrationError(); !errors.Is(err, errReadinessCollapseDeferred) {
		t.Fatalf("an attempt cancelled midway through a step must end deferred, got %v", err)
	}
	if held := f.count(t, `SELECT count(*) FROM bun_migration_locks`); held != 0 {
		t.Fatalf("the cancelled attempt left %d migration lock row(s); nothing was left running, so it must release the lock", held)
	}

	runner.runMigrations(context.Background(), f.db)
	if err := runner.MigrationError(); err != nil {
		t.Fatalf("the attempt after the cancelled one failed: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM bun_migrations WHERE name = '20260921000000'`); n != 1 {
		t.Fatalf("the migration is recorded %d times, want once", n)
	}
	f.assertCollapsed(t)
}

// severableProxy forwards TCP connections to the database and can cut every
// one it carries at once. The driver then sees its connection end mid-statement
// while the backend on the other end runs on until it next talks to its socket,
// which is what a failover or a dropped NAT entry looks like from the client.
type severableProxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newSeverableProxy(t *testing.T, target string) *severableProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &severableProxy{ln: ln}
	go p.serve(target)
	t.Cleanup(func() {
		_ = ln.Close()
		p.sever()
	})
	return p
}

func (p *severableProxy) serve(target string) {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", target)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
		go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
	}
}

func (p *severableProxy) sever() {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// A STEP WHOSE CONNECTION IS CUT IS WAITED OUT, NOT HANDED TO THE RUNNER. The
// first batch blocks on a row lock another session holds; then the connection
// under it is cut, as a failover would cut it. The driver reports end of file,
// a transport error the runner would keep its lock for -- but the collapse
// knows the backend's pid, waits until that backend is gone, and defers. The
// batch never committed: the backend can only have rolled it back.
func TestReadinessCollapseWaitsOutAStepWhoseConnectionWasCut(t *testing.T) {
	_, admin, schema := migrationLockDB(t, 10*time.Second)
	u, err := url.Parse(os.Getenv("MEMQL_DATABASE_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	proxy := newSeverableProxy(t, u.Host)
	u.Host = proxy.ln.Addr().String()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(u.String()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}), pgdriver.WithApplicationName(schema))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	f := seedReadinessCollapseFixture(t, db, admin, schema, 23)
	ctx := context.Background()
	table := quoteIdentifier(f.schema) + `."MemoryNodes"`

	var victim time.Time
	if err := f.admin.QueryRowContext(ctx, `SELECT "createdAt" FROM `+table+`
 WHERE concept = ? AND id = ? ORDER BY "createdAt" DESC OFFSET 1 LIMIT 1`,
		readinessCollapseTestConcept, readinessCollapseTestBigId).Scan(&victim); err != nil {
		t.Fatal(err)
	}
	holder, err := f.admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback() //nolint:errcheck // the release, below or here
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM `+table+` WHERE concept = ? AND id = ? AND "createdAt" = ? FOR UPDATE`,
		readinessCollapseTestConcept, readinessCollapseTestBigId, victim); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		report readinessCollapseReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := collapseModuleReadinessHistory(ctx, f.db, nil, 5)
		done <- outcome{r, err}
	}()

	deadline := time.Now().Add(readinessCollapseStatementTimeout)
	for {
		var waiting int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE application_name = ? AND wait_event_type = 'Lock'`, f.schema).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the collapse never waited on the held row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	proxy.sever()
	// The backend is still waiting on the lock: the collapse must still be
	// waiting for it, not deferring over a statement that could yet commit.
	select {
	case got := <-done:
		t.Fatalf("the collapse returned while its cut-off backend was still running: %+v, %v", got.report, got.err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}

	got := <-done
	if !errors.Is(got.err, errReadinessCollapseDeferred) || migrationMayStillBeRunning(got.err) {
		t.Fatalf("a step whose connection was cut must end in a deferral once its backend is gone, got %v", got.err)
	}
	if got.report.Collapsed != 0 {
		t.Fatalf("the cut-off batch is reported collapsed (%d); its COMMIT was never sent", got.report.Collapsed)
	}
	if n := f.readinessVersions(t); n != f.total {
		t.Fatalf("the table holds %d readiness versions after the cut-off batch, want all %d: it must have rolled back", n, f.total)
	}

	r, err := collapseModuleReadinessHistory(ctx, f.db, nil, 5)
	if err != nil || !r.Done || r.Collapsed != f.doomed {
		t.Fatalf("the next run: %+v, %v; want it done, collapsing all %d", r, err, f.doomed)
	}
	f.assertCollapsed(t)
}

// A STEP REFUSES A CURSOR ANOTHER WALK MOVED. Two walks over one table should
// never happen -- the runner's lock is what prevents it -- but an operator who
// clears a live lock against the runbook makes one. Each writing step therefore
// checks, under the progress row's lock and in the statement that writes, that
// the row still names the id this walk is on. Here the row is moved to another
// id just before the walk's statement goes out, as the other walk's committed
// advance would move it: the batch must delete nothing and write nothing into
// the row, and the advance must not mistake the mismatch for the end of the
// walk.
func TestReadinessCollapseRefusesACursorAnotherWalkMoved(t *testing.T) {
	const other = "v1:platform:moduleReadiness:storage--node-a"
	for _, tc := range []struct {
		name string
		// start positions the progress row before the walk opens it.
		start string
		// moving names the statement the row is moved under.
		moving string
	}{
		{name: "batch", start: readinessCollapseTestBigId, moving: `DELETE FROM "MemoryNodes"`},
		{name: "advance", start: "v1:platform:moduleReadiness:email--node-b", moving: `SET cursor_id`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReadinessCollapseFixture(t, 23)
			ctx := context.Background()
			if _, err := f.db.ExecContext(ctx, readinessCollapseProgressDDL); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, `INSERT INTO `+readinessCollapseProgressTable+` (singleton, cursor_id) VALUES (true, ?)`, tc.start); err != nil {
				t.Fatal(err)
			}
			progress := quoteIdentifier(f.schema) + `.` + readinessCollapseProgressTable
			moved := false
			rec := &readinessCollapseRecorder{before: func(query string) {
				if moved || !strings.Contains(query, tc.moving) {
					return
				}
				moved = true
				if _, err := f.admin.ExecContext(ctx, `UPDATE `+progress+` SET cursor_id = ?, older_than = NULL WHERE singleton`, other); err != nil {
					t.Errorf("moving the cursor: %v", err)
				}
			}}

			r, err := collapseModuleReadinessHistory(ctx, f.db.WithQueryHook(rec), nil, 5)
			if !moved {
				t.Fatalf("the walk never sent the statement the case moves the cursor under (%+v, %v)", r, err)
			}
			if !errors.Is(err, errReadinessCollapseCursorMoved) {
				t.Fatalf("a %s under a cursor another walk moved must fail on the mismatch, got %+v, %v", tc.name, r, err)
			}
			if r.Done || r.Collapsed != 0 {
				t.Fatalf("the refused walk reported %+v; it must have deleted nothing and not finished", r)
			}
			if got := f.readinessVersions(t); got != f.total {
				t.Fatalf("the table holds %d readiness versions, want all %d: the refused step deleted rows", got, f.total)
			}
			cursor, collapsed, exists := f.progress(t)
			if !exists || cursor != other || collapsed != 0 {
				t.Fatalf("progress after the refusal: cursor %q collapsed %d exists %v; the other walk's row must be as it left it",
					cursor, collapsed, exists)
			}
			var olderThan sql.NullTime
			if err := f.db.QueryRowContext(ctx, `SELECT older_than FROM `+readinessCollapseProgressTable+` WHERE singleton`).Scan(&olderThan); err != nil {
				t.Fatal(err)
			}
			if olderThan.Valid {
				t.Fatalf("the refused step wrote %s into a row naming another id", olderThan.Time)
			}
		})
	}
}

// NO CURSOR, HOWEVER WRONG, CAN DELETE AN ID'S NEWEST VERSION. The state a
// second walk could leave behind: the row names one id while older_than is a
// time from another id's history, here one newer than every version the named
// id has. Bounded only by the cursor, the resumed batch would take that id's
// newest version with the rest. Every batch is also bounded by the id's newest
// version as the statement reads it, so the walk collapses the id to that
// version and finishes as if the row had been right. The row names the walk's
// first id, so the whole table is walked and the whole expected result holds; a
// cursor further on would leave the ids before it to the walk that passed them.
func TestReadinessCollapseNeverDeletesAnIdsNewestFromACorruptCursor(t *testing.T) {
	f := newReadinessCollapseFixture(t, 23)
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, readinessCollapseProgressDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO `+readinessCollapseProgressTable+` (singleton, cursor_id, older_than)
VALUES (true, ?, timestamptz '2099-01-01 00:00:00+00')`, readinessCollapseTestBigId); err != nil {
		t.Fatal(err)
	}
	r, err := collapseModuleReadinessHistory(ctx, f.db, nil, 5)
	if err != nil || !r.Done {
		t.Fatalf("the walk from a corrupt cursor: %+v, %v; want it done", r, err)
	}
	f.assertCollapsed(t)
}
