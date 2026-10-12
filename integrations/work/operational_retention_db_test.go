package work

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type verifyingArchive struct {
	sweepDBArchive
	corrupt, downloadFail bool
	afterUpload           func()
}

func (a *verifyingArchive) Upload(ctx context.Context, c, o string, b []byte, kind string) (string, error) {
	result, err := a.sweepDBArchive.Upload(ctx, c, o, b, kind)
	if err == nil && a.afterUpload != nil {
		a.afterUpload()
	}
	return result, err
}
func (a *verifyingArchive) DownloadWithLimit(ctx context.Context, c, o string, limit int64) ([]byte, error) {
	if a.downloadFail {
		return nil, fmt.Errorf("download failed")
	}
	if a.corrupt {
		return []byte("corrupt archive"), nil
	}
	data, err := a.sweepDBArchive.DownloadWithLimit(ctx, c, o, limit)
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("over budget")
	}
	return data, err
}
func retentionOwner() context.Context {
	return auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("workJournalRetentionSweep"))
}

func TestOperationalRetentionDBProtectsActiveOwnedRecentAndArchivesSystemHistory(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "30")
	archive := &verifyingArchive{}
	i.SetArchiver(archive)
	for _, tc := range []struct {
		id, owner, goal, status string
		days                    int
	}{
		{"old", "", "", "succeeded", 40}, {"active", "", "", "running", 40}, {"owned", "person", "goal", "succeeded", 40}, {"recent", "", "", "succeeded", 2}, {"recent-detail", "", "", "succeeded", 40},
	} {
		id := runConcept + ":" + tc.id
		p := map[string]any{"ownerUserId": tc.owner, "goalId": tc.goal, "status": tc.status, "triggeredBy": "schedule"}
		insertSweepRow(t, db, id, runConcept, now.AddDate(0, 0, -tc.days-1), p)
		insertSweepRow(t, db, id, runConcept, now.AddDate(0, 0, -tc.days), p)
		insertSweepRow(t, db, "v1:work:step:"+tc.id, "v1:work:step", now.AddDate(0, 0, -tc.days), map[string]any{"ownerUserId": tc.owner, "runId": id, "status": "done"})
	}
	insertSweepRow(t, db, "v1:work:modelCall:recent", modelCallConcept, now.AddDate(0, 0, -1), map[string]any{"ownerUserId": "", "runId": runConcept + ":recent-detail"})
	insertSweepRow(t, db, "v1:identity:auditEvent:old", "v1:identity:auditEvent", now.AddDate(0, 0, -366), map[string]any{"action": "test", "sequence": int64(9007199254740993)})
	insertSweepRow(t, db, "v1:identity:auditEvent:recent", "v1:identity:auditEvent", now.AddDate(0, 0, -30), map[string]any{"action": "test"})
	insertSweepRow(t, db, "v1:business:product:keep", "v1:business:product", now.AddDate(0, 0, -900), map[string]any{"name": "business record"})
	// Operational safety evidence belonging to live work remains even past its age window.
	insertSweepRow(t, db, "v1:safety:classification:active", "v1:safety:classification", now.AddDate(0, 0, -100), map[string]any{"runId": runConcept + ":active"})
	insertSweepRow(t, db, "v1:safety:classification:old", "v1:safety:classification", now.AddDate(0, 0, -100), map[string]any{})
	dry, err := i.operationalRetention(ctx, true)
	if err != nil || len(dry) != len(operationalPolicies) || len(archive.blobs) != 0 {
		t.Fatalf("dry run: %v %v", dry, err)
	}
	result, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	deleted := 0
	for _, r := range result {
		deleted += r.DeletedVersions
	}
	if deleted != 5 {
		t.Fatalf("deleted %d versions, expected old audit, classification and run+step: %+v", deleted, result)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes"`).Scan(&count); err != nil || count != 16 {
		t.Fatalf("preserved rows=%d: %v", count, err)
	}
	// Recoverable archives contain the historical versions and intrinsic fields.
	archived := 0
	for object, blob := range archive.objects {
		if !strings.HasSuffix(object, fmt.Sprintf("/%x.ndjson.gz", sha256.Sum256(blob))) {
			t.Fatalf("archive filename cannot be independently verified with sha256sum: %s", object)
		}
		r, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(r)
		dec.UseNumber()
		for {
			var row map[string]any
			err = dec.Decode(&row)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"id", "concept", "createdAt", "createdBy", "schema", "payload", "provenance"} {
				if _, ok := row[key]; !ok {
					t.Fatalf("archive missing %s", key)
				}
			}
			if row["id"] == "v1:identity:auditEvent:old" && fmt.Sprint(row["payload"].(map[string]any)["sequence"]) != "9007199254740993" {
				t.Fatal("archive rounded integer evidence before deleting the original")
			}
			archived++
		}
		_ = r.Close()
	}
	if archived != 5 {
		t.Fatalf("archive lost history: %d", archived)
	}
}

