package work

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// Use real PostgreSQL and the production Bun/pgdriver stack. Session-local
// tables isolate sweep/retention tests from other suites and developer data.
// A single connection keeps every query on the session owning these tables.
func sweepDB(t *testing.T) (*bun.DB, *Integration) {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "work sweep SQL", dbtest.DSN(), err)
	}
	for _, q := range []string{
		`CREATE TEMP TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`CREATE TEMP TABLE node_vectors (LIKE public.node_vectors INCLUDING ALL)`,
	} {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	i := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	i.bunDB = func() *bun.DB { return db }
	i.admitRow = func(context.Context, memorynodes.MemoryNode) bool { return true }
	return db, i
}

func insertSweepRow(t *testing.T, db *bun.DB, id, concept string, at time.Time, fields map[string]any) {
	t.Helper()
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload) VALUES (?, ?, ?, 'sweep-test', 'object', '{}', ?::jsonb)`, id, concept, at, string(payload))
	if err != nil {
		t.Fatal(err)
	}
}

func TestSweepDB_ReadsBindParametersAndCollapseBeforeFiltering(t *testing.T) {
	db, i := sweepDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	insertSweepRow(t, db, "v1:work:run:finished", runConcept, now.Add(-3*time.Hour), map[string]any{"status": "running"})
	insertSweepRow(t, db, "v1:work:run:finished", runConcept, now, map[string]any{"status": "succeeded"})
	insertSweepRow(t, db, "v1:work:run:waiting", runConcept, now.Add(-time.Hour), map[string]any{"status": "waiting"})
	insertSweepRow(t, db, "v1:work:run:running", runConcept, now, map[string]any{"status": "running"})
	insertSweepRow(t, db, "v1:work:run:denied", runConcept, now.Add(-time.Hour), map[string]any{"status": "waiting", "allow": true})
	insertSweepRow(t, db, "v1:work:run:denied", runConcept, now, map[string]any{"status": "waiting", "allow": false})
	insertSweepRow(t, db, "v1:work:observation:other", observationConcept, now, map[string]any{"status": "running"})
	i.admitRow = func(_ context.Context, n memorynodes.MemoryNode) bool {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		return p["allow"] != false
	}
	rows, err := i.runsInFlight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["id"] != "v1:work:run:waiting" || rows[1]["id"] != "v1:work:run:running" {
		t.Fatalf("in-flight latest/admitted rows: %v", rows)
	}
	rows, err = i.selectAdmitted(ctx, runConcept, runsInFlightSQL, runConcept, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["id"] != "v1:work:run:waiting" {
		t.Fatalf("LIMIT parameter/order: %v", rows)
	}
	for _, concept := range []string{modelCallConcept, observationConcept} {
		old := concept + ":old'quoted"
		refreshed := concept + ":refreshed"
		insertSweepRow(t, db, old, concept, now.Add(-3*time.Hour), nil)
		insertSweepRow(t, db, refreshed, concept, now.Add(-3*time.Hour), nil)
		insertSweepRow(t, db, refreshed, concept, now, nil)
		insertSweepRow(t, db, concept+":boundary", concept, now.Add(-2*time.Hour), nil)
		rows, err = i.expiredJournalRows(ctx, concept, now.Add(-2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0]["id"] != old {
			t.Fatalf("retention boundary/latest/concept for %s: %v", concept, rows)
		}
	}
}

func TestSweepDB_ClosesOnlyMissingSystemApprovalWaits(t *testing.T) {
	db, i := sweepDB(t)
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	eng, err := memqlengine.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	i.engine, i.admitRow = eng, memqlengine.AdmitSourceRow
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	actor := auth.MaintenanceActor("sweepWaitingWorkRuns")
	ctx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), actor), &auth.TokenInfo{Subject: actor.UserId})
	for _, tc := range []struct {
		id          string
		owner       string
		goal        string
		hasApproval bool
		trigger     string
	}{
		{"orphan", "", "", false, "schedule"},
		{"real-approval", "", "", true, "schedule"},
		{"user-work", "v1:identity:user:alice", "v1:work:goal:alice", false, "schedule"},
		{"event-work", "", "", false, "event:created"},
	} {
		approvalID := "v1:work:approval:" + tc.id
		seed := map[string]any{"runId": runConcept + ":" + tc.id, "goalId": tc.goal, "triggeredBy": tc.trigger, "status": "waiting", "automationName": "sweepWaitingWorkRuns", "templateFingerprint": "test", "startedAt": rfc(now.Add(-time.Hour))}
		st := i.store()
		writeCtx := ownerActor(ctx, tc.owner)
		if err := st.writeInternal(writeCtx, "mutation "+call("createWorkRun", seed)); err != nil {
			t.Fatal(err)
		}
		if err := st.updateRun(writeCtx, runConcept+":"+tc.id, map[string]any{"waitingOn": map[string]any{"kind": "approval", "subject": approvalID, "since": rfc(now.Add(-time.Hour))}}); err != nil {
			t.Fatal(err)
		}
		if tc.hasApproval {
			if err := st.writeInternal(ctx, "mutation "+call("createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runConcept + ":" + tc.id, "kind": "feedback", "artifactHash": "test", "requestedAt": rfc(now.Add(-time.Hour))})); err != nil {
				t.Fatal(err)
			}
		}
	}
	var logOutput bytes.Buffer
	i.logger = slog.New(slog.NewTextHandler(&logOutput, nil))
	res, err := i.SweepWaiting(ctx, time.Minute)
	if err != nil || res.OrphanedWaitsClosed != 1 {
		t.Fatalf("orphan recovery=%+v: %v; %s", res, err, logOutput.String())
	}
	for _, id := range []string{"orphan", "real-approval", "user-work", "event-work"} {
		var status, code string
		if err := db.QueryRowContext(ctx, `SELECT payload->>'status',COALESCE(payload->>'errorCode','') FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, runConcept+":"+id).Scan(&status, &code); err != nil {
			t.Fatal(err)
		}
		if id == "orphan" {
			if status != "failed" || code != "approval_missing" {
				t.Fatalf("orphan state: %s %s", status, code)
			}
		} else if status != "waiting" {
			t.Fatalf("preserved %s changed to %s", id, status)
		}
	}
}

func TestSweepDB_CompletedHistoryDoesNotResurrectWork(t *testing.T) {
	db, i := sweepDB(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
SELECT 'v1:work:run:history-'||n,'v1:work:run',TIMESTAMPTZ '2026-01-01'+v*interval '1 minute','sweep-test','{}',
 jsonb_build_object('status',CASE WHEN v=10 THEN 'succeeded' ELSE 'running' END,'detail',repeat('x',1000))
FROM generate_series(1,1000) n CROSS JOIN generate_series(1,10) v`)
	if err != nil {
		t.Fatal(err)
	}
	insertSweepRow(t, db, "v1:work:run:still-waiting", runConcept, time.Now().UTC(), map[string]any{"status": "waiting"})
	rows, err := i.runsInFlight(ctx)
	if err != nil || len(rows) != 1 || rows[0]["id"] != "v1:work:run:still-waiting" {
		t.Fatalf("completed historical versions entered recovery: %v %v", rows, err)
	}
}

func TestSweepDB_DeleteBindsExactIDsAndRemovesVectors(t *testing.T) {
	db, i := sweepDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	target := observationConcept + ":quoted' OR true --"
	keep := observationConcept + ":keep"
	insertSweepRow(t, db, target, observationConcept, now.Add(-time.Hour), nil)
	insertSweepRow(t, db, target, observationConcept, now, nil)
	insertSweepRow(t, db, target, modelCallConcept, now.Add(time.Second), nil)
	insertSweepRow(t, db, keep, observationConcept, now, nil)
	for _, id := range []string{target, keep} {
		_, err := db.ExecContext(ctx, `INSERT INTO node_vectors (id,concept,vector_field,embedding) VALUES (?, ?, 'content', array_fill(0::real, ARRAY[1536])::vector)`, id, observationConcept)
		if err != nil {
			t.Fatal(err)
		}
	}
	n, err := i.deleteJournalRows(ctx, map[string][]string{observationConcept: {target}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted %d versions, want 2", n)
	}
	var remaining, vectors int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes"`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM node_vectors WHERE id = ?`, keep).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 || vectors != 1 {
		t.Fatalf("unrelated rows/vector not preserved: rows=%d keepVectors=%d", remaining, vectors)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM node_vectors WHERE id = ?`, target).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if vectors != 0 {
		t.Fatalf("deleted row retained %d vectors", vectors)
	}
	n, err = i.deleteJournalRows(ctx, map[string][]string{observationConcept: {target}})
	if err != nil || n != 0 {
		t.Fatalf("repeat delete: %d %v", n, err)
	}
}

type sweepDBArchive struct {
	blobs [][]byte
	fail  bool
}

func (a *sweepDBArchive) Upload(_ context.Context, _, object string, data []byte, _ string) (string, error) {
	if a.fail {
		return "", fmt.Errorf("test archive unavailable")
	}
	a.blobs = append(a.blobs, append([]byte(nil), data...))
	return object, nil
}

func TestSweepDB_BuiltinLifecycleWithCanonicalRunID(t *testing.T) {
	db, i := sweepDB(t)
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	eng, err := memqlengine.New(db)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	i.engine = eng
	i.admitRow = memqlengine.AdmitSourceRow
	if err := eng.RegisterIntegration(i); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	i.SetNow(func() time.Time { return now })
	owner := actorCtx("sweep-db-owner")
	maintenance := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns"))
	st := i.store()
	runID := "v1:work:run:sweep-db-run"
	if err := st.createRunRow(owner, runSeed{RunId: runID, AutomationName: "sweepDBProbe", TemplateFingerprint: "probe", Status: "waiting", Mode: "live", ReplayPolicy: "strict", StartedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.updateRun(owner, runID, map[string]any{"waitingOn": map[string]any{"kind": "timer", "resumeAt": rfc(now.Add(-time.Minute))}}); err != nil {
		t.Fatal(err)
	}
	execute := func(query string) {
		t.Helper()
		if _, err := eng.Execute(maintenance, query); err != nil {
			t.Fatal(err)
		}
	}
	execute(`workSweepWaiting(olderThanSeconds: 60)`)
	var status string
	if err := db.QueryRowContext(context.Background(), `SELECT payload->>'status' FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("due timer status=%q, want running", status)
	}
	// The actual mutation path advances this same canonical run before retention.
	if err := st.updateRun(owner, runID, map[string]any{"status": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvModelCallRetentionDays, "1")
	t.Setenv(EnvObservationRetentionDays, "1")
	t.Setenv(EnvArchiveContainer, "test-archive")
	for _, concept := range []string{modelCallConcept, observationConcept} {
		insertSweepRow(t, db, concept+":detail", concept, now.Add(-48*time.Hour), map[string]any{"runId": runID, "ownerUserId": canonicalUser("sweep-db-owner"), "inputTokens": 2, "outputTokens": 3})
	}
	archive := &sweepDBArchive{}
	i.SetArchiver(archive)
	assertRetained := func() {
		t.Helper()
		var count int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept IN (?, ?)`, modelCallConcept, observationConcept).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 2 || len(archive.blobs) != 0 {
			t.Fatalf("unarchived detail changed: rows=%d uploads=%d", count, len(archive.blobs))
		}
	}
	execute(`workRetentionSweep(dryRun: true)`)
	assertRetained()
	archive.fail = true
	execute(`workRetentionSweep(dryRun: false)`)
	assertRetained()
	archive.fail = false
	execute(`workRetentionSweep(dryRun: false)`)
	if len(archive.blobs) != 2 {
		t.Fatalf("archived %d objects, want both journal concepts", len(archive.blobs))
	}
	var remaining int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept IN (?, ?)`, modelCallConcept, observationConcept).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("retention left %d archived rows", remaining)
	}
	var summary []byte
	if err := db.QueryRowContext(context.Background(), `SELECT payload->'summary' FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, runID).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(summary, &fields); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(fields["modelCalls"]) != "1" || fmt.Sprint(fields["observations"]) != "1" {
		t.Fatalf("run summary not folded: %s", summary)
	}
	execute(`workRetentionSweep(dryRun: false)`)
	if len(archive.blobs) != 2 {
		t.Fatal("repeat retention archived already deleted rows")
	}
}
