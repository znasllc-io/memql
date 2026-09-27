package shopify

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/datasync"
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
// engine -- and demands it land and read back.

// syncEngine widens the engine the way component/datasync's own plugin
// adapter does (plugin.go engineAdapter), which is unexported.
type syncEngine struct{ inner *memql.MemQLEngine }

func (e syncEngine) Execute(ctx context.Context, q string) (any, error) {
	return e.inner.Execute(ctx, q)
}

func TestTheSyncRuntimeHealthRowActuallyLands(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	store := datasync.NewStore(syncEngine{eng})
	ctx := datasync.OperatorContext(context.Background())
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
	if err := store.WriteSyncState(ctx, before); err != nil {
		t.Fatalf("WriteSyncState refused -- this is the refusal every runner discards: %v", err)
	}

	after, err := store.SyncStateFor(ctx, concept, connector, direction)
	if err != nil {
		t.Fatalf("SyncStateFor after the write: %v", err)
	}
	if after.LastError != before.LastError || !after.LastReconcileAt.Equal(before.LastReconcileAt) {
		t.Fatalf("the health row did not come back as written:\n  wrote %+v\n  read  %+v", before, after)
	}
}
