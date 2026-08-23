package authactivity

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// The SQL, against real Postgres.
//
// prune_test.go covers the loop with a fake deleter; nothing there executes a
// statement. These are the assertions that can only be made against a table:
// that the cutoff selects the right rows, that EVERY VERSION of a row goes,
// that another concept's rows are untouched, and that two replicas sweeping at
// once do not error.

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "authActivity retention DB test", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db.DB
}

// insertActivityRow writes one version of a row straight into the node table.
// Deliberately raw SQL rather than the engine: this test is about what the
// DELETE sees, and going through createAuthActivity would drag the whole
// actor-borrowing path in for no gain.
func insertActivityRow(t *testing.T, db *sql.DB, concept, id string, createdAt time.Time, occurredAt string) {
	t.Helper()
	payload := fmt.Sprintf(`{"id":%q,"action":"session_refreshed","occurredAt":%q}`, id, occurredAt)
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, metadata, "type", concept)
		 VALUES ($1, $2, 'test', '{}'::jsonb, $3::jsonb, '{}'::jsonb, 'object', $4)`,
		id, createdAt, payload, concept)
	if err != nil {
		t.Fatalf("seed row %s: %v", id, err)
	}
}

func countRows(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM "MemoryNodes" WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", id, err)
	}
	return n
}

// uniqueSuffix keeps runs from colliding: MemoryNodes is append-only and
// shared across the whole lane, so a fixed id would find the previous run's
// rows and turn every assertion below into a statement about them.
func uniqueSuffix() string {
	return time.Now().UTC().Format("20060102T150405.000000000")
}

func TestPruneDeletesPastTheWindowAndKeepsTheRest(t *testing.T) {
	db := testDB(t)
	sfx := uniqueSuffix()
	now := time.Now().UTC()

	old := ConceptID + ":old-" + sfx
	recent := ConceptID + ":recent-" + sfx
	insertActivityRow(t, db, ConceptID, old, now.Add(-40*24*time.Hour),
		now.Add(-40*24*time.Hour).Format(time.RFC3339Nano))
	insertActivityRow(t, db, ConceptID, recent, now.Add(-2*24*time.Hour),
		now.Add(-2*24*time.Hour).Format(time.RFC3339Nano))

	// Another concept's row at the same age. The predicate is scoped by
	// concept, and without that scope this statement prunes the whole graph.
	bystander := "v1:identity:auditEvent:bystander-" + sfx
	insertActivityRow(t, db, "v1:identity:auditEvent", bystander, now.Add(-40*24*time.Hour),
		now.Add(-40*24*time.Hour).Format(time.RFC3339Nano))

	p := &Pruner{DB: func() *sql.DB { return db }, Retention: 30 * 24 * time.Hour, Logger: discard()}
	deleted, err := p.PruneOnce(context.Background())
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if deleted < 1 {
		t.Fatalf("deleted %d rows, want at least the one seeded past the window", deleted)
	}
	if got := countRows(t, db, old); got != 0 {
		t.Errorf("a row 40 days old survived the 30-day window (%d version(s) left)", got)
	}
	if got := countRows(t, db, recent); got != 1 {
		t.Errorf("a row 2 days old was deleted under a 30-day window (%d version(s) left)", got)
	}
	if got := countRows(t, db, bystander); got != 1 {
		t.Errorf("an auditEvent row was deleted by the authActivity sweep (%d version(s) left) -- "+
			"the concept scope is what stands between this statement and the whole graph", got)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM "MemoryNodes" WHERE id = ANY(ARRAY[$1,$2,$3])`, old, recent, bystander)
	})
}

