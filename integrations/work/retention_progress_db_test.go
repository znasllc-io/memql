package work

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestRetentionProgressResumesOnAnotherReplicaAndRevisitsProtectedRows(t *testing.T) {
	db, first := sweepDB(t)
	now := time.Now().UTC().Truncate(24 * time.Hour)
	ctx := retentionOwner()
	t.Setenv(EnvArchiveContainer, "test-retention")
	p := operationalPolicies[policyIndex(runConcept, EnvSystemRunRetentionDays)]
	for age := 45; age >= 40; age-- {
		insertSweepRow(t, db, fmt.Sprintf("v1:work:run:protected-%d", age), runConcept, now.AddDate(0, 0, -age), map[string]any{"ownerUserId": "alice", "status": "running"})
	}
	seedScheduledRuns(t, db, "resumed", now, 1, 39)
	recorder := &statementRecorder{}
	db.AddQueryHook(recorder)
	replica := func(i *Integration) {
		i.bunDB = func() *bun.DB { return db }
		i.admitRow = func(context.Context, memorynodes.MemoryNode) bool { return true }
		i.SetNow(func() time.Time { return now.Add(time.Duration(len(recorder.candidateReads())) * time.Second) })
		i.SetArchiver(&verifyingArchive{})
	}
	replica(first)
	var result OperationalRetentionResult
	var tally retirementTally
	spent, err := first.retirePolicy(ctx, p, now.AddDate(0, 0, -30), false, first.clock(), 2*time.Second, &result, &tally)
	if err != nil || !spent {
		t.Fatalf("first pass: spent=%v err=%v", spent, err)
	}
	reads := len(recorder.candidateReads())
	var before time.Time
	if err := db.QueryRowContext(ctx, `SELECT cursor_at FROM work_retention_progress`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	second := New(nil, nil) // another process knows no first-replica state
	replica(second)
	spent, err = second.retirePolicy(ctx, p, now.AddDate(0, 0, -30), false, second.clock(), time.Minute, &result, &tally)
	if err != nil || spent || tally.deleted != versionsPerScheduledRun {
		t.Fatalf("resumed: %+v spent=%v err=%v", tally, spent, err)
	}
	lo, _, ok := candidateWindow(t, recorder.candidateReads()[reads])
	if !ok || lo.Before(before) {
		t.Fatalf("re-walked protected history: first next=%s second start=%s", before, lo)
	}
	// A record becomes eligible behind the old cursor. A complete traversal
	// resets progress so the next cycle cannot lose it permanently.
	seedScheduledRuns(t, db, "late", now, 1, 44)
	tally = retirementTally{}
	_, err = second.retirePolicy(ctx, p, now.AddDate(0, 0, -30), false, second.clock(), time.Minute, &result, &tally)
	if err != nil || tally.deleted != versionsPerScheduledRun {
		t.Fatalf("late eligible history lost: %+v %v", tally, err)
	}
}

func TestRetentionProgressRefusesStaleReplicaAndDryRunLeavesProgress(t *testing.T) {
	_, i := sweepDB(t)
	ctx := retentionOwner()
	p := operationalPolicies[0]
	a, err := i.loadRetentionProgress(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := i.loadRetentionProgress(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-400 * 24 * time.Hour)
	if err := i.saveRetentionProgress(ctx, a, at, "last"); err != nil {
		t.Fatal(err)
	}
	if err := i.saveRetentionProgress(ctx, b, at.Add(-24*time.Hour), ""); err == nil {
		t.Fatal("stale replica rewound committed progress")
	}
	var result OperationalRetentionResult
	var tally retirementTally
	if _, err := i.retirePolicy(ctx, p, time.Now(), true, time.Now(), time.Minute, &result, &tally); err != nil {
		t.Fatal(err)
	}
	c, err := i.loadRetentionProgress(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.at.Time.Equal(at.Truncate(time.Microsecond)) || c.id != "last" || c.generation != a.generation {
		t.Fatalf("dry run changed progress: %+v", c)
	}
}

func TestJournalRetentionReadsOnlyBoundedLatestDayPages(t *testing.T) {
	db, i := sweepDB(t)
	now := time.Now().UTC().Truncate(24 * time.Hour)
	for n := 0; n < 250; n++ {
		at := now.AddDate(0, 0, -40-n%5).Add(time.Duration(n) * time.Second)
		insertSweepRow(t, db, fmt.Sprintf("v1:work:modelCall:window-%d", n), modelCallConcept, at, nil)
	}
	recorder := &statementRecorder{}
	db.AddQueryHook(recorder)
	rows, err := i.expiredJournalRows(retentionOwner(), modelCallConcept, now.AddDate(0, 0, -30))
	if err != nil || len(rows) != 250 {
		t.Fatalf("journal rows=%d err=%v", len(rows), err)
	}
	reads := recorder.candidateReads()
	if len(reads) < 5 {
		t.Fatalf("journal did not walk day windows: %v", recorder.sqls)
	}
	for _, q := range reads {
		lo, hi, ok := candidateWindow(t, q)
		if !ok || hi.Sub(lo) > 24*time.Hour {
			t.Fatalf("unbounded journal read: %s", q)
		}
	}
}
