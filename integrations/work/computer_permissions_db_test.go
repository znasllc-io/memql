package work

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

func TestComputerPermissionDBTwoReplicasConsumeOnceAndRevokeAlways(t *testing.T) {
	a := openActsDB(t)
	runID := a.openRun(t, nil)
	peerDB := workHeadsPeer(t, a.db)
	b := New(dispatchDBEngine(t, peerDB), testLogger(), func() *bun.DB { return peerDB })
	b.SetGateDB(testGates(t))
	person := actorCtx(a.owner)
	row, req := computerPermissionFixture()
	req.OwnerID = a.owner
	req.RunID = runID
	subject := rowMap(row, "subject")
	create := func(mode string) string {
		id := newRowId(approvalConcept)
		a.write(t, "createWorkApproval", map[string]any{"approvalId": id, "runId": runID, "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "requestedAt": rfc(time.Now()), "expiresAt": rfc(time.Now().Add(time.Hour))})
		// Warm the other replica's pending-row cache before the actual decision.
		_, err := b.store().query(person, "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
		if err != nil {
			t.Fatal(err)
		}
		a.write(t, "decideWorkApproval", map[string]any{"approvalId": id, "decision": "approved", "decidedBy": a.owner, "decidedAt": rfc(time.Now()), "answer": map[string]any{"permissionMode": mode}})
		return id
	}
	once := create("once")
	if found, err := b.Find(person, req); err != nil || found == nil {
		t.Fatalf("stale negative read: %v %v", found, err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for n, replica := range []*Integration{a.i, b} {
		wg.Add(1)
		go func(n int, i *Integration) {
			defer wg.Done()
			_, errs[n] = i.Claim(person, once, req, "worker", "machine")
		}(n, replica)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("once must admit exactly one replica: %v", errs)
	}
	rows, err := b.store().query(memql.ContextWithFreshRead(person), "query "+call("workApprovalForOwner", map[string]any{"approvalId": once}))
	if err != nil || len(rows) != 1 || rowString(rows[0], "permissionUsedAt") == "" {
		t.Fatalf("missing durable consumption: %v %v", rows, err)
	}
	always := create("always")
	// An Always decision is not a one-hour grant: only its pending question expires.
	future := time.Now().Add(2 * time.Hour)
	a.i.SetNow(func() time.Time { return future })
	b.SetNow(func() time.Time { return future })
	for _, replica := range []*Integration{a.i, b} {
		if _, err := replica.Claim(person, always, req, "worker", "machine"); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := b.store().query(memql.ContextWithFreshRead(person), "query workComputerPermissions()")
	if err != nil || len(listed) != 1 {
		t.Fatalf("active saved list: %v %v", listed, err)
	}
	for _, tc := range []struct{ registration, machine string }{{"other", "machine"}, {"worker", "other"}} {
		if _, err := b.Claim(person, always, req, tc.registration, tc.machine); err == nil {
			t.Fatal("machine mismatch admitted")
		}
	}
	if _, err := b.handleRevokeComputerPermission(actorCtx("someone-else"), map[string]any{"permissionId": always}, 0); err == nil {
		t.Fatal("another owner revoked permission")
	}
	if _, err := a.i.handleRevokeComputerPermission(person, map[string]any{"permissionId": always}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(person, always, req, "worker", "machine"); err == nil {
		t.Fatal("stale other replica ignored revocation")
	}
	if _, err := b.handleRevokeComputerPermission(person, map[string]any{"permissionId": always}, 0); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	listed, err = b.store().query(memql.ContextWithFreshRead(person), "query workComputerPermissions()")
	if err != nil || len(listed) != 0 {
		t.Fatalf("revoked remains listed: %v %v", listed, err)
	}
	cancelled, cancel := context.WithCancel(person)
	cancel()
	if _, err := b.Claim(cancelled, always, req, "worker", "machine"); err == nil {
		t.Fatal("cancelled request claimed")
	}
}