func TestVerifiedRetentionDBFailuresAndConcurrentWritesPreserveHistory(t *testing.T) {
	for _, mode := range []string{"upload", "download", "corrupt", "concurrent revision", "concurrent child"} {
		t.Run(mode, func(t *testing.T) {
			db, i := sweepDB(t)
			ctx := retentionOwner()
			now := time.Now().UTC().Truncate(time.Second)
			cutoff := now.AddDate(0, 0, -30)
			old := now.AddDate(0, 0, -40)
			t.Setenv(EnvArchiveContainer, "test-retention")
			id := runConcept + ":guard"
			p := map[string]any{"ownerUserId": "", "status": "succeeded", "goalId": "", "triggeredBy": "schedule"}
			insertSweepRow(t, db, id, runConcept, old.Add(-time.Hour), p)
			insertSweepRow(t, db, id, runConcept, old, p)
			archive := &verifyingArchive{}
			i.SetArchiver(archive)
			switch mode {
			case "upload":
				archive.fail = true
			case "download":
				archive.downloadFail = true
			case "corrupt":
				archive.corrupt = true
			case "concurrent revision":
				archive.afterUpload = func() {
					insertSweepRow(t, db, id, runConcept, now, map[string]any{"status": "running", "ownerUserId": ""})
				}
			case "concurrent child":
				archive.afterUpload = func() {
					insertSweepRow(t, db, "v1:work:step:new", "v1:work:step", now, map[string]any{"runId": id, "ownerUserId": ""})
				}
			}
			_, deleted, _, err := i.retireVerified(ctx, runConcept, []map[string]any{{"id": id, "createdAt": rfc(old)}}, true, cutoff, false)
			if deleted != 0 {
				t.Fatalf("unsafe delete: %d", deleted)
			}
			if mode == "upload" || mode == "download" || mode == "corrupt" {
				if err == nil {
					t.Fatal("failed archive reported success")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=?`, id).Scan(&count); err != nil || count < 2 {
				t.Fatalf("history lost: %d %v", count, err)
			}
		})
	}
}

func TestVerifiedRetentionDBPartialBatchesCannotOverwriteArchives(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	old := time.Now().UTC().Truncate(time.Second).AddDate(0, 0, -200)
	t.Setenv(EnvArchiveContainer, "test-retention")
	archive := &verifyingArchive{}
	i.SetArchiver(archive)
	for _, id := range []string{"v1:work:observation:first", "v1:work:observation:second"} {
		insertSweepRow(t, db, id, observationConcept, old, map[string]any{"ownerUserId": ""})
		_, deleted, _, err := i.retireVerified(ctx, observationConcept, []map[string]any{{"id": id, "createdAt": rfc(old)}}, false, time.Now(), false)
		if err != nil || deleted != 1 {
			t.Fatalf("partial retire: %d %v", deleted, err)
		}
	}
	if len(archive.objects) != 2 {
		t.Fatal("later batch overwrote an earlier archive")
	}
}

func TestOperationalRetentionDBHonorsStoredPolicyAndExplicitOverride(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC()
	t.Setenv("MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS", "")
	insertSweepRow(t, db, "v1:platform:globalVariable:audit", "v1:platform:globalVariable", now, map[string]any{"name": "MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS", "value": "730", "active": true})
	insertSweepRow(t, db, "v1:platform:globalVariable:worker", "v1:platform:globalVariable", now, map[string]any{"name": "WORKER_INVOCATION_RETENTION_DAYS", "value": "180", "active": true})
	windows, err := i.operationalRetentionWindows(ctx)
	if err != nil || windows["MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS"] != 730 || windows["MEMQL_WORKER_INVOCATION_RETENTION_DAYS"] != 180 {
		t.Fatalf("stored policy lost: %v %v", windows, err)
	}
	t.Setenv("MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS", "800")
	windows, err = i.operationalRetentionWindows(ctx)
	if err != nil || windows["MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS"] != 800 {
		t.Fatalf("explicit policy lost: %v %v", windows, err)
	}
}

func TestVerifiedRetentionDBAdmissionRefusalPreservesWholeHistory(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	t.Setenv(EnvArchiveContainer, "test-retention")
	archive := &verifyingArchive{}
	i.SetArchiver(archive)
	id := observationConcept + ":denied-history"
	insertSweepRow(t, db, id, observationConcept, now.Add(-time.Hour), map[string]any{"ownerUserId": "other"})
	insertSweepRow(t, db, id, observationConcept, now, map[string]any{"ownerUserId": ""})
	i.admitRow = func(_ context.Context, n memorynodes.MemoryNode) bool {
		return !bytes.Contains(n.Payload, []byte("other"))
	}
	_, deleted, _, err := i.retireVerified(ctx, observationConcept, []map[string]any{{"id": id, "createdAt": rfc(now)}}, false, now, false)
	if err == nil || deleted != 0 || len(archive.blobs) != 0 {
		t.Fatalf("denied history leaked/retired: %d %v", deleted, err)
	}
}

func TestVerifiedRetentionDBVectorFailureRollsBackRecordDeletion(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	t.Setenv(EnvArchiveContainer, "test-retention")
	i.SetArchiver(&verifyingArchive{})
	id := observationConcept + ":vector-failure"
	insertSweepRow(t, db, id, observationConcept, now, map[string]any{"ownerUserId": ""})
	// A broken vector table injects a real transactional failure after the
	// MemoryNodes DELETE, without touching the shared developer tables.
	if _, err := db.ExecContext(ctx, `ALTER TABLE node_vectors RENAME COLUMN id TO broken_id`); err != nil {
		t.Fatal(err)
	}
	_, deleted, _, err := i.retireVerified(ctx, observationConcept, []map[string]any{{"id": id, "createdAt": rfc(now)}}, false, now, false)
	if err == nil || deleted != 0 {
		t.Fatalf("vector failure was not atomic: %d %v", deleted, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=?`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("record deletion escaped rollback: %d %v", count, err)
	}
}

func TestVerifiedRetentionDBParentResumedDuringArchivePreservesDetail(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -200)
	t.Setenv(EnvArchiveContainer, "test-retention")
	parent := runConcept + ":resumed"
	id := observationConcept + ":resumed"
	insertSweepRow(t, db, parent, runConcept, old, map[string]any{"status": "succeeded", "ownerUserId": ""})
	insertSweepRow(t, db, id, observationConcept, old, map[string]any{"runId": parent, "ownerUserId": ""})
	archive := &verifyingArchive{afterUpload: func() {
		insertSweepRow(t, db, parent, runConcept, now, map[string]any{"status": "running", "ownerUserId": ""})
	}}
	i.SetArchiver(archive)
	_, deleted, _, err := i.retireVerified(ctx, observationConcept, []map[string]any{{"id": id, "createdAt": rfc(old)}}, false, now, false)
	if err != nil || deleted != 0 {
		t.Fatalf("resumed parent lost detail: %d %v", deleted, err)
	}
}

func TestRetentionDBFailedSummaryPreservesJournal(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	old := time.Now().UTC().AddDate(0, 0, -200)
	engine := newRecordingEngine()
	engine.refuse("updateWorkRun", fmt.Errorf("summary write failed"))
	i.engine = engine
	t.Setenv(EnvArchiveContainer, "test-retention")
	archive := &verifyingArchive{}
	i.SetArchiver(archive)
	insertSweepRow(t, db, modelCallConcept+":summary-failure", modelCallConcept, old, map[string]any{"ownerUserId": "", "runId": runConcept + ":summary-failure"})
	result, err := i.RetentionSweep(ctx, false)
	if err == nil || result.RowsDeleted != 0 || len(archive.blobs) != 0 {
		t.Fatalf("summary failure lost detail: %+v %v", result, err)
	}
}

// statementRecorder keeps the SQL of every statement a pass sends, as bun
// formatted it for the server: placeholders already replaced by their literals,
// so a test reads the timestamps a candidate read was bounded by from the text
// the database received, not from the arguments the code meant to send.
type statementRecorder struct {
	mu   sync.Mutex
	sqls []string
}

func (r *statementRecorder) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (r *statementRecorder) AfterQuery(_ context.Context, e *bun.QueryEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, e.Query)
}

// candidateReads are the statements that select retention candidates, in the
// order they were sent: every one begins with the candidate SELECT.
func (r *statementRecorder) candidateReads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.sqls {
		if strings.HasPrefix(q, `SELECT n.id,n."createdAt",n.payload FROM `) {
			out = append(out, q)
		}
	}
	return out
}

var (
	candidateLower = regexp.MustCompile(`\bn\."createdAt" >= '([^']+)'`)
	candidateUpper = regexp.MustCompile(`\bn\."createdAt" < '([^']+)'`)
)

// candidateWindow is the [lower, upper) createdAt range a candidate read asked
// for, or ok=false when the statement lacks either bound.
func candidateWindow(t *testing.T, q string) (lo, hi time.Time, ok bool) {
	t.Helper()
	lower, upper := candidateLower.FindStringSubmatch(q), candidateUpper.FindStringSubmatch(q)
	if lower == nil || upper == nil {
		return time.Time{}, time.Time{}, false
	}
	parse := func(literal string) time.Time {
		at, err := time.Parse("2006-01-02 15:04:05.999999-07:00", literal)
		if err != nil {
			t.Fatalf("a candidate read carries a bound that is not a timestamp literal (%q): %v", literal, err)
		}
		return at
	}
	return parse(lower[1]), parse(upper[1]), true
}

// seedScheduledRuns writes perDay eligible system runs on each of the days
// (whole days before now), and one unowned step of each: the shape the
// system-run policy retires. A run's final version lands two hours into its UTC
// day, 5 s after the run before it, so the runs of a day fall in one day window
// and read in a known order; its first version is an hour earlier. It returns
// the run ids in that order.
func seedScheduledRuns(t *testing.T, db *bun.DB, prefix string, now time.Time, perDay int, days ...int) []string {
	t.Helper()
	var ids []string
	for _, age := range days {
		day := now.AddDate(0, 0, -age).Truncate(24 * time.Hour)
		name := fmt.Sprintf("%s-%d-", prefix, age)
		if _, err := db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload)
SELECT ?||r, ?, ?::timestamptz + interval '2 hours' + r * interval '5 seconds' - (2 - v) * interval '1 hour', 'sweep-test', 'object', '{}',
       jsonb_build_object('ownerUserId', '', 'goalId', '', 'triggeredBy', 'schedule', 'status', CASE WHEN v = 2 THEN 'succeeded' ELSE 'running' END)
FROM generate_series(1, ?) r CROSS JOIN generate_series(1, 2) v`, runConcept+":"+name, runConcept, day, perDay); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload)
SELECT ?||r, 'v1:work:step', ?::timestamptz + interval '2 hours' + r * interval '5 seconds' - (2 - v) * interval '1 minute', 'sweep-test', 'object', '{}',
       jsonb_build_object('ownerUserId', '', 'runId', ?||r, 'status', 'done')
FROM generate_series(1, ?) r CROSS JOIN generate_series(1, 2) v`, "v1:work:step:"+name, day, runConcept+":"+name, perDay); err != nil {
			t.Fatal(err)
		}
		for j := 1; j <= perDay; j++ {
			ids = append(ids, fmt.Sprintf("%s:%s%d", runConcept, name, j))
		}
	}
	return ids
}

