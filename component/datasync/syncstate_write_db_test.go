package datasync

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// syncstate_write_db_test.go -- the sync runtime's own health row lands.
//
// A local cluster ran the reconciliation sweep every ten minutes for days
// and never held a single v1:platform:syncState row, so every domain was
// "due" on every tick whatever its cadence, and no sweep failure ever
// reached the Data origins page. Every runner discards WriteSyncState's
// error, so the refusal was invisible. This puts the exact write the runner
// makes -- the same store, the same operator context, through a real
// engine -- and demands it land and read back. It is db-gated: it skips
// without a database and fails under MEMQL_REQUIRE_DB=1.

func healthEngine(t *testing.T) (*memql.MemQLEngine, *sql.DB) {
	t.Helper()
	dsn := dbtest.DSN()
	raw := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = raw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "datasync health row", dsn, err)
		return nil, nil
	}
	db := bun.NewDB(raw, pgdialect.New())
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	return eng, raw
}

// engineSeam widens the engine's Execute to the store's one-method seam,
// the way plugin.go's engineAdapter does in production.
type engineSeam struct{ inner *memql.MemQLEngine }

func (e engineSeam) Execute(ctx context.Context, q string) (any, error) {
	return e.inner.Execute(ctx, q)
}

func TestTheSyncRuntimeHealthRowActuallyLands(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	store := NewStore(engineSeam{eng})
	ctx := OperatorContext(context.Background())
	// A concept id of this run's own, so the probe is re-runnable against a
	// database that keeps its rows, and swept away afterwards.
	concept := fmt.Sprintf("v1:shopify:dbtest-health-probe-%d", time.Now().UnixNano())
	const connector, direction = "shopify", "inbound"
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'conceptId' = $1`, concept)
	})

	before, err := store.SyncStateFor(ctx, concept, connector, direction)
	if err != nil {
		t.Fatalf("SyncStateFor before any write: %v", err)
	}
	if !before.LastReconcileAt.IsZero() {
		t.Fatalf("a fresh database already holds a health row: %+v", before)
	}

	before.LastError = "probe: the origin refused"
	before.LastReconcileAt = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	before.LastAttemptAt = time.Date(2026, 9, 27, 12, 30, 0, 0, time.UTC)
	if err := store.WriteSyncState(ctx, before); err != nil {
		t.Fatalf("WriteSyncState refused -- this is the refusal every runner discards: %v", err)
	}

	after, err := store.SyncStateFor(ctx, concept, connector, direction)
	if err != nil {
		t.Fatalf("SyncStateFor after the write: %v", err)
	}
	if after.LastError != before.LastError || !after.LastReconcileAt.Equal(before.LastReconcileAt) || !after.LastAttemptAt.Equal(before.LastAttemptAt) {
		t.Fatalf("the health row did not come back as written:\n  wrote %+v\n  read  %+v", before, after)
	}
}
