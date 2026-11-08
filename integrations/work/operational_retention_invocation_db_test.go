package work

import (
	"context"
	"testing"
	"time"
)

// operational_retention_invocation_db_test.go -- a soft-deleted worker
// invocation is archived and deleted a day after its tombstone
// (EnvWorkerInvocationDeletedRetentionDays), not 90 days after it.
//
// workerInvocationRetentionSweep (dsl/worker) writes `deleted: true` on a row
// once it is older than the invocation window, which appends a version. Before
// 2026-11-08 the only physical policy judged the row by that NEWEST version, so
// the tombstone restarted its 90 days (a row lived about 180), and a row that
// never reached a terminal outcome was never deleted.
func TestOperationalRetentionDBRetiresASoftDeletedInvocationADayAfterItsTombstone(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	archive := &verifyingArchive{}
	i.SetArchiver(archive)

	const invocation = "v1:worker:invocation"
	day := 24 * time.Hour
	type version struct {
		age     time.Duration
		outcome string
		deleted bool
	}
	rows := map[string][]version{
		// Hidden at day 90 the night before last: its tombstone is 26 hours old.
		"tombstoned-yesterday": {{91 * day, "success", false}, {26 * time.Hour, "success", true}},
		// Never reached a terminal outcome, which the 90-day policy does not
		// select: before this policy it was kept forever.
		"tombstoned-unfinished": {{100 * day, "", false}, {30 * time.Hour, "", true}},
		// Hidden two hours ago: inside its day of grace.
		"tombstoned-today": {{91 * day, "failure", false}, {2 * time.Hour, "failure", true}},
		// Past the window with no tombstone yet: the 90-day policy's.
		"expired-unhidden": {{95 * day, "success", false}},
		"recent":           {{10 * day, "success", false}},
	}
	for name, versions := range rows {
		for _, v := range versions {
			fields := map[string]any{"action": "exec", "outcome": v.outcome, "ownerUserId": "person"}
			if v.deleted {
				fields["deleted"] = true
			}
			insertSweepRow(t, db, invocation+":"+name, invocation, now.Add(-v.age), fields)
		}
	}

	result, err := i.operationalRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	deletedBy := map[string]int{}
	for _, r := range result {
		if r.Concept == invocation {
			deletedBy[r.Env] += r.DeletedVersions
		}
	}
	// Two tombstoned rows of two versions each go on the tombstone's window;
	// the untombstoned expired row's single version goes on the 90-day one.
	if deletedBy[EnvWorkerInvocationDeletedRetentionDays] != 4 || deletedBy["MEMQL_WORKER_INVOCATION_RETENTION_DAYS"] != 1 {
		t.Fatalf("deleted versions by window = %v, want 4 on %s and 1 on the 90-day window", deletedBy, EnvWorkerInvocationDeletedRetentionDays)
	}
	for name, want := range map[string]int{
		"tombstoned-yesterday": 0, "tombstoned-unfinished": 0, "expired-unhidden": 0,
		"tombstoned-today": 2, "recent": 1,
	} {
		var got int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id = ?`, invocation+":"+name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s keeps %d version(s), want %d", name, got, want)
		}
	}
	if len(archive.objects) == 0 {
		t.Fatal("rows were deleted with nothing archived")
	}
}
