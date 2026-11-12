package datasync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// mirror_fence_db_test.go -- the applier's fence against the engine that
// enforces it. The fakes can say a write CARRIED a fence; only the engine can
// say that the version the guard read is the one its fence compares, and that
// a write the row moved under is refused. db-gated.

// overtakingEngine is the real engine with other writers in it: just before
// each of the first `burst` mirror writes of one row reaches the engine (one,
// when burst is not set), `meanwhile` runs with that write's number --
// another node's commit landing between the guard's read and the write.
type overtakingEngine struct {
	engineSeam
	rowID     string
	meanwhile func(n int)
	burst     int
	inserts   int
}

func (e *overtakingEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "insert(") && strings.Contains(q, e.rowID) {
		e.inserts++
		if e.inserts <= max(e.burst, 1) {
			e.meanwhile(e.inserts)
		}
	}
	return e.engineSeam.Execute(ctx, q)
}

func storedTitle(t *testing.T, db *sql.DB, rowID string) string {
	t.Helper()
	var title string
	if err := db.QueryRowContext(context.Background(),
		`SELECT payload->>'title' FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2 ORDER BY "createdAt" DESC LIMIT 1`,
		versionGuardConcept, "gid://shopify/Product/"+rowID).Scan(&title); err != nil {
		t.Fatalf("read the stored title: %v", err)
	}
	return title
}

// A WRITE OVERTAKEN BETWEEN THE GUARD AND THE WRITE DOES NOT PUT THE MIRROR
// BACK. The guard admits a 12:00 state against the row it read; before the
// write lands, another writer commits a 13:00 state. Unfenced, the 12:00 write
// replaces it and the mirror is an hour behind until the origin changes the
// record again. Fenced, the engine refuses it, the row is judged again, and
// the 12:00 state is skipped as stale. A write overtaken by an OLDER state is
// judged again and written; so is one to a row another writer created
// meanwhile.
func TestAWriteOvertakenBetweenTheGuardAndTheWriteDoesNotPutTheMirrorBack(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	ctx := auth.ContextWithConnectorActor(context.Background(), "shopify")
	specs := versionGuardSpecs()
	plain := NewApplier(NewStore(engineSeam{eng}), NewEngineMirrorWriter(engineSeam{eng}))
	const (
		t10 = "2026-08-23T10:00:00Z"
		t11 = "2026-08-23T11:00:00Z"
		t12 = "2026-08-23T12:00:00Z"
		t13 = "2026-08-23T13:00:00Z"
	)
	for _, tc := range []struct {
		name          string
		held          string // the state the mirror holds before, "" for no row
		meanwhile     string // the state another writer commits in between
		write         string // the state this write carries
		applied       int
		stale         int
		wantTitle     string
		wantVersion   string
		wantNoFailure bool
	}{
		{name: "overtaken by a newer state", held: t10, meanwhile: t13, write: t12, stale: 1, wantTitle: "meanwhile", wantVersion: t13},
		{name: "overtaken by an older state", held: t10, meanwhile: t11, write: t12, applied: 1, wantTitle: "this write", wantVersion: t12},
		{name: "created meanwhile, newer", meanwhile: t13, write: t12, stale: 1, wantTitle: "meanwhile", wantVersion: t13},
		{name: "created meanwhile, older", meanwhile: t11, write: t12, applied: 1, wantTitle: "this write", wantVersion: t12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rowID := fmt.Sprintf("shpdbtestfence%d", time.Now().UnixNano())
			t.Cleanup(func() {
				_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2`,
					versionGuardConcept, "gid://shopify/Product/"+rowID)
			})
			if tc.held != "" {
				if res, err := plain.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, tc.held, "held")}); err != nil || res.Applied != 1 {
					t.Fatalf("seed the held row: %+v, %v", res, err)
				}
			}
			racing := &overtakingEngine{engineSeam: engineSeam{eng}, rowID: rowID}
			racing.meanwhile = func(int) {
				if res, err := plain.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, tc.meanwhile, "meanwhile")}); err != nil || res.Applied != 1 {
					t.Errorf("the other writer's commit: %+v, %v", res, err)
				}
			}
			applier := NewApplier(NewStore(racing), NewEngineMirrorWriter(racing))
			res, err := applier.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, tc.write, "this write")})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if res.Applied != tc.applied || res.Stale != tc.stale {
				t.Fatalf("applied %d, stale %d; want %d and %d", res.Applied, res.Stale, tc.applied, tc.stale)
			}
			if got := storedTitle(t, db, rowID); got != tc.wantTitle {
				t.Fatalf("the mirror holds %q, want %q", got, tc.wantTitle)
			}
			got, _, err := NewEngineMirrorWriter(engineSeam{eng}).StoredVersion(ctx, specs[versionGuardConcept], rowID)
			if err != nil || got != tc.wantVersion {
				t.Fatalf("the mirror holds version %q (%v), want %q", got, err, tc.wantVersion)
			}
		})
	}
}

// A BURST OF OTHER WRITERS IS OUTLASTED. The origin sends several notices for
// one change, and their writes land within a fraction of a second of each
// other. A write that gave up at the first repeat would fail its batch, and
// the batch of a backfill page fails the backfill, which nothing starts again
// (Runner.backfillOne). Three other writers commit here, each just before this
// write's next try; it is judged again each time and written on the fourth,
// over the last of them.
func TestAMirrorWriteOutlastsABurstOfOtherWriters(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	ctx := auth.ContextWithConnectorActor(context.Background(), "shopify")
	specs := versionGuardSpecs()
	plain := NewApplier(NewStore(engineSeam{eng}), NewEngineMirrorWriter(engineSeam{eng}))
	rowID := fmt.Sprintf("shpdbtestfenceburst%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2`,
			versionGuardConcept, "gid://shopify/Product/"+rowID)
	})
	if res, err := plain.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, "2026-08-23T10:00:00Z", "held")}); err != nil || res.Applied != 1 {
		t.Fatalf("seed the held row: %+v, %v", res, err)
	}
	const burst = 3
	racing := &overtakingEngine{engineSeam: engineSeam{eng}, rowID: rowID, burst: burst}
	racing.meanwhile = func(n int) {
		// Each of the others is newer than the one before it and older than
		// the write under test, so every one of them moves the row and none
		// makes this write stale.
		at := fmt.Sprintf("2026-08-23T10:%02d:00Z", 10*n)
		if res, err := plain.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, at, "meanwhile")}); err != nil || res.Applied != 1 {
			t.Errorf("the other writer's commit %d: %+v, %v", n, res, err)
		}
	}
	applier := NewApplier(NewStore(racing), NewEngineMirrorWriter(racing))
	res, err := applier.Apply(ctx, "shopify", specs, []memqlsync.MirrorWrite{versionGuardWrite(rowID, "2026-08-23T12:00:00Z", "this write")})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied != 1 || res.Stale != 0 {
		t.Fatalf("applied %d, stale %d; want the write applied", res.Applied, res.Stale)
	}
	if racing.inserts != burst+1 {
		t.Fatalf("the write reached the engine %d times, want %d: once for each writer that overtook it, and once more", racing.inserts, burst+1)
	}
	if got := storedTitle(t, db, rowID); got != "this write" {
		t.Fatalf("the mirror holds %q, want this write", got)
	}
}

