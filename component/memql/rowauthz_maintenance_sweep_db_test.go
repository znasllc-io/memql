package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE PROOF memql#4406 asked for: the retention sweep still retires a row after
// v1:worker:invocation's tier landed.
//
// "A green suite with a dead sweep is the failure mode here" -- the issue's own
// words, and the reason this file exists rather than a note saying the sweep was
// reviewed. Declaring an owned tier over a concept a system-actor sweep reads
// makes the sweep read ZERO ROWS: not an error, not a log line, just a table
// that quietly stops being pruned while WORKER_INVOCATION_RETENTION_DAYS goes on
// looking like a setting. Every gate in this package stays green through that.
//
// So the test is built around a NEGATIVE CONTROL rather than a positive one. It
// is easy to write a test where the sweep's read returns the row and learn
// nothing -- an unenforced tier returns it too. What makes the pass meaningful
// is that the SAME read, under the ordinary automation principal, returns
// nothing: the tier is doing work, and the maintenance principal is what gets
// past it.
//
// Postgres-gated like its neighbours; CI's db-tests lane runs this package with
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

const invocationConcept = "v1:worker:invocation"

// The RoleReader principal below is the one component/automations'
// contextWithSystemActor builds for an automation that is NOT on the maintenance
// list. It is rebuilt here rather than imported because component/automations
// imports THIS package, so the dependency cannot run the other way.
//
// That is safe for what it is used for: it is the NEGATIVE control, and its only
// claim is "a RoleReader actor is refused". The tie back to the real thing is
// asserted separately, by requiring MaintenanceActor to answer nil for an
// unlisted name -- i.e. that the LIST is what confers the elevation, not the
// shape of the context.

// ownerFieldString renders a stored owner field for an error message. The write
// path canonicalises an @relationship owner to `v1:identity:user:<id>`, so the
// assertion is on containment rather than equality -- sameRowAuthzOwner's own
// reason for existing.
func ownerFieldString(v any) string {
	s, _ := v.(string)
	return s
}

