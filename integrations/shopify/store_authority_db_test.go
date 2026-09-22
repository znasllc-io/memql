package shopify

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// store_authority_db_test.go -- the connector's own configuration reads,
// against a real engine over a real Postgres (memql#5574).
//
// # Why a fake engine cannot hold this
//
// The bug is two independent gates saying no, and NEITHER is reachable
// through `fakeEngine`: its `Execute` records the MemQL text and answers from
// a row table (harness_test.go), so the hand-written `actor.isClusterOwner`
// conjunct is never lowered and `rowAuthzAdmitsMode` is never called. Every
// test in this package passed while the connector read zero stores in
// production -- which is the green the issue's "Why CI is green" section
// describes, and the reason this file boots the real thing.
//
// # The three readings, and why all three are asserted
//
// A test that reads zero rows looks identical whether the gate refused or the
// seed was never written, so the refusal is only evidence beside a REACHABLE
// POSITIVE:
//
//	a cluster owner        -> the row      the seed IS readable
//	a connector actor      -> nothing      the refusal, and it STAYS after the
//	                                       fix: this fix does not widen a
//	                                       connector's reach, it stops using a
//	                                       connector's identity for MemQL's own
//	                                       configuration
//	the StoreRegistry       -> the row      what the connector actually does
//
// The middle one is the negative control. Without it a green run cannot tell
// "the fix is targeted" from "the connector now reads everything".

const (
	// The BARE id, which is what a Store carries and what every call passes:
	// storeFromRow runs the read's canonical id through shortID, and the
	// engine resolves a bare arg on the way back in.
	authorityTestStoreId = "dbtest-authority"
	authorityTestDomain  = "dbtest-authority.myshopify.com"
)

// authorityEngine boots a real engine over the embedded DSL tree.
func authorityEngine(t *testing.T) (*memql.MemQLEngine, *sql.DB) {
	t.Helper()
	dsn := dbtest.DSN()
	raw := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = raw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "shopify connector authority", dsn, err)
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

// seedStore writes one v1:shopify:store row THE WAY PRODUCTION DOES: through
// the engine's own createStore mutation, under a real cluster owner.
//
// A direct INSERT was the first attempt and it was the wrong fixture. The row
// it produced was readable by `stores()` and invisible to `recordStoreHealth`'s
// existence check, so the write case failed with "no existing row" -- a fixture
// artefact that reads exactly like the bug under test. Seeding through the
// mutation removes the question: the row is the shape the engine writes.
func seedStore(t *testing.T, eng *memql.MemQLEngine, db *sql.DB) {
	t.Helper()
	call := renderCall("createStore", map[string]any{
		"storeId":       authorityTestStoreId,
		"domain":        authorityTestDomain,
		"name":          "Authority Probe",
		"adminTokenRef": "shopify-admin-" + authorityTestStoreId,
		"apiVersion":    "2026-07",
	})
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("seed a store through createStore: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND payload->>'domain' = $1`,
			authorityTestDomain)
	})
}

// seedComplianceJob queues one privacy job that is already due, so
// complianceJobsDue has something to find. Its absence is what made the first
// run of this file report a refusal where there was simply nothing to read.
func seedComplianceJob(t *testing.T, eng *memql.MemQLEngine, db *sql.DB) {
	t.Helper()
	jobId := "dbtest-authority-job"
	call := renderCall("queueComplianceJob", map[string]any{
		"jobId": jobId, "storeId": authorityTestStoreId,
		"topic": "customers/data_request", "customerGid": "gid://shopify/Customer/1",
		"requestId": "1", "shopDomain": authorityTestDomain,
		"dueAt": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	})
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("seed a compliance job: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:complianceJob' AND id LIKE $1`,
			"%"+jobId)
	})
}

// seedSyncState writes one v1:platform:syncState row for this connector. The
// deployment's own sync-health row: written by the sync runtime, read back by
// the connector's capability surface.
func seedSyncState(t *testing.T, eng *memql.MemQLEngine, db *sql.DB) {
	t.Helper()
	stateId := "dbtest-authority-syncstate"
	call := renderCall("upsertSyncState", map[string]any{
		"stateId": stateId, "conceptId": "v1:shopify:product",
		"connector": ConnectorName, "direction": "inbound",
		"backfillStatus": "none",
	})
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("seed a sync state: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND id LIKE $1`,
			"%"+stateId)
	})
}

