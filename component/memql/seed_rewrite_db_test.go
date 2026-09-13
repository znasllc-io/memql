package memql

import (
	"context"
	"testing"
)

// A second boot sweep over an unchanged seed appends NO version. Every boot
// used to append one per seed row regardless (memql.znas.io, 2026-09-13: 215
// capability ids carried 19,277 versions), and each raised the catalog-reload
// event on every node for a change that had not happened.
func TestASecondSeedSweepAppendsNoVersionToAnUnchangedRow(t *testing.T) {
	eng, db, ctx := readMergeTestEngine(t)
	if eng.seedMaterializer == nil {
		t.Fatalf("Init did not wire the seed materializer")
	}
	const rowId = "v1:rbac:capability:cap-owner-read-app-logs-stream"
	versions := func() int {
		var n int
		if err := db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE id = ?`, rowId).Scan(ctx, &n); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		return n
	}

	sweep := func(label string) {
		if err := eng.seedMaterializer.Start(context.Background()); err != nil {
			t.Fatalf("%s sweep: %v", label, err)
		}
		_ = eng.seedMaterializer.Stop(context.Background())
	}

	sweep("first")
	first := versions()
	if first == 0 {
		t.Fatalf("the first sweep did not materialize %s", rowId)
	}
	sweep("second")
	if second := versions(); second != first {
		t.Fatalf("an unchanged seed row went from %d to %d versions across one sweep; want no new version", first, second)
	}
}