// queryInvocationIds runs a read and returns the set of row ids it produced.
func queryInvocationIds(t *testing.T, ctx context.Context, eng *MemQLEngine, q string) map[string]bool {
	t.Helper()
	res, err := eng.Execute(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	out := map[string]bool{}
	if res == nil || res.Bundle == nil {
		return out
	}
	for _, n := range res.Bundle.Nodes {
		if n == nil {
			continue
		}
		out[n.Id] = true
	}
	return out
}

func TestWorkerInvocationRetentionSweepStillRetiresARowUnderTheTier(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	// ---- POSITIVE CONTROL, first, because #4366 records exactly this trap:
	// "a first probe reported 'survives' because the concept registry had not
	// been loaded, so nothing was injected". A test that measures enforcement
	// over an unloaded registry measures nothing and reports success.
	concept, err := memoryNodes.Get(invocationConcept)
	if err != nil || concept == nil {
		t.Fatalf("%s did not resolve in the loaded registry (%v); nothing below would be enforced", invocationConcept, err)
	}
	decl := concept.RowAuthz
	if decl == nil {
		t.Fatalf("%s declares no row-authz tier, so this test would pass over an unenforced concept", invocationConcept)
	}
	if decl.Tier != langparser.RowAuthzOwned || strings.TrimSpace(decl.Owner) != "ownerUserId" || !decl.ClusterOwnerBypass {
		t.Fatalf("%s declares tier=%q owner=%q clusterOwner=%v; this test is written against the composite owner tier",
			invocationConcept, decl.Tier, decl.Owner, decl.ClusterOwnerBypass)
	}

	suffix := uniqueSuffix("sweep4406")
	owner := "worker-owner-" + suffix
	invocationId := "inv-" + suffix

	ownerCtx := rowAuthzCallerCtx(owner)

	// ---- The write. ownerUserId is NOT an argument: the concept marks it
	// @serverSet and the mutation stamps actor.userId, which is the half of
	// memql#4406 that TestDeclaredOwnerFieldsAreServerStamped enforces.
	storedId := runMutation(t, ownerCtx, eng, "createWorkerInvocation", map[string]any{
		"invocationId": invocationId,
		"workerId":     "wkr-" + suffix,
		"agentId":      "agt-" + suffix,
		"tool":         "workerHost",
		"action":       "exec",
		"startedAt":    "2026-01-01T00:00:00.000Z",
		"outcome":      "success",
	})
	stamped := latestPayload(t, ownerCtx, db, invocationConcept, storedId)
	if got := ownerFieldString(stamped["ownerUserId"]); !strings.Contains(got, owner) {
		t.Fatalf("ownerUserId stamped as %q, want the actor %q -- the row is owned by nobody the sweep can be scoped against", got, owner)
	}

	// ---- The maintenance principal, taken from the REAL list rather than
	// constructed here, so this test fails if the entry is ever dropped.
	maint := auth.MaintenanceActor("workerInvocationRetentionSweep")
	if maint == nil {
		t.Fatal("workerInvocationRetentionSweep is not on the maintenance list, so the retention sweep " +
			"runs as RoleReader over an owned-tier concept and retires nothing (component/auth/maintenance_actor.go)")
	}
	if !maint.IsClusterOwner() {
		t.Fatalf("the maintenance principal is %s/%s and IsClusterOwner()=false; the composite tier's only escape is RoleOwner",
			maint.UserId, maint.Role)
	}
	maintCtx := auth.ContextWithToken(
		auth.ContextWithAccess(t.Context(), maint),
		&auth.TokenInfo{Subject: maint.UserId})

	// ---- THE CLAIM: the sweep's read still sees the row.
	rows := queryInvocationIds(t, maintCtx, eng, `query expiredWorkerInvocations()`)
	if !rows[storedId] {
		t.Fatalf("expiredWorkerInvocations returned %d row(s) and none was %s. The sweep cannot retire what "+
			"it cannot read, and this is the silent failure memql#4406 refused to ship: no error, no log line, "+
			"and WORKER_INVOCATION_RETENTION_DAYS quietly stops meaning anything.", len(rows), storedId)
	}

	// ---- THE NEGATIVE CONTROL, without which the assertion above is also
	// satisfied by a tier that is not enforced at all.
	if auth.MaintenanceActor("someAutomationNobodyListed") != nil {
		t.Fatal("MaintenanceActor answered for an unlisted automation; the elevation must come from the list")
	}
	readerCtx := auth.ContextWithToken(
		auth.ContextWithAccess(t.Context(), &auth.AccessContext{
			UserId: "system:automation:workerInvocationRetentionSweep",
			Role:   auth.RoleReader,
		}),
		&auth.TokenInfo{Subject: "system:automation:workerInvocationRetentionSweep"})
	if got := queryInvocationIds(t, readerCtx, eng, `query expiredWorkerInvocations()`); got[storedId] {
		t.Fatal("the ordinary automation principal (RoleReader) read the row too, so the tier is not being " +
			"enforced and the pass above says nothing about the maintenance principal")
	}

	// ---- The sweep's WRITE, which the write guard resolves against the same
	// admission function. A read the sweep can perform and a write it cannot is
	// still a sweep that retires nothing.
	if _, err := eng.Execute(maintCtx, `mutation softDeleteWorkerInvocation(invocationId: "`+invocationId+`")`); err != nil {
		t.Fatalf("softDeleteWorkerInvocation refused the maintenance principal: %v", err)
	}
	after := latestPayload(t, ownerCtx, db, invocationConcept, storedId)
	if after["deleted"] != true {
		t.Fatalf("deleted=%v after the sweep's soft-delete, want true", after["deleted"])
	}

	// And the ordinary principal cannot perform that write, for the same reason
	// it cannot perform the read.
	if _, err := eng.Execute(readerCtx, `mutation softDeleteWorkerInvocation(invocationId: "`+invocationId+`")`); err == nil {
		t.Fatal("the ordinary automation principal soft-deleted another owner's invocation row; the write " +
			"guard is not consulting the tier")
	}
}
