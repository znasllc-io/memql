package work

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

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
	insertSweepRow(t, db, "v1:identity:auditEvent:old", "v1:identity:auditEvent", now.AddDate(0, 0, -366), map[string]any{"action": "test"})
	insertSweepRow(t, db, "v1:identity:auditEvent:recent", "v1:identity:auditEvent", now.AddDate(0, 0, -30), map[string]any{"action": "test"})
	insertSweepRow(t, db, "v1:business:product:keep", "v1:business:product", now.AddDate(0, 0, -900), map[string]any{"name": "business record"})
	// Operational safety evidence belonging to live work remains even past its age window.
	insertSweepRow(t, db, "v1:safety:classification:active", "v1:safety:classification", now.AddDate(0, 0, -100), map[string]any{"runId": runConcept + ":active"})
	insertSweepRow(t, db, "v1:safety:classification:old", "v1:safety:classification", now.AddDate(0, 0, -100), map[string]any{})
	dry, err := i.operationalRetention(ctx, true)
	if err != nil || len(dry) != 5 || len(archive.blobs) != 0 {
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
	for object, blob := range archive.blobs {
		if !strings.HasSuffix(object, fmt.Sprintf("/%x.ndjson.gz", sha256.Sum256(blob))) {
			t.Fatalf("archive filename cannot be independently verified with sha256sum: %s", object)
		}
		r, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(r)
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
	if err != nil || windows["v1:identity:auditEvent"] != 730 || windows["v1:worker:invocation"] != 180 {
		t.Fatalf("stored policy lost: %v %v", windows, err)
	}
	t.Setenv("MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS", "800")
	windows, err = i.operationalRetentionWindows(ctx)
	if err != nil || windows["v1:identity:auditEvent"] != 800 {
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
	if _, err := db.ExecContext(ctx, `ALTER TABLE pg_temp.node_vectors RENAME COLUMN id TO broken_id`); err != nil {
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
