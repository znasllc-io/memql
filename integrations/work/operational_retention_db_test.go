package work

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
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

// TestOperationalRetentionStopsAtBudgetAndResumes: a policy spends at most its
// share of the pass's time budget on candidates and stops cleanly with work
// left, and the next pass picks up where the oldest version stored is. The
// system-run policy is the fifth of eight and the policies before it have nothing
// to do, so when its turn comes the four minutes are whole and it may spend a
// quarter of them, a minute. The fake clock moves half a minute for each window
// archived: windows retire at 0:30, 1:00 and 1:30, and the policy stops at the
// one that crossed the minute (the comparison is strictly greater, so 1:00 has
// not).
func TestOperationalRetentionStopsAtBudgetAndResumes(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	clock := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return clock })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{afterUpload: func() { clock = clock.Add(30 * time.Second) }})

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
		t.Fatalf("first pass %+v, want three windows (%d versions) retired and the policy's share reported spent", r, want)
	}
	if left := stored(); left != 2*50*versionsPerScheduledRun {
		t.Fatalf("%d versions remain after the first pass, want the two newest windows' %d", left, 2*50*versionsPerScheduledRun)
	}
	// The result has an entry for every policy, and only the one that spent its
	// share says so: the others read what they had to read.
	if len(first) != len(operationalPolicies) {
		t.Fatalf("%d results for %d policies", len(first), len(operationalPolicies))
	}
	stoppedIn := policyIndex(runConcept, EnvSystemRunRetentionDays)
	for k, result := range first {
		if want := k == stoppedIn; result.BudgetExhausted != want {
			t.Errorf("policy %d (%s on %s): BudgetExhausted=%v, want %v", k, result.Concept, result.Env, result.BudgetExhausted, want)
		}
	}

	second, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r = resultFor(t, second, runConcept, EnvSystemRunRetentionDays)
	if want := 2 * 50 * versionsPerScheduledRun; r.DeletedVersions != want || r.BudgetExhausted {
		t.Fatalf("second pass %+v, want the remaining %d versions retired inside the share", r, want)
	}
	if left := stored(); left != 0 {
		t.Fatalf("%d versions remain after the second pass", left)
	}
}

// TestOperationalRetentionStopsBetweenPagesOfAWindow: the share is checked before
// every read, not every window, so a window of many pages does not run on once the
// policy's time is gone -- and the policy says so, since a full page came back and
// its window may hold more. 230 runs on one day are three pages; the fake clock
// moves 40 s for each, and the system-run policy's share is a minute, so it reads
// two pages (200 runs) and stops. The next pass starts again at the oldest version
// stored, which is the 30 left, and finishes.
func TestOperationalRetentionStopsBetweenPagesOfAWindow(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	clock := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return clock })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{afterUpload: func() { clock = clock.Add(40 * time.Second) }})
	seedScheduledRuns(t, db, "pages", clock, 230, 40)

	first, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, first, runConcept, EnvSystemRunRetentionDays)
	if r.Candidates != 200 || r.DeletedVersions != 200*versionsPerScheduledRun || !r.BudgetExhausted {
		t.Fatalf("first pass %+v, want two pages (200 runs) retired and the policy reported behind", r)
	}
	if n := storedVersionsLike(t, db, "v1:work:run:pages-%"); n != 30*2 {
		t.Fatalf("%d run versions remain, want the 30 runs of the last page", n)
	}

	second, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r = resultFor(t, second, runConcept, EnvSystemRunRetentionDays)
	if r.Candidates != 30 || r.DeletedVersions != 30*versionsPerScheduledRun || r.BudgetExhausted {
		t.Fatalf("second pass %+v, want the last 30 runs retired and the policy not behind", r)
	}
}

