package work

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/workjournal"
)

// operational_retention_event_runs_db_test.go -- the system-run policy retires
// what the scheduler fired on an EVENT as it retires what it fired on a
// schedule (2026-11-06). Until then an event-fired run was never retired:
// about 5,300 a day on production, each with its steps.
//
// Postgres-gated (sweepDB): skips with no database, FAILS under
// MEMQL_REQUIRE_DB=1.

// seedFiredRun writes a finished, unowned, goalless run the scheduler fired on
// a schedule -- fields override any of that, a status included -- through
// seedWorkRun, and one step that is the run's own.
func seedFiredRun(t *testing.T, db *bun.DB, name string, at time.Time, fields map[string]any) string {
	t.Helper()
	base := map[string]any{"ownerUserId": "", "goalId": "", "automationName": "probe", "triggeredBy": "schedule"}
	maps.Copy(base, fields)
	status, _ := base["status"].(string)
	if status == "" {
		status = runStatusSucceeded
	}
	delete(base, "status")
	id := seedWorkRun(t, db, name, status, at, base)
	insertSweepRow(t, db, "v1:work:step:"+name, "v1:work:step", at.Add(-time.Minute),
		map[string]any{"ownerUserId": base["ownerUserId"], "runId": id, "status": "done"})
	return id
}