// seedCtx is what the FIXTURES are written under: a cluster owner plus
// internal origin, because several of these mutations are @serverOnly and a
// @serverOnly construct is refused to a client whatever actor it carries.
//
// Kept separate from ownerCtx on purpose. ownerCtx is the REACHABLE POSITIVE's
// identity and must stay a plain client-side cluster owner -- if the positive
// were also stamped internal-origin it would stop standing in for the operator
// reading the same rows from a browser, which is the reading the assertion is
// claiming to be equivalent to.
func seedCtx() context.Context {
	return auth.ContextWithInternalOrigin(ownerCtx())
}

// ownerCtx is a real cluster owner: the reachable positive's identity.
func ownerCtx() context.Context {
	claims := map[string]any{"sub": "dbtest-owner", "role": "owner"}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "dbtest-owner", Role: auth.RoleOwner})
}

func TestConnectorReadsItsOwnStoreList(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	seedStore(t, eng, db)

	// (1) REACHABLE POSITIVE. Without this the two readings below are
	// unfalsifiable: zero rows would mean the same thing for a refused read
	// and for a seed that never landed.
	res, err := eng.Execute(ownerCtx(), "stores()")
	if err != nil {
		t.Fatalf("stores() as a cluster owner: %v", err)
	}
	if got := len(memql.MaterializeRows(res)); got == 0 {
		t.Fatalf("stores() as a cluster owner returned 0 rows -- the seed is not readable by ANYONE, " +
			"so nothing below measures a gate")
	}

	// (2) NEGATIVE CONTROL. A bare connector actor is refused, before and
	// after the fix: v1:shopify:store is @origin(\"memql\") with no
	// @mirroredTo, so its declaration names no connector, and the fix must
	// not change that.
	res, err = eng.Execute(connectorContext(context.Background()), "stores()")
	if err == nil {
		if got := len(memql.MaterializeRows(res)); got != 0 {
			t.Errorf("a bare connector actor read %d store rows; want 0 -- "+
				"v1:shopify:store names no connector, and widening that is not this fix", got)
		}
	}

	// (3) WHAT THE CONNECTOR ACTUALLY DOES. This is the assertion the bug
	// fails: StoreRegistry.refresh is the ONE place a store row is loaded.
	reg := NewStoreRegistry(eng, func(context.Context, string) (string, error) { return "", nil })
	stores, err := reg.Stores(context.Background())
	if err != nil {
		t.Fatalf("StoreRegistry.Stores: %v", err)
	}
	if len(stores) == 0 {
		t.Fatal("StoreRegistry.Stores() returned no stores while a cluster owner reads one " +
			"(memql#5574): refresh runs under a connector actor, which fails both the query's own " +
			"actor.isClusterOwner conjunct and the connector row gate")
	}
	found := false
	for _, s := range stores {
		if s.Domain == authorityTestDomain {
			found = true
		}
	}
	if !found {
		t.Errorf("StoreRegistry.Stores() = %+v; want the seeded dbtest-authority store", stores)
	}
}