// AN ORDINARY WRITE PASSES ITS OWN FENCE. The version the guard reads off the
// raw read must be the very one the engine compares, to the microsecond, or
// every write to an existing row would be refused: a first write (the row must
// be absent), then three more, each fenced on the one before.
func TestAnOrdinaryMirrorWritePassesItsOwnFence(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	rowID := fmt.Sprintf("shpdbtestfenceown%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2`,
			versionGuardConcept, "gid://shopify/Product/"+rowID)
	})
	ctx := auth.ContextWithConnectorActor(context.Background(), "shopify")
	writer := NewEngineMirrorWriter(engineSeam{eng})
	applier := NewApplier(NewStore(engineSeam{eng}), writer)
	var last time.Time
	for hour := 10; hour < 14; hour++ {
		version := fmt.Sprintf("2026-08-23T%02d:00:00Z", hour)
		res, err := applier.Apply(ctx, "shopify", versionGuardSpecs(), []memqlsync.MirrorWrite{versionGuardWrite(rowID, version, version)})
		if err != nil || res.Applied != 1 {
			t.Fatalf("write %s: %+v, %v", version, res, err)
		}
		row, err := writer.StoredRow(ctx, versionGuardSpecs()[versionGuardConcept], rowID)
		if err != nil || !row.Exists || row.Version.IsZero() {
			t.Fatalf("after %s the stored row is %+v (%v), want it with its version", version, row, err)
		}
		if !row.Version.After(last) {
			t.Fatalf("after %s the row's version is %s, not after the last one %s", version, row.Version, last)
		}
		last = row.Version
	}
	var dbVersion time.Time
	if err := db.QueryRowContext(context.Background(),
		`SELECT "createdAt" FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2 ORDER BY "createdAt" DESC LIMIT 1`,
		versionGuardConcept, "gid://shopify/Product/"+rowID).Scan(&dbVersion); err != nil {
		t.Fatal(err)
	}
	if !dbVersion.Equal(last) {
		t.Fatalf("the guard read version %s, the database holds %s", last, dbVersion.UTC())
	}
}
