package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func reportRetentionFixture(t *testing.T) (*fixture, func(string, string, time.Duration, map[string]any)) {
	t.Helper()
	f := newFixture(t)
	f.sweep.Concepts = memorynodes.NewRegistry(map[string]*memorynodes.Concept{
		callsReportCaseConcept: {Name: callsReportCaseConcept}, callsReportAuditConcept: {Name: callsReportAuditConcept},
	})
	f.sweep.GateDB = func() *sql.DB { return f.db.DB }
	put := func(concept, id string, age time.Duration, fields map[string]any) {
		t.Helper()
		f.concept = &memorynodes.Concept{Name: concept}
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		f.insert(t, "MemoryNodes", concept+":"+id, age, string(raw))
	}
	return f, put
}

func reportRetentionFields(id string, deadline time.Time) map[string]any {
	return map[string]any{"siteId": "site", "ownerUserId": "owner", "caseId": callsReportCaseConcept + ":" + id, "expiresAt": deadline.Format(time.RFC3339Nano), "stateData": "private metadata fixture"}
}

func TestCallReportsPurgeCaseAndAuditVersionsAtConfiguredExpiry(t *testing.T) {
	f, put := reportRetentionFixture(t)
	ctx := context.Background()
	expired, future := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, id := range []string{expired, future} {
		deadline := f.now
		if id == future {
			deadline = deadline.Add(time.Second)
		}
		for _, age := range []time.Duration{time.Hour, time.Minute} {
			put(callsReportCaseConcept, id, age, reportRetentionFields(id, deadline))
			put(callsReportAuditConcept, "audit-"+id, age, reportRetentionFields(id, deadline))
		}
	}
	put("v1:calls:key", "wallet", time.Hour, map[string]any{"balanceCents": 1000})
	report, err := f.sweep.Run(ctx, true)
	require.NoError(t, err)
	require.Equal(t, 1, report.Eligible)
	require.Zero(t, report.Deleted)
	require.Equal(t, 9, f.count(t, "MemoryNodes"))
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 4, report.Deleted)
	require.Empty(t, f.versionsOf(t, callsReportCaseConcept+":"+expired))
	require.Empty(t, f.versionsOf(t, callsReportAuditConcept+":audit-"+expired))
	require.Len(t, f.versionsOf(t, callsReportCaseConcept+":"+future), 2)
	require.Len(t, f.versionsOf(t, "v1:calls:key:wallet"), 1)
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Zero(t, report.Deleted)
	f.now = f.now.Add(time.Second)
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 4, report.Deleted)
	require.Equal(t, 1, f.count(t, "MemoryNodes"))
}

func TestCallReportsPurgePreservesEverythingIfAnyPrivateRowIsDenied(t *testing.T) {
	f, put := reportRetentionFixture(t)
	id := strings.Repeat("c", 64)
	fields := reportRetentionFields(id, f.now)
	put(callsReportCaseConcept, id, time.Hour, fields)
	put(callsReportAuditConcept, "audit", time.Minute, fields)
	f.sweep.Admit = func(_ context.Context, row memorynodes.MemoryNode) bool {
		return row.Concept != callsReportAuditConcept
	}
	_, err := f.sweep.Run(context.Background(), false)
	require.ErrorContains(t, err, "admission refused")
	require.Equal(t, 2, f.count(t, "MemoryNodes"))
}

func TestCallReportsPurgePreservesOversizedInventory(t *testing.T) {
	f, put := reportRetentionFixture(t)
	id := strings.Repeat("e", 64)
	fields := reportRetentionFields(id, f.now)
	put(callsReportCaseConcept, id, time.Hour, fields)
	for n := 0; n < reportPurgeRowLimit; n++ {
		put(callsReportAuditConcept, fmt.Sprintf("audit-%03d", n), time.Minute, fields)
	}
	_, err := f.sweep.Run(context.Background(), false)
	require.ErrorContains(t, err, "exceeds bounded purge")
	require.Equal(t, reportPurgeRowLimit+1, f.count(t, "MemoryNodes"))
}

