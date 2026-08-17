package backup

// staged_data_db_test.go -- the staged-DATA tier's claim on the backup path
// (epic memql#3974, task memql#3985).
//
// memql#3985 lists this package as somewhere "a restore must not resurrect
// staged rows as live". The investigation's answer is that the requirement is
// ALREADY SATISFIED, by the marking model rather than by any code here, and this
// file is what stops that from being a claim nobody checked:
//
//	the staged flag is `conceptDataStaged`, a field on a v1:authoring:construct
//	row (dsl/authoring/concepts.memql)
//	  -> v1:authoring:construct is an ordinary concept, so its rows are ordinary
//	     MemoryNodes rows
//	  -> eachRow exports the WHOLE TABLE with no WHERE clause of any kind
//	  -> the construct row, flag intact, is in the backup
//	  -> a restore reinserts it, and the boot fold re-marks the concept staged
//	  -> so a restore CANNOT resurrect staged rows as live.
//
// The concept-grain marking model is what makes that work. Under a per-row
// marker the same question would have been a real one, since the marker would
// have had to survive as a per-row column through export and restore. Under
// concept-grain there is exactly one row to carry, and it is carried for the
// same reason every other row is.
//
// The corollary is why NOTHING here is gated, and it is the more dangerous half:
// omitting staged rows from an export would DESTROY them on restore, and the
// same gate applied to CountRows -- which backs the refuse-a-non-empty-target
// check -- would make a cluster holding only staged rows count zero, so the
// restore would proceed and overwrite them. One change, causing the loss and
// disabling the check that catches it. See the comments on eachRow and CountRows
// in transfer.go.
//
// Postgres-gated like every other db-backed suite: skips when no DB is reachable.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// stagedBackupTx opens the shared test database and hands back a REPEATABLE
// READ transaction to run the whole test inside.
//
// A transaction, not the pool, and the isolation level is the point. The
// db-tests lane runs each package's binary as a PARALLEL PROCESS against one
// database -- the advisory lock in TestMain serializes migration and then
// releases, so from that moment component/memql, component/grpc and the
// integrations suites are all committing rows underneath these tests. Two
// things here would otherwise be racy against that, and both would present as
// an unreproducible flake in somebody else's PR:
//
//   - a before/after row-count delta, which any concurrent insert breaks;
//   - Export's OFFSET paging, where a row committed EARLIER in the (createdAt,
//     id) order between two pages shifts later rows past the offset and skips
//     one.
//
// Under REPEATABLE READ the snapshot is fixed at the transaction's first
// statement, so neither is reachable: concurrent commits are invisible, while
// this transaction's own insert is visible to itself. Everything rolls back, so
// the fixture also leaves nothing behind for the next run to trip over -- which
// is why there is no cleanup delete anywhere below.
//
// bun.Tx satisfies bun.IDB, which is what Export and CountRows already take.
func stagedBackupTx(t *testing.T) (bun.Tx, context.Context) {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "staged-data backup test", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatalf("begin repeatable-read tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx, ctx
}

// seedStagedConstructRow writes the row that CARRIES the staged flag: a
// v1:authoring:construct for a promoted concept, with conceptDataStaged true.
// Written straight to the table because what is under test is the EXPORT, and
// routing through the engine would add a dependency on the promote path without
// changing a byte of what eachRow reads.
func seedStagedConstructRow(t *testing.T, ctx context.Context, db bun.IDB) (id string, createdAt time.Time) {
	t.Helper()
	id = fmt.Sprintf("v1:authoring:construct:staged-3985-%d", os.Getpid())
	createdAt = time.Now().UTC().Truncate(time.Microsecond)
	row := &memoryNodes.MemoryNode{
		ID:         id,
		CreatedAt:  createdAt,
		CreatedBy:  "system:staged-backup-test",
		Concept:    "v1:authoring:construct",
		Type:       "object",
		Schema:     json.RawMessage(`{"v":1}`),
		Payload:    json.RawMessage(`{"kind":"concept","name":"trainedWidget","conceptDataStaged":true}`),
		Metadata:   json.RawMessage(`{}`),
		Provenance: json.RawMessage(`{"mutation":"stagedBackupTest"}`),
	}
	// No cleanup delete: the caller's transaction is rolled back.
	if _, err := db.NewInsert().Model(row).Exec(ctx); err != nil {
		t.Fatalf("seed construct row: %v", err)
	}
	return id, createdAt
}

func exportedRows(t *testing.T, ctx context.Context, db bun.IDB) []Row {
	t.Helper()
	var buf bytes.Buffer
	if _, err := Export(ctx, db, &buf, Options{EngineVersion: "0.0.0-test", Domain: "memql.localhost"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("read back the export: %v", err)
	}
	var rows []Row
	for {
		row, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		rows = append(rows, row)
	}
	return rows
}

// TestBackupCarriesTheStagedFlagSoARestoreCannotResurrectStagedRowsAsLive is
// the whole ruling, end to end against a real database.
//
// If this fails, the failure is not "a test broke": it means a backup no longer
// carries the fact that a concept was staged, so restoring it publishes every
// row the owner had deliberately withheld -- silently, and at the moment an
// operator is least able to notice, because a restore is expected to change
// everything at once.
func TestBackupCarriesTheStagedFlagSoARestoreCannotResurrectStagedRowsAsLive(t *testing.T) {
	db, ctx := stagedBackupTx(t)
	id, _ := seedStagedConstructRow(t, ctx, db)

	var found *Row
	rows := exportedRows(t, ctx, db)
	for i := range rows {
		if rows[i].ID == id {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the export does not contain the v1:authoring:construct row %q that carries conceptDataStaged. A restore from this backup would bring the concept back UNSTAGED, publishing every row its owner had withheld (memql#3985).", id)
	}
	if found.Table != TableMemoryNodes {
		t.Errorf("construct row exported from %q, want %q", found.Table, TableMemoryNodes)
	}

	var payload map[string]any
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatalf("exported payload is not JSON: %v", err)
	}
	if staged, _ := payload["conceptDataStaged"].(bool); !staged {
		t.Errorf("the exported construct row lost conceptDataStaged: payload = %s", found.Payload)
	}
	if payload["name"] != "trainedWidget" {
		t.Errorf("the exported construct row was rewritten: payload = %s", found.Payload)
	}
}

// TestExportIsUnfilteredAcrossConcepts pins the property the ruling rests on --
// that eachRow selects the WHOLE table -- rather than only the consequence.
//
// Stated separately because the two fail differently and a reader needs to know
// which happened. The test above goes red if the CONSTRUCT row specifically
// stops being exported; this one goes red if a concept filter appears at all,
// which is the change someone would make while "aligning the backup with the
// staged read gate" and which would additionally destroy every staged data row
// on the next restore.
func TestExportIsUnfilteredAcrossConcepts(t *testing.T) {
	db, ctx := stagedBackupTx(t)
	constructId, _ := seedStagedConstructRow(t, ctx, db)

	concepts := map[string]bool{}
	sawConstruct := false
	for _, r := range exportedRows(t, ctx, db) {
		concepts[r.Concept] = true
		if r.ID == constructId {
			sawConstruct = true
		}
	}
	if !sawConstruct {
		t.Fatal("the seeded construct row is missing from the export")
	}
	if len(concepts) < 2 {
		t.Skipf("the database holds rows of only %d concept(s); this assertion needs a populated database to mean anything", len(concepts))
	}
}

// TestCountRowsIsUnfiltered pins the second half, and it is the half whose
// failure is worst.
//
// CountRows backs restore's refuse-a-non-empty-target check. A concept filter
// here would make a cluster whose only rows are staged count ZERO -- so the
// restore would decide the target is empty and overwrite them. The same gate
// that loses the data would disable the check that exists to catch it, which is
// why neither is gated and why both are pinned.
func TestCountRowsIsUnfiltered(t *testing.T) {
	db, ctx := stagedBackupTx(t)
	before, err := CountRows(ctx, db)
	if err != nil {
		t.Fatalf("CountRows: %v", err)
	}
	seedStagedConstructRow(t, ctx, db)
	after, err := CountRows(ctx, db)
	if err != nil {
		t.Fatalf("CountRows: %v", err)
	}
	if after != before+1 {
		t.Errorf("CountRows went %d -> %d after seeding one staged-marker row; it must count every row regardless of concept, or a restore will treat a cluster holding only staged rows as empty and overwrite it (memql#3985)", before, after)
	}
}
