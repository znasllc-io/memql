package backup

// snapshot_db_test.go -- the export must read every page from ONE snapshot
// (memql#4043).
//
// The defect: eachRow pages with LIMIT/OFFSET and each page is a separate
// statement, so a DELETE landing before the current offset shifts every later
// row backward and the next page's OFFSET steps over one. The result is a row
// that existed before the export began, was never itself deleted, and is absent
// from the backup -- with exit 0, no error, and a manifest whose counts agree
// with the short stream because they are counted from it.
//
// The two tests below are the SAME probe over the SAME fixture, differing in one
// thing: whether the reads are wrapped in a snapshot.
//
//	TestUnsnapshottedPagingSilentlyOmitsARow  -> eachRow directly, omission REPRODUCED
//	TestReadTablesUnderOneSnapshotOmitsNothing -> the production path, omission GONE
//
// The first is not decoration and it is not a test of dead code. It is the
// POSITIVE CONTROL: without it, the second test passing is equally consistent
// with "the fix works" and "the probe cannot detect the bug at all", and those
// two readings are indistinguishable from a green. It is what makes the second
// test evidence rather than an assertion. So eachRow must stay unsnapshotted on
// its own -- moving the transaction down into it would silently disarm the
// control while leaving both tests green.
//
// Determinism, since a concurrent-write bug reproduced by timing would be a
// flake in someone else's PR: the delete is fired from inside the per-row
// callback at the exact page-1 boundary, so there is no race to win. The probe
// rows are stamped with an ancient createdAt so they sort at the head of the
// (createdAt, id) ordering, which is where the damage is done and where the
// page boundary therefore has to fall.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// probeRows is one more than a page plus enough margin that the boundary lands
// comfortably inside the block even if the shared database already holds a few
// rows older than the fixture. eachRow's page is 2000, so anything at or below
// that crosses no boundary and the probe would report a clean export for the
// trivial reason that it never paged.
const probeRows = 2200

// probeCreatedAt is deliberately ancient. Ordering is (createdAt, id), so this
// puts the whole block at the HEAD of the table -- both because that is where a
// deletion does its damage (the real trigger, knowledge chunk purges, carries
// old timestamps for the same reason) and because it makes the page-1 boundary
// fall inside the fixture rather than somewhere in another suite's rows.
var probeCreatedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// probePool opens a connection pool against the shared test database.
//
// A POOL, not a transaction, and unlike stagedBackupTx that is forced rather
// than chosen: the whole point is a delete that is COMMITTED and visible to a
// different connection mid-export, which is unreachable from inside a single
// transaction. The cost is that this fixture is really in the database, so it
// cleans up after itself instead of rolling back.
func probePool(t *testing.T) (*bun.DB, context.Context) {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "backup snapshot test", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, ctx
}

// seedProbeRows commits probeRows rows whose ids sort in ascending order, and
// registers their removal. Returns the id prefix and the ids in sort order.
func seedProbeRows(t *testing.T, ctx context.Context, db bun.IDB, tag string) (prefix string, ids []string) {
	t.Helper()
	prefix = fmt.Sprintf("v1:memql:backupSnapshotProbe:%s-%d-", tag, os.Getpid())

	rows := make([]memoryNodes.MemoryNode, 0, probeRows)
	ids = make([]string, 0, probeRows)
	for i := range probeRows {
		// Zero-padded so lexical id order matches numeric order; every row
		// shares one createdAt, so id IS the tiebreak and the ordering is
		// fully determined.
		id := fmt.Sprintf("%s%06d", prefix, i)
		ids = append(ids, id)
		rows = append(rows, memoryNodes.MemoryNode{
			ID:         id,
			CreatedAt:  probeCreatedAt,
			CreatedBy:  "system:backup-snapshot-test",
			Concept:    "v1:memql:backupSnapshotProbe",
			Type:       "object",
			Schema:     json.RawMessage(`{"v":1}`),
			Payload:    json.RawMessage(`{"probe":true}`),
			Metadata:   json.RawMessage(`{}`),
			Provenance: json.RawMessage(`{"mutation":"backupSnapshotTest"}`),
		})
	}

	// Registered BEFORE the insert: a failure partway through still leaves rows
	// behind, and this fixture is committed, so the next run of any suite in
	// this lane would inherit them.
	t.Cleanup(func() {
		if _, err := db.NewDelete().
			Model((*memoryNodes.MemoryNode)(nil)).
			ModelTableExpr(`"MemoryNodes" AS mn`).
			Where(`mn.id LIKE ?`, prefix+"%").
			Exec(context.Background()); err != nil {
			t.Errorf("cleanup probe rows %q: %v", prefix, err)
		}
	})

	if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		t.Fatalf("seed %d probe rows: %v", probeRows, err)
	}
	return prefix, ids
}

// deleteAtPageBoundary returns a per-row callback that records every id it is
// handed and, at the exact moment page 1 has been fully delivered, commits a
// DELETE of an EARLY row over a separate connection.
//
// Separate connection is load-bearing twice over: it is what makes the delete
// visible to an unsnapshotted reader, and it is what makes it INVISIBLE to a
// snapshotted one. A plain SELECT takes ACCESS SHARE, which does not conflict
// with a row delete, so the delete cannot block on the reader.
func deleteAtPageBoundary(t *testing.T, deleter *bun.DB, victim string, seen *[]string) func(Row) error {
	t.Helper()
	fired := false
	return func(r Row) error {
		*seen = append(*seen, r.ID)
		if !fired && len(*seen) == 2000 {
			fired = true
			res, err := deleter.NewDelete().
				Model((*memoryNodes.MemoryNode)(nil)).
				ModelTableExpr(`"MemoryNodes" AS mn`).
				Where(`mn.id = ?`, victim).
				Exec(context.Background())
			if err != nil {
				t.Fatalf("delete %q at the page-1 boundary: %v", victim, err)
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				t.Fatalf("delete %q at the page-1 boundary removed %d rows, want 1", victim, n)
			}
		}
		return nil
	}
}