func TestCallReportsPurgeRefusesMismatchedOwnershipOrExpiry(t *testing.T) {
	for _, field := range []string{"ownerUserId", "siteId", "expiresAt"} {
		t.Run(field, func(t *testing.T) {
			f, put := reportRetentionFixture(t)
			id := strings.Repeat("d", 64)
			fields := reportRetentionFields(id, f.now)
			put(callsReportCaseConcept, id, time.Hour, fields)
			fields[field] = "mismatched"
			put(callsReportAuditConcept, "audit", time.Minute, fields)
			_, err := f.sweep.Run(context.Background(), false)
			require.ErrorContains(t, err, "mismatch")
			require.Equal(t, 2, f.count(t, "MemoryNodes"))
		})
	}
}

func TestCallReportsPurgeUsesReviewGateAndRereadsLatestSnapshot(t *testing.T) {
	f, put := reportRetentionFixture(t)
	ctx := context.Background()
	id := strings.Repeat("e", 64)
	fields := reportRetentionFields(id, f.now)
	put(callsReportCaseConcept, id, time.Hour, fields)
	put(callsReportAuditConcept, "audit", time.Minute, fields)
	release, err := memql.AcquireWriteGate(ctx, f.db.DB, "calls.report."+id)
	require.NoError(t, err)
	selected := make(chan struct{}, 1)
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
		t.Fatal("purger did not select case")
	}
	select {
	case err := <-done:
		release()
		t.Fatalf("purger bypassed native case gate: %v", err)
	default:
	}
	// Simulate replacement arriving under the gate after candidate selection.
	// The API itself does not allow deadline extension; maintenance must still
	// reread and preserve an unexpected newer snapshot instead of deleting it.
	fields["expiresAt"] = f.now.Add(time.Hour).Format(time.RFC3339Nano)
	put(callsReportCaseConcept, id, 0, fields)
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("purger did not finish")
	}
	require.Equal(t, 3, f.count(t, "MemoryNodes"))
}

func TestCallReportsPurgeTransactionRefusesAChangedSnapshot(t *testing.T) {
	f, put := reportRetentionFixture(t)
	id := strings.Repeat("f", 64)
	fields := reportRetentionFields(id, f.now)
	put(callsReportCaseConcept, id, time.Hour, fields)
	put(callsReportAuditConcept, "audit", time.Minute, fields)
	injected := false
	f.sweep.Admit = func(_ context.Context, row memorynodes.MemoryNode) bool {
		if row.Concept == callsReportAuditConcept && !injected {
			injected = true
			changed := reportRetentionFields(id, f.now)
			changed["stateData"] = "newer private metadata"
			put(callsReportCaseConcept, id, 0, changed)
		}
		return true
	}
	_, err := f.sweep.Run(context.Background(), false)
	require.ErrorContains(t, err, "snapshot changed")
	require.Equal(t, 3, f.count(t, "MemoryNodes"))
}

func TestCallReportsScheduledSweepUsesPrivateRowAdmission(t *testing.T) {
	f, put := reportRetentionFixture(t)
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	caseConcept, err := memorynodes.Get(callsReportCaseConcept)
	require.NoError(t, err)
	auditConcept, err := memorynodes.Get(callsReportAuditConcept)
	require.NoError(t, err)
	registry := memorynodes.NewRegistry(map[string]*memorynodes.Concept{caseConcept.Name: caseConcept, auditConcept.Name: auditConcept})
	id := strings.Repeat("1", 64)
	fields := reportRetentionFields(id, f.now.Add(-time.Second))
	put(callsReportCaseConcept, id, time.Hour, fields)
	put(callsReportAuditConcept, "audit", time.Minute, fields)
	integration := &Integration{context: memql.PluginContext{Concepts: registry, BunDB: func() *bun.DB { return f.db }, GateDB: func() *sql.DB { return f.db.DB }, AdmitSourceRow: memql.AdmitSourceRow, InvalidateConcept: func(string) {}}}
	maintenance := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "retireDeclaredConceptVersions"))
	nodes, err := integration.sweep(maintenance, map[string]any{}, 0)
	require.NoError(t, err)
	require.Equal(t, 2, sweepReport(t, nodes).Deleted)
	require.Zero(t, f.count(t, "MemoryNodes"))
}