// TestTheConnectorsOwnConceptsAreReadableAndWritable covers the REST of the
// blast radius (memql#5574). The issue named two reads; the measurement found
// eight call sites over four concepts, and a fix verified on one of them is a
// fix for one of them.
//
// Each case is driven under BOTH identities and both answers are asserted. The
// operator identity must reach the row; what a BARE CONNECTOR gets is stated
// per case, because it is not uniform and the difference is the tree's own
// design rather than an accident:
//
//   - a READ is REFUSED. Row admission answers the connector branch from the
//     concept's own declaration, and none of these concepts names a connector.
//     This is the half memql#5574 filed, and it is the half that was broken.
//   - a WRITE is NOT refused, and this is the one a reader gets wrong. The
//     row-authz write guard consults its ESCAPES first, and escape #1 is
//     INTERNAL ORIGIN -- which `connectorContext` has always stamped, for the
//     @serverOnly axis. So the escape returns before row admission is asked,
//     and a connector's write to a concept that does not name it is admitted.
//
// Which means the writes were never broken by row authz. They were broken by
// something else this change also fixes -- the connector actor carried no
// TokenInfo, so `createdBy` could not resolve and every mutation answered "no
// actor found in context" (see mirror_write_end_to_end_db_test.go). Moving
// them to the operator identity is therefore about ATTRIBUTION, not admission:
// a row the deployment wrote should say the deployment wrote it, and a write
// admitted by a blanket "the engine is doing this" escape is not the same as
// one admitted because the identity is right.
//
// Stating that here rather than asserting a refusal is the point. A test that
// demanded these writes be refused would be pinning a bug that never existed,
// and it would fail the moment somebody read the guard correctly.
func TestTheConnectorsOwnConceptsAreReadableAndWritable(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	seedStore(t, eng, db)
	seedComplianceJob(t, eng, db)
	seedSyncState(t, eng, db)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	bare := authorityTestStoreId

	// What a bare connector actor gets, and why.
	const (
		// refused: row admission's connector branch denies, because the
		// concept's declaration names no connector.
		refused = "refused"
		// internalOriginEscape: admitted, and NOT by row authz. The write
		// guard's first escape is internal origin, which connectorContext
		// stamps, so the escape answers before admission is consulted.
		internalOriginEscape = "internal-origin escape"
	)

	cases := []struct {
		what    string
		concept string
		call    string
		// write is true for a mutation: an empty result is SUCCESS there, so
		// the assertion is on the error rather than on a row count.
		write bool
		// connector is what a BARE connector actor gets for this exact call.
		connector string
	}{
		{
			what:      "the store list -- StoreRegistry.refresh, the one place a store row is loaded",
			concept:   "v1:shopify:store",
			call:      "stores()",
			connector: refused,
		},
		{
			what:      "the privacy queue -- what the hourly compliance runner reads",
			concept:   "v1:shopify:complianceJob",
			call:      renderCall("complianceJobsDue", map[string]any{"asOf": now}),
			connector: refused,
		},
		{
			what:      "the deployment's sync health -- what the capability surface reports on",
			concept:   "v1:platform:syncState",
			call:      renderCall("syncStatesAll", map[string]any{"connector": ConnectorName}),
			connector: refused,
		},
		{
			what:    "the store's health stamp -- written after every subscription reconcile",
			concept: "v1:shopify:store",
			call: renderCall("recordStoreHealth", map[string]any{
				"storeId": bare,
				"health":  map[string]any{"lastProbe": now},
			}),
			write:     true,
			connector: internalOriginEscape,
		},
		{
			what:    "the compliance audit line -- the trail IS the deliverable",
			concept: "v1:identity:auditEvent",
			call: renderCall("createAuditEvent", map[string]any{
				"eventId": "dbtest-authority-audit", "occurredAt": now,
				"category": "data", "action": "shopify_compliance_probe",
				"actorUserId": "system:connector:" + ConnectorName,
				"targetType":  "shopifyStore", "targetId": bare,
				"detail": map[string]any{"probe": true}, "outcome": "success",
			}),
			write:     true,
			connector: internalOriginEscape,
		},
	}

	for _, c := range cases {
		t.Run(c.concept+" -- "+c.what, func(t *testing.T) {
			// THE OPERATOR IDENTITY: what the package uses after the fix.
			res, err := eng.Execute(operatorContext(ctx), c.call)
			if err != nil {
				t.Fatalf("under operatorContext: %v\n  call: %s\n  %s", err, c.call,
					"the deployment cannot reach its own row -- memql#5574")
			}
			if !c.write {
				if got := len(memql.MaterializeRows(res)); got == 0 {
					t.Fatalf("under operatorContext the read returned 0 rows\n  call: %s\n  %s",
						c.call, "an empty result here is the memql#5574 symptom: a refusal that "+
							"reads as 'there is nothing configured'")
				}
			}

			// THE NEGATIVE CONTROL. Asserted per concept rather than once,
			// because the connector branch answers per concept from that
			// concept's own declaration -- and because a create answers
			// differently from a read or an update.
			res, err = eng.Execute(connectorContext(ctx), c.call)
			switch c.connector {
			case internalOriginEscape:
				if err != nil {
					t.Errorf("a bare connector actor was refused %s: %v\n  %s", c.concept, err,
						"the write guard's internal-origin escape admits this, and "+
							"connectorContext stamps internal origin -- if it now refuses, either "+
							"the escape set changed or the actor lost a surface again; both are "+
							"things to read, not to relax")
				}
			case refused:
				if err == nil {
					if got := len(memql.MaterializeRows(res)); got != 0 {
						t.Errorf("a bare connector actor read %d rows of %s; want 0 -- %s",
							got, c.concept, "this concept names no connector, and widening "+
								"that is not this fix")
					}
				}
			}
		})
	}

	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, bare)
	})
}