// EVERY VERSION, not just the newest. MemoryNodes is keyed (id, createdAt), so
// a sweep that removed only the versions it selected would leave the rest as
// rows no read returns and no later sweep ever finds again.
func TestPruneDeletesEveryVersionOfARow(t *testing.T) {
	db := testDB(t)
	sfx := uniqueSuffix()
	now := time.Now().UTC()
	id := ConceptID + ":versioned-" + sfx

	// Three versions, all past the window.
	for i := 1; i <= 3; i++ {
		at := now.Add(-time.Duration(30+i) * 24 * time.Hour)
		insertActivityRow(t, db, ConceptID, id, at, at.Format(time.RFC3339Nano))
	}
	if got := countRows(t, db, id); got != 3 {
		t.Fatalf("seeded %d version(s), want 3 -- the test cannot measure what it did not create", got)
	}

	p := &Pruner{DB: func() *sql.DB { return db }, Retention: 30 * 24 * time.Hour, Logger: discard()}
	if _, err := p.PruneOnce(context.Background()); err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if got := countRows(t, db, id); got != 0 {
		t.Errorf("%d version(s) of a pruned row survived; the id will never be selected again, so "+
			"they are unreachable rows nothing will ever remove", got)
	}
}

// Idempotent, and safe when two replicas sweep at once. Deletes are the
// operation that makes this true -- a row a sibling already removed simply is
// not there -- which is why the job carries no advisory lock.
func TestPruneIsIdempotentAndConcurrent(t *testing.T) {
	db := testDB(t)
	sfx := uniqueSuffix()
	now := time.Now().UTC()

	var ids []string
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("%s:conc-%s-%d", ConceptID, sfx, i)
		ids = append(ids, id)
		at := now.Add(-time.Duration(40+i) * 24 * time.Hour)
		insertActivityRow(t, db, ConceptID, id, at, at.Format(time.RFC3339Nano))
	}

	newPruner := func() *Pruner {
		return &Pruner{
			DB:        func() *sql.DB { return db },
			Retention: 30 * 24 * time.Hour,
			BatchSize: 3, // force several batches
			Logger:    discard(),
		}
	}

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := newPruner().PruneOnce(context.Background())
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent sweep %d errored: %v", i, err)
		}
	}
	for _, id := range ids {
		if got := countRows(t, db, id); got != 0 {
			t.Errorf("%s survived two concurrent sweeps (%d version(s))", id, got)
		}
	}

	// A third, serial run over the same window must be a clean no-op.
	deleted, err := newPruner().PruneOnce(context.Background())
	if err != nil {
		t.Fatalf("re-running the sweep errored: %v", err)
	}
	if deleted != 0 {
		t.Errorf("a repeat sweep deleted %d row(s); it must be a no-op", deleted)
	}
}

// A row whose occurredAt is unreadable must not abort the sweep for every
// other row -- which is what a bare ::timestamptz cast would do.
func TestPruneToleratesAMalformedOccurredAt(t *testing.T) {
	db := testDB(t)
	sfx := uniqueSuffix()
	now := time.Now().UTC()

	bad := ConceptID + ":malformed-" + sfx
	good := ConceptID + ":alongside-" + sfx
	insertActivityRow(t, db, ConceptID, bad, now.Add(-40*24*time.Hour), "not-a-timestamp")
	insertActivityRow(t, db, ConceptID, good, now.Add(-40*24*time.Hour),
		now.Add(-40*24*time.Hour).Format(time.RFC3339Nano))

	p := &Pruner{DB: func() *sql.DB { return db }, Retention: 30 * 24 * time.Hour, Logger: discard()}
	if _, err := p.PruneOnce(context.Background()); err != nil {
		t.Fatalf("PruneOnce aborted on a malformed occurredAt: %v", err)
	}
	if got := countRows(t, db, good); got != 0 {
		t.Errorf("the well-formed row survived because a sibling's occurredAt was malformed (%d left)", got)
	}
	// The malformed one falls back to createdAt, which is also past the
	// window, so it goes too. Stated rather than assumed: the alternative
	// would be a row that can never be pruned.
	if got := countRows(t, db, bad); got != 0 {
		t.Errorf("the malformed row survived (%d left); with no readable occurredAt it must fall "+
			"back to createdAt, or it is unprunable forever", got)
	}
}