// missingFrom reports which of want were never seen, ignoring the one row the
// probe itself deleted.
func missingFrom(want []string, seen []string, deleted string) []string {
	got := make(map[string]struct{}, len(seen))
	for _, id := range seen {
		got[id] = struct{}{}
	}
	var missing []string
	for _, id := range want {
		if id == deleted {
			continue
		}
		if _, ok := got[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// TestUnsnapshottedPagingSilentlyOmitsARow is the POSITIVE CONTROL: it proves
// the probe above can actually detect the defect.
//
// It reads through eachRow directly -- the paging with no snapshot around it --
// and asserts the omission HAPPENS. If this test ever starts failing, the
// correct response is NOT to delete it: it means either eachRow acquired a
// snapshot of its own (in which case the sibling test below is no longer
// evidence of anything and the control needs re-pointing at whatever is now
// unsnapshotted) or the probe stopped exercising a page boundary. Both are
// findings. A silently-removed control is how the sibling test would go on
// passing forever while measuring nothing.
func TestUnsnapshottedPagingSilentlyOmitsARow(t *testing.T) {
	db, ctx := probePool(t)
	_, ids := seedProbeRows(t, ctx, db, "bare")

	deleter, _ := probePool(t)
	victim := ids[5] // early: well before the page-1 boundary

	var seen []string
	if err := eachRow(ctx, db, TableMemoryNodes, deleteAtPageBoundary(t, deleter, victim, &seen)); err != nil {
		t.Fatalf("eachRow: %v", err)
	}

	missing := missingFrom(ids, seen, victim)
	if len(missing) == 0 {
		t.Fatalf("CONTROL FAILED: unsnapshotted paging omitted nothing, so this probe cannot "+
			"detect the memql#4043 defect and TestReadTablesUnderOneSnapshotOmitsNothing "+
			"proves nothing. Read %d rows across the boundary after deleting %q.",
			len(seen), victim)
	}
	t.Logf("control: %d row(s) silently omitted by unsnapshotted paging, first %q "+
		"(existed before the read, never itself deleted)", len(missing), missing[0])
}

// TestReadTablesUnderOneSnapshotOmitsNothing is the fix.
//
// Same fixture, same probe, same delete at the same boundary -- routed through
// the function Export actually calls. Every seeded row must come back.
func TestReadTablesUnderOneSnapshotOmitsNothing(t *testing.T) {
	db, ctx := probePool(t)
	_, ids := seedProbeRows(t, ctx, db, "snap")

	deleter, _ := probePool(t)
	victim := ids[5]

	var seen []string
	err := readTablesUnderOneSnapshot(ctx, db, []string{TableMemoryNodes},
		deleteAtPageBoundary(t, deleter, victim, &seen))
	if err != nil {
		t.Fatalf("readTablesUnderOneSnapshot: %v", err)
	}

	// The victim is expected back too: it was present when the snapshot was
	// taken, and a backup records the state the export began from.
	if missing := missingFrom(ids, seen, ""); len(missing) > 0 {
		t.Fatalf("snapshotted read omitted %d row(s), first %q -- a row present before the "+
			"export and never deleted is absent from the backup (memql#4043)",
			len(missing), missing[0])
	}
}

// TestExportRefusesACallerTransactionWithNoSnapshot pins the fail-CLOSED half,
// and the reason it needs pinning is a footgun in the library rather than in
// this package: bun's Tx.RunInTx DISCARDS its *sql.TxOptions and opens a
// savepoint, so an export handed a READ COMMITTED transaction would page with no
// snapshot no matter what isolation level the code above asked for. Silently.
//
// This also proves Export routes through the snapshot path at all -- the error
// cannot be produced any other way.
func TestExportRefusesACallerTransactionWithNoSnapshot(t *testing.T) {
	db, ctx := probePool(t)

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("begin read-committed tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = Export(ctx, tx, io.Discard, Options{EngineVersion: "0.0.0-test", Domain: "memql.localhost"})
	if err == nil {
		t.Fatal("Export accepted a READ COMMITTED transaction; it pages with LIMIT/OFFSET, " +
			"so it would silently omit rows a concurrent delete shifted past the offset")
	}
	if !strings.Contains(err.Error(), "read committed") {
		t.Fatalf("refusal should name the isolation level it found, got: %v", err)
	}
}

// TestExportAcceptsARepeatableReadCallerTransaction is the other side of that
// gate: a caller that DOES hold a snapshot is not turned away. Without this, the
// refusal above is equally consistent with "Export rejects every transaction",
// which would break the sibling suite in this package and every caller that
// wraps an export for its own reasons.
func TestExportAcceptsARepeatableReadCallerTransaction(t *testing.T) {
	db, ctx := probePool(t)

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatalf("begin repeatable-read tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := Export(ctx, tx, io.Discard, Options{EngineVersion: "0.0.0-test", Domain: "memql.localhost"}); err != nil {
		t.Fatalf("Export refused a REPEATABLE READ transaction, which does hold one snapshot: %v", err)
	}
}