// TestOperationalRetentionSharesItsBudgetAmongThePolicies: a policy that spends
// its share does not take the later policies' time, and does not stop them. The
// fake clock moves 55 s for each batch archived, far more than a share, so every
// policy overruns by the batch it was in the middle of -- the most an overrun can
// be, since the share is checked before each read.
//
// Four minutes, eight policies. The system-run policy (fifth) finds the whole
// four minutes left and may spend a fourth of them, 60 s: it retires two
// windows, 55 s apiece, and stops (a share of 30 s, four minutes over all eight
// from the start, would have stopped it after one; a budget for the pass as a
// whole would have let it retire all five). The policies after it each get what is
// left over what is still to run -- 43 s, 37.5 s, 20 s -- run, retire the one
// window they have, and are not reported spent, because nothing is left for them
// to read.
func TestOperationalRetentionSharesItsBudgetAmongThePolicies(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	clock := time.Now().UTC().Truncate(time.Second)
	step := 55 * time.Second
	i.SetNow(func() time.Time { return clock })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "")
	t.Setenv(EnvPipelinesRunRetentionDays, "")
	i.SetArchiver(&verifyingArchive{afterUpload: func() { clock = clock.Add(step) }})

	// Five windows of system runs; and, on the last of those days, thirty finished
	// pipeline runs with a step each, twenty finished pipelines run rows, and a
	// goal for ten of the runs -- each a single window of its policy's work.
	seedScheduledRuns(t, db, "sys", clock, 50, 40, 39, 38, 37, 36)
	at := clock.AddDate(0, 0, -36).Truncate(24 * time.Hour).Add(12 * time.Hour)
	for n := 1; n <= 30; n++ {
		name := fmt.Sprintf("pipe-%02d", n)
		run := seedPipelineWorkRun(t, db, name, pipelineOwner, "succeeded", at)
		seedWorkChild(t, db, "v1:work:step", name, run, at, ownedBy(pipelineOwner, "status", "done"))
		if n <= 10 {
			seedGoal(t, db, name, at, pipelineGoal(pipelineOwner))
		}
	}
	for n := 1; n <= 20; n++ {
		seedPipelinesRun(t, db, fmt.Sprintf("pipe-%02d", n), pipelineOwner, "completed", at)
	}

	first, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		concept, env     string
		candidates, gone int
		exhausted        bool
		why              string
	}{
		{runConcept, EnvSystemRunRetentionDays, 100, 100 * versionsPerScheduledRun, true, "two windows of a 60 s share, and its share spent"},
		{runConcept, EnvPipelinesRunRetentionDays, 30, 30 * 4, false, "its one window, inside what was left"},
		{pipelinesRunConcept, EnvPipelinesRunRetentionDays, 20, 20 * 3, false, "its one window"},
		{goalConcept, EnvPipelinesRunRetentionDays, 10, 10 * 3, false, "its one window, the last of the time"},
	} {
		got := resultFor(t, first, want.concept, want.env)
		if got.Candidates != want.candidates || got.DeletedVersions != want.gone || got.BudgetExhausted != want.exhausted {
			t.Errorf("%s on %s: %+v, want %d candidates, %d versions deleted, spent=%v (%s)", want.concept, want.env, got, want.candidates, want.gone, want.exhausted, want.why)
		}
	}
	for k := 0; k < policyIndex(runConcept, EnvSystemRunRetentionDays); k++ {
		if first[k].BudgetExhausted || first[k].Candidates != 0 {
			t.Errorf("policy %d (%s) had nothing to do but reports %+v", k, first[k].Concept, first[k])
		}
	}
	if n := storedVersionsLike(t, db, "v1:work:%:sys-%"); n != 3*50*versionsPerScheduledRun {
		t.Fatalf("%d system-run versions remain, want the three newest windows' %d", n, 3*50*versionsPerScheduledRun)
	}
	if n := storedVersionsLike(t, db, "%:pipe-%"); n != 0 {
		t.Fatalf("%d versions of the pipeline runs, their run rows and their goals were left behind", n)
	}

	// The next pass, with the clock standing still, retires what the share left.
	step = 0
	second, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r := resultFor(t, second, runConcept, EnvSystemRunRetentionDays)
	if r.Candidates != 150 || r.DeletedVersions != 150*versionsPerScheduledRun || r.BudgetExhausted {
		t.Fatalf("second pass %+v, want the remaining 150 runs retired", r)
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
	SubplanName      string        `json:"Subplan Name"`
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
	// And the batch's own rows are the floor. Each statement goes to every one of
	// them, so a count below it means the plan's scans were not counted at all --
	// rowsExamined counts scans of "MemoryNodes", and on a hypertable they are
	// scans of its chunks -- and the bound above would pass on nothing. The
	// parents' load returns the hundred runs' two versions each, the children's
	// load their steps' two each, and the delete takes all of them.
	floors := []float64{100 * 2, 100 * 2}
	loads := retirementLoads(runConcept, ids, true)
	if len(loads) != len(floors) {
		t.Fatalf("%d loads, want the parents' and the children's (%d)", len(loads), len(floors))
	}
	for n, load := range loads {
		examined := explainRowsExamined(t, db, load)
		t.Logf("load %d examined %.0f MemoryNodes rows (floor %.0f, bound %.0f)", n+1, examined, floors[n], bound)
		if examined < floors[n] {
			t.Errorf("load %d examined %.0f MemoryNodes rows, fewer than the %.0f of the batch's own it returns: the plan's scans were not counted, so the bound proves nothing", n+1, examined, floors[n])
		}
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
	deleteFloor := float64(len(nodes))
	t.Logf("the guarded delete examined %.0f MemoryNodes rows (floor %.0f, bound %.0f)", examined, deleteFloor, bound*3)
	if examined < deleteFloor {
		t.Errorf("the guarded delete examined %.0f MemoryNodes rows, fewer than the %.0f versions it deletes: the plan's scans were not counted, so the bound proves nothing", examined, deleteFloor)
	}
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

// TestOperationalRetentionSkipsAheadOverEmptyStretches: the walk starts at the
// oldest version stored, retirable or not, so a row no policy retires -- a
// person's run, an old version of work still live -- anchors it, and one dated in
// 1970 or in year 1 is days upon days of windows with nothing in them. A window
// with no stored version of the concept at all is jumped: the next read starts on
// the day of the next version stored before the cutoff. The clock here moves a
// second for every candidate read, so a walk that goes through every empty day
// spends its share on them (the share of the system-run policy is a minute) and
// retires nothing.
func TestOperationalRetentionSkipsAheadOverEmptyStretches(t *testing.T) {
	for _, tc := range []struct {
		name     string
		anchored bool      // there is a version of a person's run, which no policy retires
		anchor   time.Time // and it is dated so (year 1 is time.Time's zero value, hence the flag)
		days     []int     // the days that hold eligible scheduled runs
		maxReads int       // candidate reads, both run policies together
	}{
		{"a version dated 1970 that no policy retires", true, time.Unix(0, 0).UTC(), []int{40, 39}, 12},
		{"a version dated year 1 that no policy retires", true, time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), []int{40, 39}, 12},
		{"a gap of days between two stretches of work", false, time.Time{}, []int{45, 36}, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, i := sweepDB(t)
			ctx := retentionOwner()
			start := time.Now().UTC().Truncate(time.Second)
			recorder := &statementRecorder{}
			db.AddQueryHook(recorder)
			i.SetNow(func() time.Time {
				return start.Add(time.Duration(len(recorder.candidateReads())) * time.Second)
			})
			t.Setenv(EnvArchiveContainer, "test-retention")
			t.Setenv(EnvSystemRunRetentionDays, "")
			t.Setenv(EnvPipelinesRunRetentionDays, "")
			i.SetArchiver(&verifyingArchive{})

			if tc.anchored {
				insertSweepRow(t, db, runConcept+":anchor", runConcept, tc.anchor, map[string]any{
					"ownerUserId": "v1:identity:user:alice", "goalId": "v1:work:goal:alice", "triggeredBy": "goal", "status": "running"})
			}
			seedScheduledRuns(t, db, "skip", start, 50, tc.days...)

			results, err := i.operationalRetention(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			r := resultFor(t, results, runConcept, EnvSystemRunRetentionDays)
			want := 50 * len(tc.days)
			if r.Candidates != want || r.DeletedVersions != want*versionsPerScheduledRun || r.BudgetExhausted {
				t.Fatalf("system-run policy %+v, want its %d eligible runs retired and its share not spent", r, want)
			}
			if n := storedVersionsLike(t, db, "v1:work:%:skip-%"); n != 0 {
				t.Fatalf("%d versions of the eligible runs were left behind", n)
			}
			if tc.anchored {
				if n := storedVersionsLike(t, db, "%:anchor"); n != 1 {
					t.Fatalf("the version no policy retires has %d versions stored, want its 1", n)
				}
			}
			// The two run policies read the same concept: each makes a handful of
			// reads, and neither goes through the days between.
			if reads := len(recorder.candidateReads()); reads > tc.maxReads {
				t.Fatalf("%d candidate reads, want at most %d: the walk went through the empty days", reads, tc.maxReads)
			}
		})
	}
}

// TestRetentionSweepRunsTheOperationalHalfWhenTheJournalHalfFails: the journal
// half (model-call and observation detail) failing must not zero the night.
// Every policy of the operational half still runs, and the error that comes back
// carries both halves'.
func TestRetentionSweepRunsTheOperationalHalfWhenTheJournalHalfFails(t *testing.T) {
	t.Run("a journal failure leaves the operational half to retire its candidates", func(t *testing.T) {
		db, i := sweepDB(t)
		ctx := retentionOwner()
		now := time.Now().UTC().Truncate(time.Second)
		i.SetNow(func() time.Time { return now })
		t.Setenv(EnvArchiveContainer, "test-retention")
		t.Setenv(EnvSystemRunRetentionDays, "")
		t.Setenv(EnvModelCallRetentionDays, "")
		archive := &verifyingArchive{}
		i.SetArchiver(archive)
		// The journal half folds a run's summary onto the run before it deletes
		// that run's detail, and the fold is refused.
		errSummary := errors.New("summary write failed")
		engine := newRecordingEngine()
		engine.refuse("updateWorkRun", errSummary)
		i.engine = engine

		// Sixty eligible runs. And one more, which is eligible but for a model call
		// of its own, past the model-call window: the journal half cannot fold it,
		// so the call stays, and the call keeps the run in the operational half's
		// own read.
		seedScheduledRuns(t, db, "free", now, 60, 40)
		held := seedScheduledRuns(t, db, "held", now, 1, 40)[0]
		insertSweepRow(t, db, modelCallConcept+":held-call", modelCallConcept, now.AddDate(0, 0, -200), map[string]any{"ownerUserId": "", "runId": held})

		result, err := i.RetentionSweep(ctx, false)
		if err == nil || !errors.Is(err, errSummary) || !strings.Contains(err.Error(), "summary fold failed") {
			t.Fatalf("the error is %v, want the journal half's failure in it", err)
		}
		if len(engine.callsTo("updateWorkRun")) == 0 {
			t.Fatal("the journal half never tried to fold a summary; it failed some other way")
		}
		r := resultFor(t, result.Operational, runConcept, EnvSystemRunRetentionDays)
		if r.Candidates != 60 || r.DeletedVersions != 60*versionsPerScheduledRun {
			t.Fatalf("system-run policy %+v, want the sixty free runs retired and the held one not a candidate", r)
		}
		if result.RowsDeleted != 60*versionsPerScheduledRun {
			t.Fatalf("%d versions deleted in all, want the operational half's %d and none of the journal's", result.RowsDeleted, 60*versionsPerScheduledRun)
		}
		if n := storedVersionsLike(t, db, "v1:work:%:free-%"); n != 0 {
			t.Fatalf("%d versions of the free runs remain", n)
		}
		// The run, its step and its model call are all still there.
		if n := storedVersionsLike(t, db, "v1:work:%:held-%"); n != 2+2+1 {
			t.Fatalf("%d versions of the held run, its step and its model call remain, want 5", n)
		}
	})

	t.Run("both halves failing return both errors", func(t *testing.T) {
		db, i := sweepDB(t)
		ctx := retentionOwner()
		now := time.Now().UTC().Truncate(time.Second)
		i.SetNow(func() time.Time { return now })
		t.Setenv(EnvArchiveContainer, "test-retention")
		t.Setenv(EnvSystemRunRetentionDays, "")
		t.Setenv(EnvModelCallRetentionDays, "")
		i.engine = newRecordingEngine()
		i.SetArchiver(&verifyingArchive{sweepDBArchive: sweepDBArchive{fail: true}})
		seedScheduledRuns(t, db, "both", now, 3, 40)
		insertSweepRow(t, db, modelCallConcept+":both-call", modelCallConcept, now.AddDate(0, 0, -200), map[string]any{"ownerUserId": "", "runId": runConcept + ":gone"})

		_, err := i.RetentionSweep(ctx, false)
		joined, ok := err.(interface{ Unwrap() []error })
		if !ok || len(joined.Unwrap()) != 2 {
			t.Fatalf("the error is %v, want the two halves' errors joined", err)
		}
		if n := storedVersionsLike(t, db, "%:both-%"); n != 3*versionsPerScheduledRun+1 {
			t.Fatalf("%d versions remain, want all %d: no archive, no delete", n, 3*versionsPerScheduledRun+1)
		}
	})
}

// firstUploadRefused is an archive that refuses its first upload and keeps
// every later one: the first policy of a pass with candidates fails, and the
// policies after it find the archive working.
type firstUploadRefused struct {
	verifyingArchive
	refused bool
}

var errFirstUploadRefused = errors.New("test archive refused the first upload")

func (a *firstUploadRefused) Upload(ctx context.Context, c, o string, b []byte, kind string) (string, error) {
	if !a.refused {
		a.refused = true
		return "", errFirstUploadRefused
	}
	return a.verifyingArchive.Upload(ctx, c, o, b, kind)
}

// TestOperationalRetentionAPolicysErrorDoesNotEndThePass: one policy's error is
// that policy's, not the night's. The two run policies that filled production's
// disk come fifth and sixth of eight, so a transient failure in a policy before
// them used to cost exactly their rows the whole night. Here the system-run
// policy (fifth) fails on its archive; the pipelines run policy (seventh) still
// runs and retires its rows, every policy reports its row, and the error names
// the policy that failed.
func TestOperationalRetentionAPolicysErrorDoesNotEndThePass(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "")
	t.Setenv(EnvPipelinesRunRetentionDays, "")
	i.SetArchiver(&firstUploadRefused{})

	seedScheduledRuns(t, db, "refused", now, 20, 40)
	at := now.AddDate(0, 0, -40).Truncate(24 * time.Hour).Add(12 * time.Hour)
	for n := 1; n <= 5; n++ {
		seedPipelinesRun(t, db, fmt.Sprintf("after-%02d", n), pipelineOwner, "completed", at)
	}
	if failing, later := policyIndex(runConcept, EnvSystemRunRetentionDays), policyIndex(pipelinesRunConcept, EnvPipelinesRunRetentionDays); failing < 0 || later <= failing {
		t.Fatalf("the system-run policy (%d) must come before the pipelines run policy (%d) for this test to say anything", failing, later)
	}

	results, err := i.operationalRetention(ctx, false)
	if !errors.Is(err, errFirstUploadRefused) {
		t.Fatalf("the pass returned %v, want the system-run policy's archive failure", err)
	}
	if !strings.Contains(err.Error(), runConcept) || !strings.Contains(err.Error(), EnvSystemRunRetentionDays) {
		t.Errorf("the error %q does not name the policy that failed (%s on %s)", err, runConcept, EnvSystemRunRetentionDays)
	}
	if len(results) != len(operationalPolicies) {
		t.Fatalf("%d result rows, want one for each of the %d policies: a policy after the failure did not run", len(results), len(operationalPolicies))
	}
	if r := resultFor(t, results, runConcept, EnvSystemRunRetentionDays); r.DeletedVersions != 0 {
		t.Errorf("the failed policy deleted %d versions without an archive: %+v", r.DeletedVersions, r)
	}
	if r := resultFor(t, results, pipelinesRunConcept, EnvPipelinesRunRetentionDays); r.Candidates != 5 || r.DeletedVersions != 5*3 {
		t.Errorf("the pipelines run policy after the failure reported %+v, want its 5 runs retired (15 versions)", r)
	}
	if n := storedVersionsLike(t, db, "v1:work:%:refused-%"); n != 20*versionsPerScheduledRun {
		t.Errorf("%d versions of the failed policy's runs remain, want all %d: no archive, no delete", n, 20*versionsPerScheduledRun)
	}
	if n := storedVersionsLike(t, db, pipelinesRunConcept+":after-%"); n != 0 {
		t.Errorf("%d versions of the later policy's runs remain, want none", n)
	}
}
