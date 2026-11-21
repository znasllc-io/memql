package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func portraitRetentionFixture(t *testing.T) (*fixture, func(string, time.Duration, map[string]any)) {
	t.Helper()
	f := newFixture(t)
	f.sweep.Concepts = memorynodes.NewRegistry(map[string]*memorynodes.Concept{
		callsPortraitConcept: {Name: callsPortraitConcept},
	})
	f.sweep.GateDB = func() *sql.DB { return f.db.DB }
	put := func(id string, age time.Duration, fields map[string]any) {
		t.Helper()
		f.concept = &memorynodes.Concept{Name: callsPortraitConcept}
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		f.insert(t, "MemoryNodes", callsPortraitConcept+":"+id, age, string(raw))
	}
	return f, put
}

func portraitRetentionFields(deadline time.Time) map[string]any {
	return map[string]any{
		"siteId": "site", "ownerUserId": "owner", "keyId": "v1:calls:key:key", "digest": strings.Repeat("a", 64),
		"mimeType": "image/png", "width": 16, "height": 16, "issuedAt": deadline.Add(-72 * time.Hour).Format(time.RFC3339Nano),
		"status": "retired", "retirementId": "11111111-1111-4111-8111-111111111111", "retirementReady": true,
		"retiredAt": deadline.Add(-24 * time.Hour).Format(time.RFC3339Nano), "purgeAt": deadline.Format(time.RFC3339Nano),
	}
}

func TestCallPortraitsPurgeMakesBoundedProgressWithoutLosingLatestEarly(t *testing.T) {
	f, put := portraitRetentionFixture(t)
	ctx := context.Background()
	fields := portraitRetentionFields(f.now)
	for n := 0; n < portraitPurgeBatchSize+3; n++ {
		if n > 0 {
			fields["status"] = "active"
			delete(fields, "retirementReady")
			delete(fields, "retirementId")
			delete(fields, "retiredAt")
			delete(fields, "purgeAt")
		}
		put("expired", time.Duration(n+1)*time.Second, fields)
	}
	put("active", time.Second, portraitRetentionFields(f.now.Add(time.Hour)))
	report, err := f.sweep.Run(ctx, true)
	require.NoError(t, err)
	require.Equal(t, 1, report.Eligible)
	require.Zero(t, report.Deleted)
	require.Equal(t, portraitPurgeBatchSize+4, f.count(t, "MemoryNodes"))
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, portraitPurgeBatchSize, report.Deleted)
	versions := f.versionsOf(t, callsPortraitConcept+":expired")
	require.Len(t, versions, 3)
	require.Contains(t, versions, f.now.Add(-time.Second))
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Equal(t, 3, report.Deleted)
	require.Empty(t, f.versionsOf(t, callsPortraitConcept+":expired"))
	require.Len(t, f.versionsOf(t, callsPortraitConcept+":active"), 1)
	report, err = f.sweep.Run(ctx, false)
	require.NoError(t, err)
	require.Zero(t, report.Deleted)
}

func TestCallPortraitsPurgePreservesBatchWhenAnOlderVersionIsDenied(t *testing.T) {
	f, put := portraitRetentionFixture(t)
	fields := portraitRetentionFields(f.now)
	put("denied", time.Hour, fields)
	put("denied", time.Second, fields)
	f.sweep.Admit = func(_ context.Context, row memorynodes.MemoryNode) bool {
		return row.CreatedAt.After(f.now.Add(-time.Minute))
	}
	_, err := f.sweep.Run(context.Background(), false)
	require.ErrorContains(t, err, "admission refused")
	require.Equal(t, 2, f.count(t, "MemoryNodes"))
}

func TestCallPortraitsPurgeRefusesChangedImmutableBindings(t *testing.T) {
	for _, field := range []string{"siteId", "ownerUserId", "accountId", "keyId", "digest", "mimeType", "issuedAt"} {
		t.Run(field, func(t *testing.T) {
			f, put := portraitRetentionFixture(t)
			fields := portraitRetentionFields(f.now)
			put("mismatch", time.Second, fields)
			fields[field] = "different"
			put("mismatch", time.Hour, fields)
			_, err := f.sweep.Run(context.Background(), false)
			require.ErrorContains(t, err, "binding mismatch")
			require.Equal(t, 2, f.count(t, "MemoryNodes"))
		})
	}
}

