package work

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE KILL SWITCH'S SELECTION COSTS WHAT IS IN FLIGHT, NOT THE HISTORY
// (2026-10-29). The owner below owns a finished history of 400 runs of six
// TOASTed versions each -- production's cluster owner owns every pipeline run
// -- and the DSL query this replaces read all of it to find the runs executing
// now. The projection read must find exactly those, including runs that
// changed after the last projection, which only its pending events name.
func TestRunningRunsForOwnerReadsTheProjectionNotTheHistory(t *testing.T) {
	db, i := sweepDB(t)
	ctx := context.Background()
	const owner = "v1:identity:user:kill-switch-owner"
	if _, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",type,schema,payload)
        SELECT 'v1:work:run:history-'||n,'v1:work:run',TIMESTAMPTZ '2026-01-01'+n*interval '1 minute'+v*interval '1 second','sweep-test','object','{}',
          jsonb_build_object('ownerUserId',?::text,'status',CASE WHEN v<5 THEN 'running' ELSE 'succeeded' END,
            'spine',jsonb_build_object('closure',(SELECT string_agg(md5(n||'-'||v||'-'||g),'') FROM generate_series(1,96) g)))
        FROM generate_series(1,400) n CROSS JOIN generate_series(0,5) v`, owner); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	run := func(id, who, status string, offset time.Duration) {
		insertSweepRow(t, db, "v1:work:run:"+id, runConcept, at.Add(offset), map[string]any{"ownerUserId": who, "status": status})
	}
	run("projected", owner, runStatusRunning, 0)
	run("stranger", "v1:identity:user:someone-else", runStatusRunning, 0)
	run("finished-later", owner, runStatusRunning, 0)
	run("rerun", owner, "succeeded", 0)
	catchUpWorkHeads(t, db)
	// Written after the projection: only work_run_head_events names them.
	run("finished-later", owner, "succeeded", time.Minute)
	run("rerun", owner, runStatusRunning, 2*time.Minute)
	run("started-later", owner, runStatusRunning, 3*time.Minute)

	ids := func(rows []map[string]any) []string {
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, rowString(row, "id"))
		}
		return out
	}
	want := []string{"v1:work:run:started-later", "v1:work:run:rerun", "v1:work:run:projected"}
	for _, named := range []string{owner, "kill-switch-owner"} {
		rows, err := i.runningRunsForOwner(ownerActor(ctx, named), named)
		if err != nil {
			t.Fatalf("running runs for %s: %v", named, err)
		}
		if got := ids(rows); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("running runs for %s = %v, want %v: the owner's runs whose LATEST version is running, "+
				"newest first, including the ones only a pending event names", named, got, want)
		}
	}

	var raw []byte
	if err := db.QueryRowContext(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+runningRunsForOwnerSQL,
		runConcept, bun.In([]string{"kill-switch-owner", owner}), runningRunsLimit).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []map[string]any
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	plan := plans[0]["Plan"].(map[string]any)
	hit, _ := plan["Shared Hit Blocks"].(float64)
	read, _ := plan["Shared Read Blocks"].(float64)
	t.Logf("running runs touched %.0f buffers over a %d-version history", hit+read, 400*6)
	if hit+read > 500 {
		t.Fatalf("the read touched %.0f buffers; it must cost the runs in flight, not the history: %s", hit+read, raw)
	}

	// Every row passes admission under the owner's actor, as fetched.
	i.admitRow = func(_ context.Context, n memorynodes.MemoryNode) bool { return n.ID != "v1:work:run:rerun" }
	rows, err := i.runningRunsForOwner(ownerActor(ctx, owner), owner)
	if err != nil || strings.Join(ids(rows), ",") != "v1:work:run:started-later,v1:work:run:projected" {
		t.Fatalf("a row refused admission was returned, or the read failed: %v %v", ids(rows), err)
	}
	i.admitRow = func(context.Context, memorynodes.MemoryNode) bool { return true }

	// It refuses rather than answer from a projection that cannot answer.
	refused := func(why string) {
		t.Helper()
		if rows, err := i.runningRunsForOwner(ownerActor(ctx, owner), owner); err == nil {
			t.Fatalf("%s: the read answered %v; an empty or partial list would read as \"nothing to stop\"", why, ids(rows))
		}
	}
	exec := func(stmt string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	exec(`ALTER TABLE "MemoryNodes" DISABLE TRIGGER work_run_head_insert`)
	refused("a capture trigger is disabled")
	exec(`ALTER TABLE "MemoryNodes" ENABLE TRIGGER work_run_head_insert`)
	exec(`INSERT INTO work_run_head_events (id) SELECT 'v1:work:run:backlog-'||n FROM generate_series(1,10001) n`)
	refused("the projector is more than the cap behind")
	exec(`TRUNCATE work_run_heads`)
	refused("the projection was reset and its backfill has not finished")
	catchUpWorkHeads(t, db)
	if rows, err := i.runningRunsForOwner(ownerActor(ctx, owner), owner); err != nil || len(rows) != len(want) {
		t.Fatalf("after the backfill the read must answer again: %v %v", ids(rows), err)
	}
}
