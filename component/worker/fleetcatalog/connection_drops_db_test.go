package fleetcatalog_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/component/worker/fleetcatalog"
)

// A drop is written by the agent replica that held the machine's stream and
// read by whichever replica places the next pipeline step (network-aware fleet
// selection, section 4 and section 8). The two are different engines here, as
// they are different replicas in a cluster, and the reader must see the
// writer's rows at once (the query is @cache(0)), only the caller's own, only
// the machine asked about, only inside the window, newest first, under either
// spelling of the machine's id.
func TestConnectionDropsRoundTripAcrossReplicasAndOwners(t *testing.T) {
	ctx := context.Background()
	db, holder, planner, ok := dropEngines(t)
	if !ok {
		return
	}

	now := time.Now().UTC()
	prefix := fmt.Sprintf("connection-drops-%d", now.UnixNano())
	owner := "v1:identity:user:" + prefix + "-owner"
	stranger := "v1:identity:user:" + prefix + "-stranger"
	machine, other := prefix+"-m", prefix+"-n"
	cleanUpDrops(t, db, prefix)

	writer := &workerservice.EngineStore{Engine: holder}
	record := func(registrationId, cause, nodeId string, at time.Time, inFlight int) {
		t.Helper()
		if err := writer.RecordConnectionDrop(ctx, workerservice.ConnectionDrop{
			RegistrationId: registrationId, OwnerUserId: owner, DroppedAt: at, ConnectedAt: at.Add(-time.Minute),
			Cause: cause, WorkInFlight: inFlight, NodeId: nodeId, Code: "unknown", Reason: "context canceled",
		}); err != nil {
			t.Fatalf("record a drop of %s: %v", registrationId, err)
		}
	}
	old, recent, newest := now.Add(-3*time.Hour), now.Add(-30*time.Minute), now.Add(-10*time.Minute)
	record(machine, workerservice.DropCauseReplaced, "agent-a", old, 0)
	record(machine, workerservice.DropCauseWorkInFlight, "agent-a", recent, 1)
	// The same machine under its canonical id, as a caller on the other side of
	// a seam may spell it.
	record("v1:worker:registration:"+machine, workerservice.DropCauseReplaced, "agent-b", newest, 0)
	record(other, workerservice.DropCauseReplaced, "agent-a", now.Add(-5*time.Minute), 0)

	reader := &fleetcatalog.EngineStore{Engine: planner}
	since := now.Add(-2 * time.Hour)
	for _, asked := range []string{machine, "v1:worker:registration:" + machine} {
		got, err := reader.ConnectionDrops(ctx, owner, asked, since)
		if err != nil {
			t.Fatalf("read drops of %s: %v", asked, err)
		}
		if len(got) != 2 {
			t.Fatalf("drops of %s since %s = %+v, want the two inside the window", asked, since, got)
		}
		want := []struct {
			at           time.Time
			cause, node  string
			workInFlight int
		}{{newest, workerservice.DropCauseReplaced, "agent-b", 0}, {recent, workerservice.DropCauseWorkInFlight, "agent-a", 1}}
		for i, w := range want {
			d := got[i]
			if !d.DroppedAt.Equal(w.at) || d.Cause != w.cause || d.NodeId != w.node || d.WorkInFlight != w.workInFlight {
				t.Fatalf("drop %d of %s = %+v, want %+v (newest first)", i, asked, d, w)
			}
			if strings.TrimPrefix(d.RegistrationId, "v1:worker:registration:") != machine {
				t.Fatalf("drop %d names machine %q, want %q", i, d.RegistrationId, machine)
			}
		}
	}
	if got, err := reader.ConnectionDrops(ctx, stranger, machine, since); err != nil || len(got) != 0 {
		t.Fatalf("another owner read %+v (%v); a machine's drops are its owner's", got, err)
	}
}

// SAME-SECOND DROPS. The window and the order are decided on the stored text,
// in byte order, so a time written with a fraction of varying length orders
// wrongly inside one second: "...:56Z" sorts after "...:56.7Z". A drop 700 ms
// into the window's first second is inside the window, and the newer of two
// drops in one second comes first.
func TestConnectionDropsInOneSecondReadInTimeOrder(t *testing.T) {
	ctx := context.Background()
	db, holder, planner, ok := dropEngines(t)
	if !ok {
		return
	}
	second := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	prefix := fmt.Sprintf("connection-drops-second-%d", second.UnixNano())
	owner, machine := "v1:identity:user:"+prefix+"-owner", prefix+"-m"
	cleanUpDrops(t, db, prefix)

	writer := &workerservice.EngineStore{Engine: holder}
	for _, at := range []time.Time{second, second.Add(700 * time.Millisecond)} {
		if err := writer.RecordConnectionDrop(ctx, workerservice.ConnectionDrop{
			RegistrationId: machine, OwnerUserId: owner, DroppedAt: at,
			Cause: workerservice.DropCauseReplaced, NodeId: "agent-a", Code: "unknown", Reason: "superseded",
		}); err != nil {
			t.Fatalf("record a drop at %s: %v", at, err)
		}
	}
	got, err := (&fleetcatalog.EngineStore{Engine: planner}).ConnectionDrops(ctx, owner, machine, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].DroppedAt.Equal(second.Add(700*time.Millisecond)) || !got[1].DroppedAt.Equal(second) {
		t.Fatalf("drops since %s = %+v, want both, the later one first", second.Format(time.RFC3339Nano), got)
	}
}

// dropEngines is two engines on the test database: the replica that holds a
// machine's stream and writes its drops, and the one that places the next step
// and reads them. ok is false when the database is unreachable (dbtest decides
// whether that skips or fails).
func dropEngines(t *testing.T) (*bun.DB, *memql.MemQLEngine, *memql.MemQLEngine, bool) {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "fleet connection drops", dbtest.DSN(), err)
		return nil, nil, nil, false
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engine := func() *memql.MemQLEngine {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		e.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		return e
	}
	return db, engine(), engine(), true
}

// cleanUpDrops removes the drops a test wrote, by its owner prefix.
func cleanUpDrops(t *testing.T, db *bun.DB, prefix string) {
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("concept = ?", "v1:worker:connectionDrop").
			Where("payload->>'ownerUserId' LIKE ?", "%"+prefix+"%").Exec(context.Background())
	})
}
