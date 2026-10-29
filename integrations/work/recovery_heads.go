package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/memql"
)

// Refresh and read share one snapshot. An event committed while this reader is
// running stays queued for the next reader, just as a normal SELECT sees only
// commits before its snapshot. A backlog commits bounded progress and refuses
// to describe an incomplete projection as an empty set of waiting work.
func (i *Integration) runsInFlight(ctx context.Context) ([]map[string]any, error) {
	if i.admitRow == nil {
		return nil, fmt.Errorf("work: no row-admission gate is wired")
	}
	if i.bunDB == nil || i.bunDB() == nil {
		return nil, fmt.Errorf("work: recovery needs a database handle")
	}
	var progress []byte
	for attempt := 0; attempt < 4; attempt++ {
		var rows []map[string]any
		var state struct {
			Ready bool `json:"ready"`
		}
		err := i.bunDB().RunInTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(ctx context.Context, tx bun.Tx) error {
			if err := tx.QueryRowContext(ctx, `SELECT refresh_work_run_heads(1000)`).Scan(&progress); err != nil {
				return err
			}
			if err := json.Unmarshal(progress, &state); err != nil {
				return err
			}
			if !state.Ready {
				return nil // Commit the batch before reporting that recovery is not ready.
			}
			var err error
			rows, err = i.selectAdmittedFrom(ctx, tx, runConcept, runsInFlightSQL, sweepPageSize, runConcept)
			return err
		})
		if err != nil {
			var pgerr pgdriver.Error
			if errors.As(err, &pgerr) && pgerr.Field('C') == "40001" {
				continue // A fresh transaction, never a statement in the aborted one.
			}
			return nil, fmt.Errorf("work: current-run recovery projection: %w", err)
		}
		if state.Ready {
			return rows, nil
		}
	}
	return nil, fmt.Errorf("work: current-run recovery projection is catching up; retry the sweep: %s", progress)
}

// THE KILL SWITCH READS THE SAME PROJECTION (2026-10-29).
//
// "This owner's runs executing right now" was the DSL query
// workRunningRunsForOwner, and no index bounds that question over the run
// history: running is a property of a run's LATEST version, every run passes
// through it, and an index holds every version. The engine answered it from
// the owner's whole history -- the owner lookup's range with a heap fetch per
// version, then every latest payload -- and production's cluster owner, who
// owns every pipeline run, hit the eight-second statement limit (SQLSTATE
// 57014). The heads table holds only each run's current state, which is why
// recovery moved onto it; this reads it for one owner.
//
// It reads WITHOUT refreshing. refresh_work_run_heads claims the projector's
// state row with NOWAIT, so a kill switch that refreshed would fail whenever a
// sweep on another replica was projecting. The candidates are instead the
// projected heads at `running` plus every run written since its head was
// projected (work_run_head_events), and the latest version of each, read in
// the same statement, decides. A run that started, re-ran or finished after
// the last projection is a candidate through its event, so the answer is the
// one a refreshed projection would give; the cost is the runs in flight plus
// the projector's backlog, never the history.
//
// staged-data: MUST-NOT-GATE -- the kill switch must stop a staged run that
// dispatched to the owner's machine as readily as a visible one; withholding
// it would leave running the work the owner just switched off. Every row
// still passes row admission under the owner's borrowed authority, and the
// readiness query reads only the projection's bookkeeping and the trigger
// catalog.
const (
	runningRunsForOwnerSQL = `
WITH candidates AS MATERIALIZED (
    SELECT id FROM work_run_heads
    WHERE status NOT IN ('succeeded', 'failed', 'cancelled', 'abandoned') AND status = 'running'
    UNION
    SELECT id FROM work_run_head_events
)
SELECT n.id,n."createdAt",n.payload
FROM candidates c
JOIN LATERAL (
    SELECT id,"createdAt",payload FROM "MemoryNodes" n
    WHERE n.concept=? AND n.id=c.id
    ORDER BY n."createdAt" DESC LIMIT 1
) n ON true
WHERE n.payload->>'status' = 'running' AND n.payload->>'ownerUserId' IN (?)
ORDER BY n."createdAt" DESC, n.id
LIMIT ?
`
	// The conditions refresh_work_run_heads refuses on -- an unfinished
	// backfill, a capture trigger missing or disabled -- and the backlog,
	// counted no further than one past the cap.
	runningRunsReadySQL = `
SELECT s.backfill_complete,
    (SELECT count(*) FROM pg_trigger
     WHERE tgrelid='"MemoryNodes"'::regclass AND NOT tgisinternal AND tgenabled IN ('O','A')
       AND ((tgname IN ('work_run_head_insert','work_run_head_update','work_run_head_delete') AND tgfoid='enqueue_work_run_head()'::regprocedure)
         OR (tgname='work_run_head_reset' AND tgfoid='invalidate_work_run_heads()'::regprocedure))) = 4,
    (SELECT count(*) FROM (SELECT 1 FROM work_run_head_events LIMIT ?) q)
FROM work_run_head_state s WHERE s.singleton
`
)

// runningRunsBacklogCap is the deepest projector backlog the kill switch reads
// past. Every unprojected run is a candidate whose latest payload is read, so
// a deeper backlog is a stuck projector -- which the recovery sweep reports --
// and refusing is the honest answer. It is the most refresh_work_run_heads
// itself counts.
const runningRunsBacklogCap = 10000

// runningRunsLimit bounds the answer as workRunningRunsForOwner did: nothing in
// the product puts 500 of one person's runs in flight at once, and the
// pre-dispatch gate refuses a missed run's next machine call regardless.
const runningRunsLimit = 500

// runningRunsForOwner is one owner's runs executing right now, read through the
// recovery projection (runningRunsForOwnerSQL). ctx carries the owner's
// borrowed authority, and every row passes row admission under it.
//
// It REFUSES rather than answer from a projection that cannot answer -- a
// backfill not finished, a capture trigger missing or disabled, a backlog past
// runningRunsBacklogCap -- because an empty list would read as "nothing to
// stop". The readiness check and the read share one repeatable-read snapshot.
//
// The owner is matched in both spellings: rows store the canonical user id,
// and the kill switch is handed whatever the trigger or the caller carried.
func (i *Integration) runningRunsForOwner(ctx context.Context, owner string) ([]map[string]any, error) {
	db, err := i.gatedDB()
	if err != nil {
		return nil, err
	}
	bare := memql.BareShortId(owner)
	if bare == "" {
		return nil, fmt.Errorf("work: reading running runs needs their owner")
	}
	var rows []map[string]any
	err = db.RunInTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(ctx context.Context, tx bun.Tx) error {
		var complete, captured bool
		var pending int
		if err := tx.QueryRowContext(ctx, runningRunsReadySQL, runningRunsBacklogCap+1).Scan(&complete, &captured, &pending); err != nil {
			return err
		}
		switch {
		case !complete:
			return fmt.Errorf("its backfill has not finished")
		case !captured:
			return fmt.Errorf("its capture triggers are missing or disabled")
		case pending > runningRunsBacklogCap:
			return fmt.Errorf("it is more than %d runs behind", runningRunsBacklogCap)
		}
		var err error
		rows, err = i.selectAdmittedFrom(ctx, tx, runConcept, runningRunsForOwnerSQL,
			runConcept, bun.In([]string{bare, "v1:identity:user:" + bare}), runningRunsLimit)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("work: the current-run projection cannot say which runs are running: %w", err)
	}
	return rows, nil
}
