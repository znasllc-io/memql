package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

func workHeadsPeer(t *testing.T, db *bun.DB) *bun.DB {
	t.Helper()
	var path string
	if err := db.QueryRowContext(context.Background(), `SELECT current_schema()`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	peer := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": path}))), pgdialect.New())
	t.Cleanup(func() { _ = peer.Close() })
	return peer
}

func workHeadBatch(t *testing.T, db bun.IDB, size int) bool {
	t.Helper()
	var raw []byte
	if err := db.QueryRowContext(context.Background(), `SELECT refresh_work_run_heads(?)`, size).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var progress struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal(raw, &progress); err != nil {
		t.Fatal(err)
	}
	return progress.Ready
}

func workHeadInsert(t *testing.T, db bun.IDB, id, status string, at time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload) VALUES (?,'v1:work:run',?,'projection-test','{}',jsonb_build_object('status',?::text))`, id, at, status)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkHeadsDB_ReverseCommitOrderRollbackAndSourceChanges(t *testing.T) {
	db, i := sweepDB(t)
	peer := workHeadsPeer(t, db)
	catchUpWorkHeads(t, db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	slow, err := peer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Rollback()
	// The lower queue sequence remains uncommitted while a higher one is
	// consumed. A max-sequence watermark would lose this first run forever.
	workHeadInsert(t, slow, "lower-sequence", "waiting", now)
	workHeadInsert(t, db, "higher-sequence", "succeeded", now)
	catchUpWorkHeads(t, db)
	if err := slow.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := i.runsInFlight(ctx)
	if err != nil || len(rows) != 1 || rows[0]["id"] != "lower-sequence" {
		t.Fatalf("late lower sequence lost: %v %v", rows, err)
	}
	rollback, err := peer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	workHeadInsert(t, rollback, "rolled-back", "waiting", now)
	if err := rollback.Rollback(); err != nil {
		t.Fatal(err)
	}
	workHeadInsert(t, db, "lower-sequence", "succeeded", now.Add(time.Second))
	rows, err = i.runsInFlight(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("terminal or rolled-back work resurrected: %v %v", rows, err)
	}
	// Retention removes exact old versions; deleting the head exposes the
	// surviving historical version, and deleting the whole ID removes it.
	if _, err := db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id='lower-sequence' AND "createdAt"=?`, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	rows, err = i.runsInFlight(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("surviving head missing: %v %v", rows, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id='lower-sequence'`); err != nil {
		t.Fatal(err)
	}
	rows, err = i.runsInFlight(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("deleted head retained: %v %v", rows, err)
	}
	workHeadInsert(t, db, "lower-sequence", "waiting", now.Add(-time.Hour))
	rows, err = i.runsInFlight(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("reinsert at older timestamp lost: %v %v", rows, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE "MemoryNodes" SET id='renamed', "createdAt"="createdAt"+interval '1 second' WHERE id='lower-sequence'`); err != nil {
		t.Fatal(err)
	}
	rows, err = i.runsInFlight(ctx)
	if err != nil || len(rows) != 1 || rows[0]["id"] != "renamed" {
		t.Fatalf("updated keys lost: %v %v", rows, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE "MemoryNodes" SET concept='v1:work:observation' WHERE id='renamed'`); err != nil {
		t.Fatal(err)
	}
	rows, err = i.runsInFlight(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("old concept leaked: %v %v", rows, err)
	}
}

func TestWorkHeadsDB_BackfillRestartConcurrentChangesAndReset(t *testing.T) {
	db, i := sweepDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, id := range []string{"b", "d", "f"} {
		workHeadInsert(t, db, id, "waiting", now)
	}
	if workHeadBatch(t, db, 1) {
		t.Fatal("partial backfill reported ready")
	}
	// These changes land behind the durable backfill cursor, including a
	// deleted source row. Reopening a connection does not reset progress.
	peer := workHeadsPeer(t, db)
	workHeadInsert(t, peer, "a", "waiting", now)
	if _, err := peer.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id='b'`); err != nil {
		t.Fatal(err)
	}
	workHeadInsert(t, peer, "d", "succeeded", now.Add(time.Second))
	catchUpWorkHeads(t, peer)
	rows, err := i.runsInFlight(ctx)
	if err != nil || len(rows) != 2 || rows[0]["id"] != "a" || rows[1]["id"] != "f" {
		t.Fatalf("backfill race: %v %v", rows, err)
	}
	for _, table := range []string{"work_run_heads", "work_run_head_events"} {
		if _, err := db.ExecContext(ctx, `TRUNCATE `+table); err != nil {
			t.Fatal(err)
		}
		var complete bool
		if err := db.QueryRowContext(ctx, `SELECT backfill_complete FROM work_run_head_state`).Scan(&complete); err != nil || complete {
			t.Fatalf("%s reset hid invalid state: %v", table, err)
		}
		catchUpWorkHeads(t, db)
		rows, err = i.runsInFlight(ctx)
		if err != nil || len(rows) != 2 {
			t.Fatalf("%s reset lost live work: %v %v", table, rows, err)
		}
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE "MemoryNodes" DISABLE TRIGGER work_run_head_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := i.runsInFlight(ctx); err == nil || !strings.Contains(err.Error(), "capture triggers") {
		t.Fatalf("disabled capture did not refuse: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE "MemoryNodes" ENABLE TRIGGER work_run_head_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE "MemoryNodes"`); err != nil {
		t.Fatal(err)
	}
	rows, err = i.runsInFlight(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("source reset retained heads: %v %v", rows, err)
	}
}

func TestWorkHeadsDB_SnapshotIsolationAndProjectorExclusion(t *testing.T) {
	db, _ := sweepDB(t)
	peer := workHeadsPeer(t, db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	workHeadInsert(t, db, "changing", "waiting", now)
	catchUpWorkHeads(t, db)
	snapshot, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback()
	if !workHeadBatch(t, snapshot, 1) {
		t.Fatal("complete projection unavailable")
	}
	workHeadInsert(t, peer, "changing", "succeeded", now.Add(time.Second))
	var status string
	if err := snapshot.QueryRowContext(ctx, `SELECT status FROM work_run_heads WHERE id='changing'`).Scan(&status); err != nil || status != "waiting" {
		t.Fatalf("snapshot changed: %s %v", status, err)
	}
	var raw []byte
	if err := peer.QueryRowContext(ctx, `SELECT refresh_work_run_heads(1)`).Scan(&raw); err == nil || !strings.Contains(err.Error(), "55P03") {
		t.Fatalf("concurrent projector did not refuse promptly: %v", err)
	}
	if err := snapshot.Commit(); err != nil {
		t.Fatal(err)
	}
	catchUpWorkHeads(t, peer)
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_run_heads WHERE id='changing'`).Scan(&status); err != nil || status != "succeeded" {
		t.Fatalf("next snapshot lost terminal event: %s %v", status, err)
	}
}

func TestWorkHeadsDB_BacklogCommitsProgressButRefusesPartialRecovery(t *testing.T) {
	db, i := sweepDB(t)
	ctx := context.Background()
	catchUpWorkHeads(t, db)
	// Unique dirty IDs exceed one sweep's four bounded batches. Progress must
	// commit despite the returned not-ready error, and the next call succeeds.
	now := time.Now().UTC()
	for n := 0; n < 4100; n++ {
		workHeadInsert(t, db, fmt.Sprintf("queued-%05d", n), "waiting", now)
	}
	if rows, err := i.runsInFlight(ctx); err == nil || !strings.Contains(err.Error(), "catching up") || len(rows) != 0 {
		t.Fatalf("partial projection presented as recovery: %v %v", len(rows), err)
	}
	var pending int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM work_run_head_events`).Scan(&pending); err != nil || pending != 100 {
		t.Fatalf("progress rolled back: %d %v", pending, err)
	}
	rows, err := i.runsInFlight(ctx)
	if err != nil || len(rows) != sweepPageSize {
		t.Fatalf("catch-up did not resume: %d %v", len(rows), err)
	}
}

func TestWorkHeadsDB_StaleSnapshotAbortsWholeProjectorTransaction(t *testing.T) {
	db, _ := sweepDB(t)
	peer := workHeadsPeer(t, db)
	ctx := context.Background()
	now := time.Now().UTC()
	catchUpWorkHeads(t, db)
	workHeadInsert(t, db, "stale-projector", "waiting", now)
	stale, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Rollback()
	var count int
	if err := stale.QueryRowContext(ctx, `SELECT count(*) FROM work_run_head_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	catchUpWorkHeads(t, peer)
	var raw []byte
	if err := stale.QueryRowContext(ctx, `SELECT refresh_work_run_heads(1000)`).Scan(&raw); err == nil || !strings.Contains(err.Error(), "40001") {
		t.Fatalf("stale projector did not serialize: %v", err)
	}
	if err := stale.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !workHeadBatch(t, db, 1000) {
		t.Fatal("fresh transaction did not recover")
	}
}
