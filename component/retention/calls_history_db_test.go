package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func TestCallHistoryPurgeDeletesEveryVersionOnlyAfterConfirmedDeadline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	history := &memorynodes.Concept{Name: callsHistoryConcept}
	receipt := &memorynodes.Concept{Name: callsHistoryReceiptConcept}
	f.sweep.Concepts = memorynodes.NewRegistry(map[string]*memorynodes.Concept{history.Name: history, receipt.Name: receipt})
	f.sweep.GateDB = func() *sql.DB { return f.db.DB }
	put := func(concept, id string, age time.Duration, fields map[string]any) {
		t.Helper()
		f.concept = &memorynodes.Concept{Name: concept}
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		f.insert(t, "MemoryNodes", concept+":"+id, age, string(raw))
	}
	payload := func(deadline time.Time) map[string]any {
		return map[string]any{"siteId": "site", "ownerUserId": "owner", "keyId": "key", "title": "Private call title", "sourceId": "room", "archivedAt": f.now.Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano), "purgeAt": deadline.Format(time.RFC3339Nano)}
	}
	proof := func(deadline time.Time) map[string]any {
		return map[string]any{"siteId": "site", "ownerUserId": "owner", "purgeAt": deadline.Format(time.RFC3339Nano)}
	}
	for _, id := range []string{"expired", "restored", "future", "missing-receipt"} {
		put(callsHistoryConcept, id, 40*24*time.Hour, map[string]any{"title": "Original private title", "siteId": "site", "ownerUserId": "owner"})
	}
	put(callsHistoryConcept, "expired", 24*time.Hour, payload(f.now))
	put(callsHistoryReceiptConcept, "expired", 24*time.Hour, proof(f.now))
	restored := payload(f.now)
	restored["archivedAt"] = ""
	restored["purgeAt"] = ""
	put(callsHistoryConcept, "restored", time.Hour, restored)
	put(callsHistoryReceiptConcept, "restored", 24*time.Hour, proof(f.now))
	put(callsHistoryConcept, "future", time.Hour, payload(f.now.Add(time.Second)))
	put(callsHistoryReceiptConcept, "future", time.Hour, proof(f.now.Add(time.Second)))
	put(callsHistoryConcept, "missing-receipt", time.Hour, payload(f.now))
	put("v1:calls:room", "room", 40*24*time.Hour, map[string]any{"title": "Operational retained title", "status": "ended"})
	count := func(concept, id string) int {
		t.Helper()
		var n int
		require.NoError(t, f.db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept=? AND id=?`, concept, concept+":"+id).Scan(&n))
		return n
	}
	report, err := f.sweep.Run(ctx, true)
	require.NoError(t, err)
	require.Equal(t, 1, report.Eligible, "dry-run eligibility also requires the matching deletion proof")
	require.Zero(t, report.Deleted)
	require.Equal(t, 2, count(callsHistoryConcept, "expired"))
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 2, report.Deleted)
	require.Zero(t, count(callsHistoryConcept, "expired"))
	require.Equal(t, 1, count(callsHistoryReceiptConcept, "expired"))
	require.Equal(t, 2, count(callsHistoryConcept, "restored"))
	require.Equal(t, 2, count(callsHistoryConcept, "future"))
	require.Equal(t, 2, count(callsHistoryConcept, "missing-receipt"))
	require.Equal(t, 1, count("v1:calls:room", "room"))
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Zero(t, report.Deleted)
	f.now = f.now.Add(time.Second)
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 2, report.Deleted)
	require.Zero(t, count(callsHistoryConcept, "future"))
}

func TestCallHistoryPurgeRefusesARecordOutsideAdmission(t *testing.T) {
	f := newFixture(t)
	f.concept = &memorynodes.Concept{Name: callsHistoryConcept}
	f.sweep.Concepts = memorynodes.NewRegistry(map[string]*memorynodes.Concept{callsHistoryConcept: f.concept})
	f.sweep.GateDB = func() *sql.DB { return f.db.DB }
	f.sweep.Admit = func(context.Context, memorynodes.MemoryNode) bool { return false }
	raw, _ := json.Marshal(map[string]any{"archivedAt": f.now.Add(-time.Hour).Format(time.RFC3339Nano), "purgeAt": f.now.Format(time.RFC3339Nano)})
	f.insert(t, "MemoryNodes", callsHistoryConcept+":private", time.Hour, string(raw))
	_, err := f.sweep.Run(context.Background(), false)
	require.ErrorContains(t, err, "admission refused")
	require.Equal(t, 1, f.count(t, "MemoryNodes"))
}

func TestScheduledCallHistoryPurgeUsesRealInternalAdmissionAndRestoreGate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	concept, err := memorynodes.Get(callsHistoryConcept)
	require.NoError(t, err)
	proofConcept, err := memorynodes.Get(callsHistoryReceiptConcept)
	require.NoError(t, err)
	registry := memorynodes.NewRegistry(map[string]*memorynodes.Concept{concept.Name: concept, proofConcept.Name: proofConcept})
	id := "shared-gate-history"
	deadline := f.now.Add(-time.Minute).Format(time.RFC3339Nano)
	payload := map[string]any{"siteId": "site", "ownerUserId": "owner", "archivedAt": f.now.Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano), "purgeAt": deadline}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	f.concept = concept
	f.insert(t, "MemoryNodes", callsHistoryConcept+":"+id, time.Hour, string(raw))
	proof, _ := json.Marshal(map[string]any{"siteId": "site", "ownerUserId": "owner", "purgeAt": deadline})
	f.concept = proofConcept
	f.insert(t, "MemoryNodes", callsHistoryReceiptConcept+":"+id, time.Hour, string(proof))
	integration := &Integration{context: memql.PluginContext{Concepts: registry, BunDB: func() *bun.DB { return f.db }, GateDB: func() *sql.DB { return f.db.DB }, AdmitSourceRow: memql.AdmitSourceRow, InvalidateConcept: func(string) {}}}
	maintenance := auth.ContextWithAccess(ctx, auth.MaintenanceActor(auth.AutomationOriginEngine, "retireDeclaredConceptVersions"))
	nodes, err := integration.sweep(maintenance, map[string]any{}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, sweepReport(t, nodes).Deleted, "the real unreadable-concept gate must admit scheduled maintenance")
	// A restore holds the exact gate the physical purger takes. The purger may
	// have selected the older archived snapshot, but must re-read after acquiring.
	f.concept = concept
	id = "restored-under-gate"
	f.insert(t, "MemoryNodes", callsHistoryConcept+":"+id, time.Hour, string(raw))
	f.concept = proofConcept
	f.insert(t, "MemoryNodes", callsHistoryReceiptConcept+":"+id, time.Hour, string(proof))
	release, err := memql.AcquireWriteGate(ctx, f.db.DB, "calls.history."+id)
	require.NoError(t, err)
	selected := make(chan struct{}, 1)
	f.sweep.Concepts = registry
	f.sweep.GateDB = func() *sql.DB { return f.db.DB }
	f.sweep.Admit = func(context.Context, memorynodes.MemoryNode) bool {
		select {
		case selected <- struct{}{}:
		default:
		}
		return true
	}
	done := make(chan error, 1)
	go func() { _, err := f.sweep.Run(ctx, false); done <- err }()
	select {
	case <-selected:
	case <-time.After(3 * time.Second):
		release()
		t.Fatal("purger did not select the candidate")
	}
	payload["archivedAt"] = ""
	payload["purgeAt"] = ""
	restored, _ := json.Marshal(payload)
	f.concept = concept
	f.insert(t, "MemoryNodes", callsHistoryConcept+":"+id, 0, string(restored))
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("purger did not release the entry gate")
	}
	require.Len(t, f.versionsOf(t, callsHistoryConcept+":"+id), 2, "restore protected all metadata versions from physical deletion")
}