// storedVersionsLike counts the versions stored under ids matching a LIKE.
func storedVersionsLike(t *testing.T, db *bun.DB, like string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id LIKE ?`, like).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// versionsPerScheduledRun is what retiring one seeded run deletes: its two
// versions and its step's two.
const versionsPerScheduledRun = 4

// TestOperationalRetentionWalksDayWindows: 250 eligible scheduled runs spread
// over five days older than the cutoff are all retired by one pass, and the pass
// reads them a day at a time. Every candidate read carries both a lower and an
// upper createdAt bound a day or less apart, which is what keeps the statement
// small on a table of hundreds of thousands of run versions (the 2026-10-10
// and 2026-10-11 sweeps timed out reading all of them in one statement).
func TestOperationalRetentionWalksDayWindows(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "")
	archive := &verifyingArchive{}
	i.SetArchiver(archive)
	recorder := &statementRecorder{}
	db.AddQueryHook(recorder)

	days := []int{40, 39, 38, 37, 36}
	seedScheduledRuns(t, db, "win", now, 50, days...)
	// A run inside the window is not a candidate and must still be there.
	seedScheduledRuns(t, db, "inside", now, 3, 5)

	results, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, results, runConcept, EnvSystemRunRetentionDays)
	if r.Candidates != 250 || r.DeletedVersions != 250*versionsPerScheduledRun || r.ArchivedVersions != r.DeletedVersions {
		t.Fatalf("system-run policy %+v, want 250 candidates retired with their steps (%d versions)", r, 250*versionsPerScheduledRun)
	}
	if n := storedVersionsLike(t, db, "v1:work:%:win-%"); n != 0 {
		t.Fatalf("%d versions of the 250 eligible runs were left behind", n)
	}
	if n := storedVersionsLike(t, db, "v1:work:%:inside-%"); n != 3*versionsPerScheduledRun {
		t.Fatalf("a run inside the window was touched: %d of its %d versions remain", n, 3*versionsPerScheduledRun)
	}

	reads := recorder.candidateReads()
	if len(reads) == 0 {
		t.Fatal("no candidate read was recorded; the assertions below would pass over nothing")
	}
	began := map[int64]bool{}
	for _, q := range reads {
		// Nothing but the run concept has a version older than its cutoff, so
		// nothing else is read: the probe found no day to walk.
		if !strings.Contains(q, `n.concept='v1:work:run'`) {
			t.Fatalf("a candidate read for a concept with nothing past its cutoff: %.300s", q)
		}
		lo, hi, ok := candidateWindow(t, q)
		if !ok {
			t.Fatalf("a candidate read has no createdAt window: %.300s", q)
		}
		if !lo.Before(hi) || hi.Sub(lo) > retentionCandidateWindow {
			t.Fatalf("a candidate read covers [%s, %s), which is not within one %s window", lo, hi, retentionCandidateWindow)
		}
		began[lo.UnixNano()] = true
	}
	for _, age := range days {
		day := now.AddDate(0, 0, -age).Truncate(24 * time.Hour)
		if !began[day.UnixNano()] {
			t.Errorf("no candidate read began at %s; the walk skipped that day", day)
		}
	}
}

// TestOperationalRetentionStopsAtBudgetAndResumes: the pass spends at most its
// time budget on candidates and stops cleanly with work left, and the next pass
// picks up where the oldest remaining version is. The fake clock moves two
// minutes for each window archived, so the pass retires windows at 2, 4 and 6
// minutes and stops after the one that crossed the 4-minute budget.
func TestOperationalRetentionStopsAtBudgetAndResumes(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	clock := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return clock })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{afterUpload: func() { clock = clock.Add(2 * time.Minute) }})

	seedScheduledRuns(t, db, "budget", clock, 50, 40, 39, 38, 37, 36)
	stored := func() int { return storedVersionsLike(t, db, "v1:work:%:budget-%") }
	if stored() != 250*versionsPerScheduledRun {
		t.Fatalf("seeded %d versions", stored())
	}

	first, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, first, runConcept, EnvSystemRunRetentionDays)
	if want := 3 * 50 * versionsPerScheduledRun; r.DeletedVersions != want || !r.BudgetExhausted {
		t.Fatalf("first pass %+v, want three windows (%d versions) retired and the budget reported spent", r, want)
	}
	if left := stored(); left != 2*50*versionsPerScheduledRun {
		t.Fatalf("%d versions remain after the first pass, want the two newest windows' %d", left, 2*50*versionsPerScheduledRun)
	}
	// The result still has an entry for every policy. The ones before the policy
	// that spent the budget finished; it and every one after it say they did not.
	if len(first) != len(operationalPolicies) {
		t.Fatalf("%d results for %d policies", len(first), len(operationalPolicies))
	}
	stoppedIn := policyIndex(runConcept, EnvSystemRunRetentionDays)
	for k, result := range first {
		if want := k >= stoppedIn; result.BudgetExhausted != want {
			t.Errorf("policy %d (%s on %s): BudgetExhausted=%v, want %v", k, result.Concept, result.Env, result.BudgetExhausted, want)
		}
		if k > stoppedIn && (result.Candidates != 0 || result.DeletedVersions != 0) {
			t.Errorf("policy %d (%s on %s) was not reached but reports %+v", k, result.Concept, result.Env, result)
		}
	}

	second, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r = resultFor(t, second, runConcept, EnvSystemRunRetentionDays)
	if want := 2 * 50 * versionsPerScheduledRun; r.DeletedVersions != want || r.BudgetExhausted {
		t.Fatalf("second pass %+v, want the remaining %d versions retired inside the budget", r, want)
	}
	if left := stored(); left != 0 {
		t.Fatalf("%d versions remain after the second pass", left)
	}
}

// TestOperationalRetentionPagesAWindowWithoutRereadingAKeptPage: a window with
// more candidates than one read returns is read in pages, each starting after
// the last candidate of the one before. A page the delete guard kept is
// therefore not read again -- re-reading the window from its start would hand
// back the same hundred candidates until the budget ran out -- and a dry run,
// which deletes nothing, still reads every candidate exactly once.
func TestOperationalRetentionPagesAWindowWithoutRereadingAKeptPage(t *testing.T) {
	t.Run("a page the guard kept is not read again", func(t *testing.T) {
		db, i := sweepDB(t)
		ctx, cancel := context.WithTimeout(retentionOwner(), 20*time.Second)
		defer cancel()
		now := time.Now().UTC().Truncate(time.Second)
		i.SetNow(func() time.Time { return now })
		t.Setenv(EnvArchiveContainer, "test-retention")
		t.Setenv(EnvSystemRunRetentionDays, "")
		// 130 candidates in one day: a full page of 100 and a page of 30.
		runs := seedScheduledRuns(t, db, "page", now, 130, 40)
		archive := &verifyingArchive{}
		archive.afterUpload = func() {
			if len(archive.objects) == 1 {
				// The first page is archived; a newer version of one of its runs
				// appears before the delete, so the guard keeps the whole page.
				insertSweepRow(t, db, runs[0], runConcept, now, map[string]any{"ownerUserId": "", "goalId": "", "triggeredBy": "schedule", "status": "running"})
			}
		}
		i.SetArchiver(archive)

		results, err := i.operationalRetention(ctx, false)
		if err != nil {
			t.Fatalf("a kept page stopped the pass: %v", err)
		}
		r := resultFor(t, results, runConcept, EnvSystemRunRetentionDays)
		if r.Candidates != 130 || r.DeletedVersions != 30*versionsPerScheduledRun || len(archive.objects) != 2 {
			t.Fatalf("result %+v with %d objects, want 130 candidates read once each and only the second page's %d versions deleted", r, len(archive.objects), 30*versionsPerScheduledRun)
		}
		if n := storedVersionsLike(t, db, "v1:work:%:page-%"); n != 100*versionsPerScheduledRun+1 {
			t.Fatalf("%d versions remain, want the kept page's %d plus the newer version", n, 100*versionsPerScheduledRun)
		}
	})

	t.Run("a dry run reads every candidate once and writes nothing", func(t *testing.T) {
		db, i := sweepDB(t)
		ctx, cancel := context.WithTimeout(retentionOwner(), 20*time.Second)
		defer cancel()
		now := time.Now().UTC().Truncate(time.Second)
		i.SetNow(func() time.Time { return now })
		t.Setenv(EnvArchiveContainer, "test-retention")
		t.Setenv(EnvSystemRunRetentionDays, "")
		archive := &verifyingArchive{}
		i.SetArchiver(archive)
		seedScheduledRuns(t, db, "dry", now, 230, 40)

		results, err := i.operationalRetention(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		r := resultFor(t, results, runConcept, EnvSystemRunRetentionDays)
		if r.Candidates != 230 || r.DeletedVersions != 0 || len(archive.blobs) != 0 {
			t.Fatalf("dry run %+v with %d uploads, want 230 candidates and nothing written", r, len(archive.blobs))
		}
		if n := storedVersionsLike(t, db, "v1:work:%:dry-%"); n != 230*versionsPerScheduledRun {
			t.Fatalf("a dry run changed the table: %d versions", n)
		}
	})
}

// TestOperationalRetentionPageEndsWhereTheScanEndedNotWhereTheAdmittedRowsDid:
// a row the admission gate refuses is not a candidate, but it is still a row the
// read went through. A full page whose first sixty rows were refused holds forty
// candidates, and the window is not over: the next page starts after the page's
// last row, refused or not, or the rows behind it would never be read.
func TestOperationalRetentionPageEndsWhereTheScanEndedNotWhereTheAdmittedRowsDid(t *testing.T) {
	db, i := sweepDB(t)
	ctx, cancel := context.WithTimeout(retentionOwner(), 20*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{})
	runs := seedScheduledRuns(t, db, "gate", now, 150, 40)
	refused := map[string]bool{}
	for _, id := range runs[:60] {
		refused[id] = true
	}
	i.admitRow = func(_ context.Context, n memorynodes.MemoryNode) bool { return !refused[n.ID] }

	results, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, results, runConcept, EnvSystemRunRetentionDays)
	if r.Candidates != 90 || r.DeletedVersions != 90*versionsPerScheduledRun {
		t.Fatalf("result %+v, want the 90 admitted runs retired: the refused rows ended the first page early, so the rows behind them were never read", r)
	}
	if n := storedVersionsLike(t, db, "v1:work:run:gate-%"); n != 60*2 {
		t.Fatalf("%d run versions remain, want the 60 refused runs' %d", n, 60*2)
	}
}

// explainNode is the part of a plan node the row count needs.
type explainNode struct {
	NodeType         string        `json:"Node Type"`
	Relation         string        `json:"Relation Name"`
	ActualRows       float64       `json:"Actual Rows"`
	ActualLoops      float64       `json:"Actual Loops"`
	RemovedByFilter  float64       `json:"Rows Removed by Filter"`
	RemovedByRecheck float64       `json:"Rows Removed by Index Recheck"`
	Plans            []explainNode `json:"Plans"`
}

// rowsExamined is how many "MemoryNodes" rows the plan went through, the ones it
// returned and the ones it threw away, across every loop of every scan -- the
// number that says whether a statement probed an index or walked a table.
func rowsExamined(n explainNode) float64 {
	total := 0.0
	if n.Relation == "MemoryNodes" && strings.Contains(n.NodeType, "Scan") {
		total += (n.ActualRows + n.RemovedByFilter + n.RemovedByRecheck) * n.ActualLoops
	}
	for _, child := range n.Plans {
		total += rowsExamined(child)
	}
	return total
}

// explainRowsExamined runs the statement under EXPLAIN ANALYZE and returns how
// many "MemoryNodes" rows it examined. The statement runs in a transaction that
// is rolled back, so a DELETE changes nothing.
func explainRowsExamined(t *testing.T, db *bun.DB, st retirementStatement) float64 {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	if err := tx.QueryRowContext(context.Background(), `EXPLAIN (ANALYZE, FORMAT JSON) `+st.query, st.args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v\n%.400s", err, st.query)
	}
	var plans []struct {
		Plan explainNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("explain output: %v %s", err, raw)
	}
	return rowsExamined(plans[0].Plan)
}

// TestRetireVerifiedStatementsExamineOnlyTheBatch: the statements that load a
// batch's versions and delete them go to the batch's rows by index and look at
// nothing else. Beside a hundred runs to retire the table holds ten thousand
// others, with their steps; a statement that examines more than a few times the
// batch has walked the table instead.
//
// This is the failure of 2026-10-10. The sweep found 1,045 candidates and then
// a statement of retireVerified timed out, so nothing was archived or deleted.
// Measured on a seeded history of 200,000 runs and 800,000 step versions, the
// load of one batch of 100 took 4.0 to 10 s: its two arms were ORed in one
// WHERE, which the planner answers with a full scan of the (concept, id,
// createdAt) index and a filter (3.3 million buffers, 708,400 rows thrown away in
// each of seven chunks). Split in two, with the steps found by a probe of the
// partial runId index for each run, it took 0.1 s. The delete's guard that no
// child of the batch was left behind had the same shape (865 ms of the
// statement's 937).
func TestRetireVerifiedStatementsExamineOnlyTheBatch(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	t.Setenv(EnvArchiveContainer, "test-retention")
	i.SetArchiver(&verifyingArchive{})

	// Volume: ten thousand runs of three versions with a step of four versions
	// each, none of them in the batch.
	if _, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload)
SELECT 'v1:work:run:bystander-'||r, 'v1:work:run', ?::timestamptz - r * interval '30 seconds' + v * interval '10 seconds', 'sweep-test', 'object', '{}',
       jsonb_build_object('ownerUserId', '', 'goalId', '', 'triggeredBy', 'schedule', 'status', CASE WHEN v = 3 THEN 'succeeded' ELSE 'running' END)
FROM generate_series(1, 10000) r CROSS JOIN generate_series(1, 3) v`, now.AddDate(0, 0, -50)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload)
SELECT 'v1:work:step:bystander-'||r, 'v1:work:step', ?::timestamptz - r * interval '30 seconds' + v * interval '3 seconds', 'sweep-test', 'object', '{}',
       jsonb_build_object('ownerUserId', '', 'runId', 'v1:work:run:bystander-'||r, 'status', 'done')
