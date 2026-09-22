package shopify

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// mirror_write_end_to_end_db_test.go -- one mirror write, all the way through
// the executor (memql#5574).
//
// # The gap this closes
//
// `component/memql/data_origins_enforcement_test.go` is thorough about the
// mirror write GUARD: it calls `guardMirrorWrite` directly and holds every
// actor but the named connector to a refusal. What it never does is run a
// STATEMENT, and the executor resolves things the guard does not -- among them
// the writing actor, for the `createdBy` stamp.
//
// So the connector could pass every gate that test measures and still write
// nothing. It did: `ContextWithConnectorActor` stamped the AccessContext and
// not TokenInfo, `ActorFromContext` reads the latter, and every mirror insert
// answered `no actor found in context`. A gate-level test and a read-only
// end-to-end test are both green against that, which is why this one goes
// through `writeMirror` -- the connector's own method, the statement it really
// builds, against a real database.
//
// # What it asserts, in both directions
//
// A test that only wrote a row could pass while the guard was off entirely, so
// the named connector's success is asserted BESIDE a different connector's
// refusal. The second is the reachable negative: the mirror is still read-only
// to everyone the concept does not name.
func TestTheConnectorWritesItsMirrorEndToEnd(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	const rowId = "dbtest-mirror-e2e"
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:product' AND payload->>'storeId' = $1`,
			authorityTestStoreId)
	})

	write := memqlsync.MirrorWrite{
		Concept: "v1:shopify:product",
		RowId:   rowId,
		Payload: map[string]any{
			"storeId": authorityTestStoreId,
			"gid":     "gid://shopify/Product/5574",
			"title":   "End-to-end probe",
			"deleted": false,
			// Required by the generated concept. The mirror's mapping rule
			// carries a timestamp as an RFC3339 STRING, not a time value.
			"updatedAt": "2026-09-01T00:00:00Z",
			"syncedAt":  "2026-09-01T00:00:00Z",
		},
	}

	// THE NAMED CONNECTOR. v1:shopify:product declares @origin("shopify"), so
	// this is the one actor the mirror admits -- and the write has to actually
	// land, not merely pass a gate.
	c := &Connector{engine: eng}
	if err := c.writeMirror(context.Background(), write); err != nil {
		t.Fatalf("the shopify connector could not write its own mirror: %v\n  %s", err,
			"if this is `no actor found in context` the connector actor has lost a surface "+
				"again -- see ContextWithConnectorActor (memql#2989, memql#5574)")
	}

	// It LANDED. Read back under a cluster owner, because the point of the
	// write is a row somebody else can see.
	res, err := eng.Execute(ownerCtx(), "productsForStore(storeId: \""+authorityTestStoreId+"\")")
	if err != nil {
		// Not every tree ships that read; fall back to the row itself, which
		// is what the assertion is really about.
		var n int
		if qerr := db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:shopify:product' AND payload->>'storeId' = $1`,
			authorityTestStoreId).Scan(&n); qerr != nil {
			t.Fatalf("count the mirrored row: %v", qerr)
		}
		if n == 0 {
			t.Fatal("writeMirror returned no error and wrote no row -- a silent success is the " +
				"failure mode this whole file exists for")
		}
		return
	}
	if got := len(memql.MaterializeRows(res)); got == 0 {
		t.Fatal("writeMirror returned no error and the row is not readable -- a silent success " +
			"is the failure mode this whole file exists for")
	}

	// THE REACHABLE NEGATIVE. A different connector is refused, so the
	// success above is evidence about the DECLARATION rather than about the
	// guard being inert.
	other := auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "quickBooks"))
	stmt, err := mirrorInsert(write)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Execute(other, stmt); err == nil {
		t.Error("a DIFFERENT connector wrote v1:shopify:product -- the mirror is read-only to " +
			"every actor its @origin does not name, and that is what makes a mirror believable")
	} else if !strings.Contains(err.Error(), "shopify") {
		t.Errorf("the refusal does not name where the change has to be made: %v", err)
	}
}