// storedVersions counts the versions stored under one id.
func storedVersions(t *testing.T, db *bun.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestOperationalRetentionRetiresEventFiredSystemRuns: a finished, unowned,
// goalless run the scheduler fired on an event goes with its steps, exactly as
// a scheduled one does -- and everything the policy kept before, it keeps.
func TestOperationalRetentionRetiresEventFiredSystemRuns(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(time.Second)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	t.Setenv(EnvSystemRunRetentionDays, "30")
	i.SetArchiver(&verifyingArchive{})
	old, recent := now.AddDate(0, 0, -40), now.AddDate(0, 0, -2)

	cases := map[string]struct {
		fields  map[string]any
		at      time.Time
		retired bool
	}{
		// The scheduler's two firings, finished and past the window.
		"scheduled":       {map[string]any{}, old, true},
		"event-graph":     {map[string]any{"triggeredBy": "event:graph.node.created.v1:accounts:account"}, old, true},
		"event-startup":   {map[string]any{"triggeredBy": "event:system.startup", "status": "failed"}, old, true},
		"event-cancelled": {map[string]any{"triggeredBy": "event:graph.node.updated.v1:accounts:account", "status": "cancelled"}, old, true},
		"event-abandoned": {map[string]any{"triggeredBy": "event:graph.node.updated.v1:accounts:account", "status": "abandoned"}, old, true},
		// The same conditions as before keep an event run.
		"event-running": {map[string]any{"triggeredBy": "event:system.startup", "status": "running"}, old, false},
		"event-waiting": {map[string]any{"triggeredBy": "event:system.startup", "status": "waiting"}, old, false},
		"event-owned":   {map[string]any{"triggeredBy": "event:system.startup", "ownerUserId": "person"}, old, false},
		"event-goal":    {map[string]any{"triggeredBy": "event:system.startup", "goalId": "v1:work:goal:g1"}, old, false},
		"event-recent":  {map[string]any{"triggeredBy": "event:system.startup"}, recent, false},
		// Triggers that are not the scheduler's stay out.
		"authored":  {map[string]any{"triggeredBy": "authored"}, old, false},
		"manual":    {map[string]any{"triggeredBy": "run:client"}, old, false},
		"journal":   {map[string]any{"triggeredBy": workjournal.TriggerPrefix + "analysis"}, old, false},
		"procedure": {map[string]any{"triggeredBy": ProcedureReplayTriggerPrefix + "shadow"}, old, false},
		// A child still holds an event run whole.
		"event-pending-approval": {map[string]any{"triggeredBy": "event:graph.node.created.v1:platform:inboundRequest"}, old, false},
		"event-model-call":       {map[string]any{"triggeredBy": "event:graph.node.created.v1:platform:inboundRequest"}, old, false},
	}
	stored := map[string]int{}
	for name, tc := range cases {
		stored[name] = storedVersions(t, db, seedFiredRun(t, db, name, tc.at, tc.fields))
	}
	insertSweepRow(t, db, "v1:work:approval:pending", "v1:work:approval", old,
		map[string]any{"ownerUserId": "", "runId": runConcept + ":event-pending-approval", "decision": ""})
	insertSweepRow(t, db, "v1:work:modelCall:held", modelCallConcept, old,
		map[string]any{"ownerUserId": "", "runId": runConcept + ":event-model-call"})

	if _, err := i.operationalRetention(ctx, false); err != nil {
		t.Fatal(err)
	}
	for name, tc := range cases {
		run, step := storedVersions(t, db, runConcept+":"+name), storedVersions(t, db, "v1:work:step:"+name)
		switch {
		case tc.retired && (run != 0 || step != 0):
			t.Errorf("%s: %d run and %d step versions left; a finished, unowned, goalless %v run past its window goes with its steps",
				name, run, step, tc.fields["triggeredBy"])
		case !tc.retired && (run != stored[name] || step != 1):
			t.Errorf("%s: %d run and %d step versions left, want the run kept whole (%d and 1)", name, run, step, stored[name])
		}
	}
}

// TestAChangedSystemRunPredicateWalksFromTheStart. Widening the predicate
// changed the policy's checkpoint key, so a checkpoint the old predicate left
// -- one that had walked past runs it did not select -- is not resumed from:
// the first pass under the new predicate walks from the oldest stored version,
// and the event runs behind the old checkpoint are retired rather than skipped.
// The superseded checkpoint row is left as it was.
func TestAChangedSystemRunPredicateWalksFromTheStart(t *testing.T) {
	db, i := sweepDB(t)
	ctx := retentionOwner()
	now := time.Now().UTC().Truncate(24 * time.Hour)
	i.SetNow(func() time.Time { return now })
	t.Setenv(EnvArchiveContainer, "test-retention")
	i.SetArchiver(&verifyingArchive{})

	current := operationalPolicies[policyIndex(runConcept, EnvSystemRunRetentionDays)]
	// The predicate this policy carried until 2026-11-06.
	before := current
	before.predicate = `n.payload->>'ownerUserId' = '' AND COALESCE(n.payload->>'goalId','') = '' AND n.payload->>'triggeredBy' = 'schedule' AND n.payload->>'status' IN ('succeeded','failed','cancelled','abandoned')`

	behind := seedFiredRun(t, db, "event-behind", now.AddDate(0, 0, -45), map[string]any{"triggeredBy": "event:system.startup"})

	// The old configuration's walk had passed day -35 when it was replaced.
	stale, err := i.loadRetentionProgress(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := now.AddDate(0, 0, -35)
	if err := i.saveRetentionProgress(ctx, stale, checkpoint, ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := i.loadRetentionProgress(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.key == stale.key {
		t.Fatalf("the checkpoint key %q ignores the predicate: the widened policy would resume past runs it never examined", fresh.key)
	}
	if fresh.at.Valid {
		t.Fatalf("the widened policy starts from a checkpoint at %s, want the oldest stored version", fresh.at.Time)
	}

	var result OperationalRetentionResult
	var tally retirementTally
	if _, err := i.retirePolicy(ctx, current, now.AddDate(0, 0, -30), false, i.clock(), time.Minute, &result, &tally); err != nil {
		t.Fatal(err)
	}
	if n := storedVersions(t, db, behind); n != 0 {
		t.Fatalf("the event run behind the old checkpoint kept %d versions: the first pass under the new predicate skipped it", n)
	}
	again, err := i.loadRetentionProgress(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	if !again.at.Valid || !again.at.Time.Equal(checkpoint) {
		t.Fatalf("the superseded checkpoint changed (%v), want it left inert at %s", again.at, checkpoint)
	}
}