FROM generate_series(1, 10000) r CROSS JOIN generate_series(1, 4) v`, now.AddDate(0, 0, -50)); err != nil {
		t.Fatal(err)
	}
	ids := seedScheduledRuns(t, db, "batch", now, 100, 40)
	if _, err := db.ExecContext(ctx, `ANALYZE "MemoryNodes"`); err != nil {
		t.Fatal(err)
	}

	// Four times what the batch's own rows come to is room for every probe's
	// misses; the walk of the table is over seventy thousand.
	bound := float64(4 * 100 * versionsPerScheduledRun)
	loads := retirementLoads(runConcept, ids, true)
	for n, load := range loads {
		examined := explainRowsExamined(t, db, load)
		t.Logf("load %d examined %.0f MemoryNodes rows (bound %.0f)", n+1, examined, bound)
		if examined > bound {
			t.Errorf("a load of the batch's versions examined %.0f rows, more than %.0f: it walked the table\n%.300s", examined, bound, load.query)
		}
	}

	nodes, err := i.loadRetirementVersions(ctx, db, runConcept, ids, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 100*versionsPerScheduledRun {
		t.Fatalf("loaded %d versions of the batch, want %d", len(nodes), 100*versionsPerScheduledRun)
	}
	keys := make([]retirementKey, 0, len(nodes))
	for _, n := range nodes {
		keys = append(keys, retirementKey{n.ID, n.Concept, n.CreatedAt})
	}
	rawKeys, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	del := retirementDelete(runConcept, true, ids, string(rawKeys))
	examined := explainRowsExamined(t, db, del)
	t.Logf("the guarded delete examined %.0f MemoryNodes rows (bound %.0f)", examined, bound*3)
	if examined > bound*3 {
		t.Errorf("the guarded delete examined %.0f rows, more than %.0f: a guard walked the table\n%.300s", examined, bound*3, del.query)
	}
}

// TestRetireVerifiedChildrenProbeBindsRunIDsAsData: a run's id goes into the
// probes that find its children -- the load and the delete's guard that none was
// left out of the archive -- as data, whatever characters it holds. A run whose
// id is an injection attempt is retired with its own step, and the run beside it
// is not touched.
func TestRetireVerifiedChildrenProbeBindsRunIDsAsData(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -40)
	cutoff := now.AddDate(0, 0, -30)
	t.Setenv(EnvArchiveContainer, "test-retention")
	i.SetArchiver(&verifyingArchive{})

	quoted := runConcept + `:quoted' OR true --`
	bystander := runConcept + ":bystander"
	for _, id := range []string{quoted, bystander} {
		run := func(status string) map[string]any {
			return map[string]any{"ownerUserId": "", "goalId": "", "triggeredBy": "schedule", "status": status}
		}
		insertSweepRow(t, db, id, runConcept, old.Add(-time.Hour), run("running"))
		insertSweepRow(t, db, id, runConcept, old, run("succeeded"))
		insertSweepRow(t, db, "v1:work:step:of-"+id, "v1:work:step", old, map[string]any{"ownerUserId": "", "runId": id, "status": "done"})
	}

	_, deleted, _, err := i.retireVerified(ctx, runConcept, []map[string]any{{"id": quoted, "createdAt": rfc(old)}}, true, cutoff, false)
	if err != nil || deleted != 3 {
		t.Fatalf("retired %d versions, %v: want the quoted run's two and its step's one", deleted, err)
	}
	if n := storedVersionsLike(t, db, "%"+quoted+"%"); n != 0 {
		t.Fatalf("%d versions of the quoted run and its step remain", n)
	}
	if n := storedVersionsLike(t, db, "%bystander%"); n != 3 {
		t.Fatalf("the run beside it has %d of its 3 versions", n)
	}
}