func TestCallPortraitsPurgeRereadsBindingUnderTheKeyGate(t *testing.T) {
	f, put := portraitRetentionFixture(t)
	fields := portraitRetentionFields(f.now)
	put("gated", time.Hour, fields)
	ctx := context.Background()
	release, err := memql.AcquireWriteGate(ctx, f.db.DB, "calls.key.key")
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
		t.Fatal("purger did not select receipt")
	}
	select {
	case err := <-done:
		release()
		t.Fatalf("purger bypassed key gate: %v", err)
	default:
	}
	// A malformed replacement must not redirect deletion under the wrong room
	// lock. Native writers do not permit this mutation; maintenance still refuses.
	fields["keyId"] = "v1:calls:key:other-key"
	put("gated", 0, fields)
	release()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "binding changed")
	case <-time.After(5 * time.Second):
		t.Fatal("purger did not finish")
	}
	require.Equal(t, 2, f.count(t, "MemoryNodes"))
}

func TestCallPortraitsPurgeTransactionRefusesAChangedSnapshot(t *testing.T) {
	f, put := portraitRetentionFixture(t)
	fields := portraitRetentionFields(f.now)
	put("race", time.Hour, fields)
	put("race", time.Second, fields)
	injected := false
	f.sweep.Admit = func(_ context.Context, row memorynodes.MemoryNode) bool {
		if row.CreatedAt.Equal(f.now.Add(-time.Hour)) && !injected {
			injected = true
			changed := portraitRetentionFields(f.now)
			changed["retirementId"] = "22222222-2222-4222-8222-222222222222"
			put("race", 0, changed)
		}
		return true
	}
	_, err := f.sweep.Run(context.Background(), false)
	require.ErrorContains(t, err, "snapshot changed")
	require.Equal(t, 3, f.count(t, "MemoryNodes"))
}

func TestCallPortraitsPurgeKeepsAnActiveLatestVersion(t *testing.T) {
	f, put := portraitRetentionFixture(t)
	fields := portraitRetentionFields(f.now)
	put("active", time.Hour, fields)
	fields["status"] = "active"
	put("active", time.Second, fields)
	report, err := f.sweep.Run(context.Background(), false)
	require.NoError(t, err)
	require.Zero(t, report.Deleted)
	require.Equal(t, 2, f.count(t, "MemoryNodes"))
}

func TestCallPortraitsScheduledSweepUsesPrivateRowAdmission(t *testing.T) {
	f, put := portraitRetentionFixture(t)
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	concept, err := memorynodes.Get(callsPortraitConcept)
	require.NoError(t, err)
	// The scheduled integration receives the actual private concept metadata. Skip
	// its independent version-pruning pass here to isolate all-version expiry.
	copyConcept := *concept
	copyConcept.Retention = nil
	registry := memorynodes.NewRegistry(map[string]*memorynodes.Concept{concept.Name: &copyConcept})
	put("maintenance", time.Second, portraitRetentionFields(f.now.Add(-time.Second)))
	integration := &Integration{context: memql.PluginContext{Concepts: registry, BunDB: func() *bun.DB { return f.db }, GateDB: func() *sql.DB { return f.db.DB }, AdmitSourceRow: memql.AdmitSourceRow, InvalidateConcept: func(string) {}}}
	maintenance := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "retireDeclaredConceptVersions"))
	nodes, err := integration.sweep(maintenance, map[string]any{}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, sweepReport(t, nodes).Deleted)
	require.Zero(t, f.count(t, "MemoryNodes"))
}

func TestCallPortraitsPurgeRequiresConfirmedFixedRetirementDeadline(t *testing.T) {
	for _, change := range []string{"retirementReady", "retirementId", "retiredAt"} {
		t.Run(change, func(t *testing.T) {
			f, put := portraitRetentionFixture(t)
			fields := portraitRetentionFields(f.now)
			delete(fields, change)
			put("unconfirmed", time.Second, fields)
			_, err := f.sweep.Run(context.Background(), false)
			require.ErrorContains(t, err, "unconfirmed")
			require.Equal(t, 1, f.count(t, "MemoryNodes"))
		})
	}
}
