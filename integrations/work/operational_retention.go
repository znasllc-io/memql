package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

const EnvSystemRunRetentionDays = "MEMQL_WORK_SYSTEM_RUN_RETENTION_DAYS"
const DefaultSystemRunRetentionDays = 30

// EnvPipelinesRunRetentionDays is how long a finished pipeline run is kept
// (epic memql#5478, #5496): its v1:pipelines:run row, the v1:work:run it
// compiled into with that run's steps, and the v1:work:goal behind them once
// no run of it remains. One window for all three, so an unblocked run's rows
// leave in the same sweep; a work run something keeps (detail on a longer
// window, a child that is not its own) stays behind, and its goal with it,
// until that clears. The Library files a run's steps archived -- logs,
// artifacts -- are the owner's, and no policy here names them.
const EnvPipelinesRunRetentionDays = "MEMQL_PIPELINES_RUN_RETENTION_DAYS"
const DefaultPipelinesRunRetentionDays = 30

// pipelinesRunConcept is spelled here, as integrations/pipelinesteps spells it
// for itself: component/pipelinerun owns the name and this module does not
// depend on it. TestRetentionPredicatesSelectOnlyWhatTheirConceptsDeclare reads
// the concept out of the DSL, so a rename fails there instead of retiring
// nothing.
const pipelinesRunConcept = "v1:pipelines:run"

const retirementBatchSize = 100
const retirementMaxVersions = 10000
const retirementMaxBytes = 32 << 20

const (
	// retentionCandidateWindow is the createdAt span one candidate read covers.
	// Candidates are read a day at a time, oldest day first, so the statement's
	// work is a day of a concept's rows however long its history is.
	retentionCandidateWindow = 24 * time.Hour

	// retentionCandidateLimit is the most candidates one read returns, and it is
	// one retirementBatchSize: a read is a batch, so a page that the archive
	// refuses or the guards keep costs one batch and nothing behind it.
	retentionCandidateLimit = 100

	// retentionRunBudget is how long a pass spends on candidates before it stops
	// with work left, shared out among the policies (retentionShare). The pass
	// is a cron-leader job and holds nothing between nights: the next one starts
	// again at the oldest version stored. See OPERATIONAL RETENTION WALKS DAY
	// WINDOWS, on operationalRetention.
	retentionRunBudget = 4 * time.Minute
)

// retentionShare is what a policy may spend of the pass's budget: the time left
// divided by the policies still to run, itself included. A policy that uses its
// share leaves every later one the same; one that uses less leaves them more; and
// the last takes what is left. So an early policy with a backlog cannot starve the
// ones after it, which is what one budget for the whole pass let it do. No time
// left, or no policy to run, is no share.
func retentionShare(left time.Duration, policies int) time.Duration {
	if policies < 1 || left <= 0 {
		return 0
	}
	return left / time.Duration(policies)
}

// Age never outranks a live parent. Check the latest run via its indexed id,
// so a previous terminal version cannot make current work look disposable.
// staged-data: MUST-NOT-GATE -- hiding a staged active parent makes its detail
// appear orphaned and permits retirement while that parent is still running.
const retentionInactiveParentSQL = `NOT EXISTS (SELECT 1 FROM "MemoryNodes" parent WHERE parent.concept='v1:work:run' AND parent.id=n.payload->>'runId' AND COALESCE(parent.payload->>'status','') NOT IN ('succeeded','failed','cancelled','abandoned') AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" newerParent WHERE newerParent.concept=parent.concept AND newerParent.id=parent.id AND newerParent."createdAt">parent."createdAt"))`

// This is a closed list of operational records, never a default lifetime for
// arbitrary concepts. Business records, goals (but a pipeline's, below),
// templates and active work are not eligible. A terminal run owns its
// step/approval archive as a unit: the children that are the run's own
// (retireVerified says which).
//
// A WINDOW IS KEYED BY ITS ENV NAME, NEVER BY ITS CONCEPT. Two policies read
// v1:work:run, and they never select the same run: a system run is triggered
// by `schedule`, a pipeline's by `pipeline:<mode>` (pipelines.WorkTriggerPrefix).
// And one window, MEMQL_PIPELINES_RUN_RETENTION_DAYS, governs three concepts.
//
// A FINISHED PIPELINE RUN (#5496) is its v1:pipelines:run row once it is
// `completed` -- the conclusion is not the test, a refused run is completed
// too -- and the work run it compiled into once component/workjournal closed
// it succeeded, failed or cancelled. Nothing closes a pipeline's work run
// `abandoned` (the waiting sweep reads past it: runnerOwnsRecovery), so that
// status is not one this list was told about and such a run is kept.
//
// A PIPELINE'S GOAL is the v1:work:goal the driver opens for a run key
// (requestedVia `pipeline`, origin `system`), retired once NO v1:work:run of it
// remains -- of any version, naming it in either spelling (goalRefSQL). A
// re-run is a second run of the same goal, so the goal outlives each run and
// goes with the last.
//
// THE ORDER BELOW IS WHAT LETS ONE SWEEP FINISH A RUN, and it holds for a run
// nothing keeps: the work run with its steps, then the run row, then the goal,
// each policy finding what the one before it left. A work run something keeps
// holds its goal in place too, so a goal never goes ahead of its runs; the run
// row is kept or retired on its own age.
type operationalPolicy struct {
	concept, env string
	days         int
	predicate    string
}

var operationalPolicies = []operationalPolicy{
	{"v1:identity:auditEvent", "MEMQL_IDENTITY_AUDIT_LOG_RETENTION_DAYS", 365, "TRUE"},
	{"v1:safety:classification", "MEMQL_SAFETY_CLASSIFICATION_RETENTION_DAYS", 90, "TRUE"},
	{"v1:safety:outputScreening", "MEMQL_SAFETY_OUTPUT_SCREENING_RETENTION_DAYS", 90, "TRUE"},
	{"v1:worker:invocation", "MEMQL_WORKER_INVOCATION_RETENTION_DAYS", 90, `COALESCE(n.payload->>'outcome','') IN ('success','failure','cancelled','timeout','denied_by_scope','denied_by_policy','denied_by_classifier','kill_switch_engaged','no_worker_available','rerouted')`},
	{runConcept, EnvSystemRunRetentionDays, DefaultSystemRunRetentionDays, `n.payload->>'ownerUserId' = '' AND COALESCE(n.payload->>'goalId','') = '' AND n.payload->>'triggeredBy' = 'schedule' AND n.payload->>'status' IN ('succeeded','failed','cancelled','abandoned')`},
	{runConcept, EnvPipelinesRunRetentionDays, DefaultPipelinesRunRetentionDays, `n.payload->>'triggeredBy' LIKE '` + pipelines.WorkTriggerPrefix + `%' AND n.payload->>'status' IN ('succeeded','failed','cancelled')`},
	{pipelinesRunConcept, EnvPipelinesRunRetentionDays, DefaultPipelinesRunRetentionDays, `n.payload->>'status' = 'completed'`},
	// After the work-run policy, always: the "no run remains" term is added in
	// retentionCandidateRead and checked again in retirementDelete. That term is
	// the one cost the day window does not bound -- it compares every run version
	// with the goal, 0.34 s a read warm and 1.26 s cold at 600,000 run versions --
	// and is ACCEPTED for this release; the partial expression index on the run's
	// goalId that would bound it is deferred to release 2 (see operationalRetention).
	{goalConcept, EnvPipelinesRunRetentionDays, DefaultPipelinesRunRetentionDays, `n.payload->>'requestedVia' = 'pipeline' AND n.payload->>'origin' = 'system'`},
}

// goalRefSQL folds a SQL expression naming a v1:work:goal to its bare id.
//
// A run's goalId is an outgoing @relationship, so the write path stores it
// canonical ("v1:work:goal:<id>"), and the goal row's own id is canonical too.
// A row written before the relationship existed, or by hand, may hold it bare.
// "No run of this goal remains" must see a run naming the goal in EITHER
// spelling: a run it missed would be a goal deleted under a run that still
// names it. Both sides go through the fold, so the comparison stays one
// equality the planner can hash.
func goalRefSQL(expr string) string {
	return `regexp_replace(` + expr + `, '^v1:work:goal:', '')`
}

type OperationalRetentionResult struct {
	Concept          string   `json:"concept"`
	Env              string   `json:"env"`
	RetentionDays    int      `json:"retentionDays"`
	Candidates       int      `json:"candidates"`
	ArchivedVersions int      `json:"archivedVersions"`
	DeletedVersions  int      `json:"deletedVersions"`
	Objects          []string `json:"objects,omitempty"`
	Oversized        []string `json:"oversized,omitempty"`
	// BudgetExhausted says this policy ran out of its share of retentionRunBudget
	// (retentionShare) with versions stored in day windows it had not read, so
	// candidates may remain. They are not lost: the next pass starts again at the
	// oldest version stored. A policy that read every window up to its cutoff, or
	// had nothing stored before it, leaves it unset however much of its share it
	// used.
	BudgetExhausted bool `json:"budgetExhausted,omitempty"`
}

// workChildConceptsSQL lists what a run owns and takes with it.
const workChildConceptsSQL = `('v1:work:step','v1:work:approval','v1:work:modelCall','v1:work:observation')`

// operationalRetention runs every policy on operationalPolicies, one after the
// other, and says what each did.
//
// ===========================================================================
// OPERATIONAL RETENTION WALKS DAY WINDOWS (storage growth stability, 2026-10-11)
// ===========================================================================
// On the production instance that filled its disk this pass retired nothing on
// 2026-10-10 or 2026-10-11, and said so only in the log:
//
//	03:40 Oct 10  {v1:work:run 30 1045 0 0 []}  then  canceling statement due to statement timeout
//	03:40 Oct 11  work: read v1:work:run: ERROR: canceling statement due to statement timeout
//
// The first night FOUND 1,045 candidates and then a statement inside
// retireVerified timed out, so nothing was archived or deleted; the second timed
// out reading the candidates. The table held about 750,000 v1:work:run versions
// and 1,000,000 v1:work:step versions. The request pool cancels a statement at 8 s
// (pgdriver stops waiting at 10 s), and decision 3 of
// docs/superpowers/specs/2026-10-11-storage-growth-stability-design.md is that
// no statement of a retention pass comes near that, however much the table holds.
//
// WHAT A POLICY DOES. It asks for the oldest version of its concept before its
// cutoff (oldestRetentionVersion), then reads the candidates one UTC day window
// [day, day+24h) at a time, oldest first, up to the cutoff, a page of
// retentionCandidateLimit at a time (retentionCandidateRead), and retires each
// page as one batch through retireInBatches, unchanged. The predicates, the
// archive-before-delete rule and the guards inside retireVerified are what they
// were: a window only chooses which candidates are looked at, and when.
//
// A page is the next hundred candidates AFTER THE LAST ROW THE PREVIOUS PAGE WENT
// THROUGH (pageEnd), so no candidate is read twice in a pass whatever becomes of
// it: a batch the guard keeps, an oversized record, a refused row and a dry run
// all leave the rows where they are, and a window read again from its start would
// return the same ones until the policy's time ran out.
//
// THE WALK SKIPS WHAT IS EMPTY. It starts at the oldest version STORED, to which
// no predicate has been applied, so rows no policy retires -- a person's run, an
// old version of live work, a goal that is kept -- anchor it, and a far-past row
// (1970, year 1) is thousands to hundreds of thousands of windows with nothing in
// them, every night, for every policy. When a window has no candidate, one cheap
// look (retentionWindowHasVersions) says whether the concept has any version
// stored in it; if it has none, one bounded probe (oldestRetentionVersion with a
// lower bound) finds the next version stored before the cutoff and the walk goes on
// from its day. A window that holds versions no policy retires is read, and
// stepped past, the ordinary way.
//
// THE BUDGET IS SHARED. When a policy's turn comes it may spend the time left in
// the pass over the policies still to run, itself included (retentionShare), and
// no more: time an earlier policy did not use is the later ones', and a policy with
// a backlog cannot starve the ones after it, as one budget for the whole pass let
// it (the run policies come fifth and sixth). The share is checked before every
// read, strictly greater, so a policy overruns by at most the batch it is in the
// middle of. When it has gone the policy reports BudgetExhausted only if something
// is left to read -- a full page just came back, or a version is stored from the
// next window on -- so one that spent its share on its last window is not reported
// behind. Nothing is stored between nights: the next pass starts again at the
// oldest version stored.
//
// ACCEPTED RESIDUAL COST. The rows a walk starts from and passes over that no
// policy retires are still walked every night: each window that holds them is read,
// finds no candidate, and is stepped past. Measured at 5,000 runs a day, that is
// 21 ms a window (0.8 s for the 40 dense days of the history below), so roughly 10
// to 30 s per year of installation age, per policy, on top of the work itself. The
// skip-ahead removes only the windows with nothing stored in them. A persisted
// low-water mark, or an index on (concept, "createdAt"), would remove the rest;
// both are deferred to release 2.
//
// MEASURED, on a seeded history in the CI database image (PostgreSQL 16.14,
// TimescaleDB 2.29.0): "MemoryNodes" as a hypertable in 7-day chunks with the
// production indexes, 200,000 runs of 3 versions and 800,000 step versions over
// 40 days (5,000 runs a day, 90% idle schedule ticks), a goal and a
// v1:pipelines:run row of 3 versions for each of 8,000 pipeline runs, and 2.6
// million narrow status versions: 4.05 million rows, 7.0 GB. The pass's connection
// carried the production statement_timeout (8 s) and pgdriver's read deadline
// (10 s). The machine is faster and warmer than production, so the figures are
// the ratios and the headroom, not what an instance will see. One real pass,
// system-run window 30 days, 41,191 eligible system runs and 1,828 pipeline runs
// older than the cutoff:
//
//	                             before                       after
//	pass                         10 m 50 s, 21,832 runs       1 m 04 s to 1 m 08 s, all 43,019
//	                             (the 20,000 cap, and the     runs, their goals and pipelines:run
//	                             rest tomorrow)               rows; 47 to 50 s of it the
//	                                                          system-run policy's, of its 60 s
//	candidate read               one read of 20,000: 2.3 s    a page: p50 17 ms, p95 35 ms
//	                             with the rows transferred    (512 reads); worst 0.5 to 1.0 s,
//	                                                          and 1.26 s on cold buffers, all
//	                                                          the goal policy's
//	oldest-version probe         (none)                       p50 6 ms, max 69 ms (8 calls); the
//	                                                          bounded ones 10 to 23 ms (4 calls)
//	load of a batch of 100 runs  p50 2.2 s, p95 2.9 s,        parents p50 9 ms, max 33 ms;
//	(700 versions)               max 3.5 s (257 batches);     children p50 9 ms, max 22 ms
//	                             over data just written:
//	                             p50 3.5 s, and one of 64
//	                             cancelled at 8 s
//	guarded DELETE of the batch  p50 0.31 s, max 0.48 s;      p50 45 to 47 ms, p95 60 to 131 ms,
//	                             1.4 s and 2.4 s for the      max 0.34 to 0.45 s
//	                             first two batches over
//	                             data just written
//
// The load and the delete's child guard are the two statements that were slow,
// and why is in retirementLoads. Nothing else in the pass is near a second. With
// a row dated 1970 and another dated year 1 added to that history, the walk before
// the skip-ahead spent all four minutes on 37,002 windows with nothing in them
// (p50 6 ms) and retired no run at all; with it the pass is the same 1 m 04 s to
// 1 m 08 s as without the two rows.
//
// WHAT IS NOT BOUNDED BY A DAY, and where it stands at that size:
//   - the oldest-version probe reads the concept's index entries up to the
//     cutoff once (69 ms at most for 600,000 run versions), and a bounded one
//     reads the entries after the window it jumps from, once for each empty
//     stretch;
//   - the goal policy's "no run of it remains", in its candidate read and in the
//     delete's guard (goalRefSQL), compares every run version's goalId with the
//     goal's, and no index serves it. MEASURED BOUND: 0.34 s a read warm and 1.26 s
//     the first time on cold buffers, for 600,000 run versions, whatever the
//     window; 14 to 15 s for the policy as a whole, 1,828 goals. ACCEPTED for this
//     release. The policy runs last, so a failure there costs nothing that went
//     before it, and the partial expression index on the run's goalId that would
//     bound it is deferred to release 2;
//   - the journal sweep's expired-rows read (expiredJournalRowsSQL in sweep.go)
//     is not part of this walk: it takes the latest version of every id of one
//     concept, 0.41 s for 400,000 model-call versions, and grows with them.
//     RetentionSweep no longer lets it stop this pass when it fails. Windowing it
//     is deferred to release 2.
//
// staged-data: MUST-NOT-GATE -- retention is physical retirement, including
// staged historical versions. Every fetched row still passes AdmitSourceRow.
func (i *Integration) operationalRetention(ctx context.Context, dry bool) ([]OperationalRetentionResult, error) {
	if err := requireClusterOwner(ctx); err != nil {
		return nil, err
	}
	var results []OperationalRetentionResult
	windows, err := i.operationalRetentionWindows(ctx)
	if err != nil {
		return nil, err
	}
	started := i.clock()
	for k, p := range operationalPolicies {
		days := windows[p.env]
		cutoff := i.clock().UTC().AddDate(0, 0, -days)
		// Each policy is given what is left of the pass's time over the policies
		// still to run (retentionShare), measured from when its turn comes. Time an
		// earlier policy did not use is therefore the later ones', and none can take
		// more than its share -- bar the batch it is in the middle of.
		share := retentionShare(retentionRunBudget-i.clock().Sub(started), len(operationalPolicies)-k)
		result := OperationalRetentionResult{Concept: p.concept, Env: p.env, RetentionDays: days}
		var tally retirementTally
		spent, policyErr := i.retirePolicy(ctx, p, cutoff, dry, i.clock(), share, &result, &tally)
		result.ArchivedVersions, result.DeletedVersions, result.Objects, result.Oversized = tally.archived, tally.deleted, tally.objects, tally.oversized
		result.BudgetExhausted = spent
		results = append(results, result)
		if policyErr != nil {
			return results, policyErr
		}
		if spent {
			i.log().Info("work: a retention policy spent its share of the pass's time budget with day windows unread; the next pass starts again at the oldest version stored",
				"component", "work.retention", "concept", p.concept, "env", p.env, "share", share.String(), "budget", retentionRunBudget.String(),
				"candidates", result.Candidates, "deleted", result.DeletedVersions)
		}
	}
	return results, nil
}

// retirePolicy walks one policy's candidates a day window at a time, oldest
// first, up to its cutoff, retiring each page it reads as a batch. It stops when
// the policy's share of the pass's time has gone (started is when its turn came)
// and returns true if that left day windows unread.
//
// A window is read in pages while the pages come back full, each page starting
// after the last row the one before went through (a refused row included, see
// pageEnd). Re-reading the window from its start instead would hand back the
// same candidates every time a page was kept -- by the guard, by the archive, or
// because a dry run deleted nothing -- until the budget ran out.
//
// THE WALK SKIPS WHAT IS EMPTY. It starts at the oldest version stored, retirable
// or not, so a row no policy retires (a person's run, an old version of live work)
// anchors it; one dated 1970, or year 1, would be thousands upon thousands of
// reads of nothing, every night, for every policy. When a window comes back with
// no candidate, a cheap look says whether the concept has any version stored in
// it; if not, one bounded probe finds the next version stored before the cutoff
// and the walk goes on from its day. A window that holds versions no policy
// retires is read, and stepped past, the ordinary way.
//
// THE SHARE IS CHECKED BEFORE EVERY READ. When it has gone, the policy says it
// stopped early only if something is left to read: a full page just came back
// (its window may hold more), or a version is stored from the next window on
// (the same probe). A policy whose share ran out on its last window has not
// stopped early.
//
// staged-data: MUST-NOT-GATE -- see operationalRetention; the reads below are
// retentionCandidateRead's, and every row they return passes AdmitSourceRow.
func (i *Integration) retirePolicy(ctx context.Context, p operationalPolicy, cutoff time.Time, dry bool, started time.Time, share time.Duration, result *OperationalRetentionResult, tally *retirementTally) (bool, error) {
	oldest, found, err := i.oldestRetentionVersion(ctx, p.concept, time.Time{}, cutoff)
	if err != nil || !found {
		return false, err
	}
	// Strictly greater: a policy that has used exactly its share goes on. A share
	// of nothing reads nothing.
	outOfTime := func() bool { return share <= 0 || i.clock().Sub(started) > share }

	lo := oldest.UTC().Truncate(retentionCandidateWindow)
	for lo.Before(cutoff) {
		hi := lo.Add(retentionCandidateWindow)
		if hi.After(cutoff) {
			hi = cutoff
		}
		next := lo.Add(retentionCandidateWindow)
		var after *pageEnd
		for {
			// Checked before every read rather than every window: a window of
			// thousands of candidates is dozens of pages, and the policy must not
			// run on for all of them once its share is gone.
			if outOfTime() {
				if after != nil {
					return true, nil
				}
				_, left, err := i.oldestRetentionVersion(ctx, p.concept, lo, cutoff)
				return left, err
			}
			query, params := retentionCandidateRead(p, cutoff, lo, hi, after)
			rows, page, err := i.selectAdmittedPage(ctx, p.concept, query, params...)
			if err != nil {
				return false, err
			}
			result.Candidates += len(rows)
			if err := i.retireInBatches(ctx, p.concept, rows, p.concept == runConcept, cutoff, dry, tally); err != nil {
				return false, err
			}
			if after == nil && page.scanned == 0 {
				stored, err := i.retentionWindowHasVersions(ctx, p.concept, lo, hi)
				if err != nil {
					return false, err
				}
				if !stored {
					if !hi.Before(cutoff) {
						return false, nil
					}
					ahead, more, err := i.oldestRetentionVersion(ctx, p.concept, hi, cutoff)
					if err != nil || !more {
						return false, err
					}
					next = ahead.UTC().Truncate(retentionCandidateWindow)
				}
			}
			if page.scanned < retentionCandidateLimit {
				break
			}
			after = &page
		}
		lo = next
	}
	return false, nil
}

// oldestRetentionVersion is the createdAt of the concept's oldest version stored
// in [from, cutoff) -- from the beginning when from is zero -- or found=false when
// there is none. It is where a policy's walk starts, and where it goes on from
// after an empty stretch, and what says a policy that ran out of time has left
// nothing unread. It is the one kind of statement in the walk that is not bounded
// to a day: an index-only scan of the concept's entries in range, which are the
// versions the walk is about to go through, once at the start and once more for
// each empty stretch it jumps.
//
// staged-data: MUST-NOT-GATE -- the walk has to start at the oldest version
// whatever its visibility. Gated, a staged row older than every other would
// never be reached and would outlive its window; and the read returns a
// timestamp, no row, so there is nothing to admit. Every candidate it leads to
// is read through selectAdmittedPage.
func (i *Integration) oldestRetentionVersion(ctx context.Context, concept string, from, cutoff time.Time) (time.Time, bool, error) {
	db, err := i.gatedDB()
	if err != nil {
		return time.Time{}, false, err
	}
	query, args := `SELECT min("createdAt") FROM "MemoryNodes" WHERE concept = ? AND "createdAt" < ?`, []any{concept, cutoff}
	if !from.IsZero() {
		query, args = `SELECT min("createdAt") FROM "MemoryNodes" WHERE concept = ? AND "createdAt" >= ? AND "createdAt" < ?`, []any{concept, from, cutoff}
	}
	var oldest sql.NullTime
	if err := db.QueryRowContext(ctx, query, args...).Scan(&oldest); err != nil {
		return time.Time{}, false, fmt.Errorf("work: read the oldest %s: %w", concept, err)
	}
	return oldest.Time, oldest.Valid, nil
}

// retentionWindowHasVersions says whether the concept has any version stored
// created in [lo, hi), retirable or not. It stops at the first it finds, so it
// costs a few index entries in a window with versions in it and a pass over the
// concept's entries in the window's chunk only in one with none -- which the
// walk then jumps.
//
// staged-data: MUST-NOT-GATE -- see oldestRetentionVersion: a staged version is
// a version, and a walk that did not count it would jump a window it must read.
// The read returns a boolean, no row.
func (i *Integration) retentionWindowHasVersions(ctx context.Context, concept string, lo, hi time.Time) (bool, error) {
	db, err := i.gatedDB()
	if err != nil {
		return false, err
	}
	var stored bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM "MemoryNodes" WHERE concept = ? AND "createdAt" >= ? AND "createdAt" < ?)`, concept, lo, hi).Scan(&stored); err != nil {
		return false, fmt.Errorf("work: read whether %s has versions in a window: %w", concept, err)
	}
	return stored, nil
}

// retentionCandidateRead is the read of one page of a policy's candidates: the
// latest versions of the concept created in [lo, hi), in (createdAt, id) order,
// the first retentionCandidateLimit of them past `after` when the window is
// being read in pages. The predicates are the policy's, unchanged.
//
// THE QUERY IS TWO LEVELS ON PURPOSE. The inner one selects the window's rows by
// the policy's own cheap predicate (a few JSON fields of the row itself) and
// sorts them. The outer one applies the tests that probe other rows -- is there
// a newer version, is a live run the parent, does a run keep a child, does a
// run of the goal remain -- as anti joins over the sorted rows, so the LIMIT
// ends them after the first retentionCandidateLimit rows that pass. Written as
// one level, the planner puts the sort on top and runs every probe for every
// row of the day first. Measured on a day window of 4,500 eligible runs: the
// one-level read took 135 to 159 ms once warm and 0.47 to 1.26 s the first time
// after the data was written, with 4,550 rows probed; this one takes 22 to 37 ms
// warm and 0.09 to 0.34 s the first time, probing about a hundred.
//
// staged-data: MUST-NOT-GATE -- see operationalRetention; every row this
// returns passes AdmitSourceRow in selectAdmittedPage.
func retentionCandidateRead(p operationalPolicy, cutoff, lo, hi time.Time, after *pageEnd) (string, []any) {
	from := lo
	paging := ""
	params := []any{p.concept}
	if after != nil {
		from = after.at
	}
	params = append(params, from, hi)
	if after != nil {
		paging = ` AND (n."createdAt",n.id) > (?,?)`
		params = append(params, after.at, after.id)
	}
	inner := `SELECT n.id,n.concept,n."createdAt",n.payload FROM "MemoryNodes" n WHERE n.concept=? AND n."createdAt" >= ? AND n."createdAt" < ?` + paging + ` AND (` + p.predicate + `) ORDER BY n."createdAt",n.id`

	probes := ""
	if p.concept == runConcept {
		// Apply child eligibility before LIMIT as well as after loading: a
		// large population awaiting longer detail retention cannot starve
		// later, eligible runs every night. The ownership term is
		// retireVerified's rule in SQL: a child that is not the run's own --
		// another owner's, or one carrying no owner -- keeps the run.
		probes += `NOT EXISTS (SELECT 1 FROM "MemoryNodes" child WHERE child.concept IN ` + workChildConceptsSQL + ` AND child.payload->>'runId'=n.id AND (child.concept IN ('v1:work:modelCall','v1:work:observation') OR child."createdAt">=? OR child.payload->>'ownerUserId' IS NULL OR child.payload->>'ownerUserId' IS DISTINCT FROM n.payload->>'ownerUserId' OR (child.concept='v1:work:approval' AND COALESCE(child.payload->>'decision','')='')) AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" newerChild WHERE newerChild.concept=child.concept AND newerChild.id=child.id AND newerChild."createdAt">child."createdAt")) AND `
		params = append(params, cutoff)
	}
	if p.concept == goalConcept {
		// A goal goes only once no run of it remains -- any version, either
		// spelling. Before LIMIT for the reason above: goals whose runs are
		// still kept must not crowd out the ones that could go. The delete
		// asks again (retireVerified), for a run opened since this read.
		//
		// This is the one probe here with no index to go to, so a read costs the
		// run table, not the window: 0.34 s warm, 1.26 s cold, at 600,000 run
		// versions. Accepted for this release; the partial expression index on
		// the run's goalId is deferred to release 2.
		probes += `NOT EXISTS (SELECT 1 FROM "MemoryNodes" r WHERE r.concept='v1:work:run' AND ` + goalRefSQL(`r.payload->>'goalId'`) + `=` + goalRefSQL(`n.id`) + `) AND `
	}
	query := `SELECT n.id,n."createdAt",n.payload FROM (` + inner + `) n WHERE ` + probes +
		`NOT EXISTS (SELECT 1 FROM "MemoryNodes" newer WHERE newer.concept=n.concept AND newer.id=n.id AND newer."createdAt">n."createdAt") AND ` +
		retentionInactiveParentSQL + ` ORDER BY n."createdAt",n.id LIMIT ` + strconv.Itoa(retentionCandidateLimit)
	return query, params
}

// errRetirementOverBudget is retireVerified's answer for a batch whose
// versions are more than one archive object takes. It is raised while the
// rows are read, so nothing was uploaded and nothing was deleted.
var errRetirementOverBudget = errors.New("retention batch exceeds archive budget; records preserved")

// retirementTally is what retiring a set of candidates did, accumulated
// across its batches -- including the batches before one that failed, which
// is what a caller reports alongside the error.
type retirementTally struct {
	archived, deleted  int
	objects, oversized []string
}

// retireInBatches retires one concept's candidates in batches of
// retirementBatchSize, each through retireSplitting. Both sweeps come through
// here: the operational policies and the journal's model-call and observation
// detail (journalRetentionSweep), so neither can fail a night on a batch the
// other would have split.
func (i *Integration) retireInBatches(ctx context.Context, concept string, rows []map[string]any, withChildren bool, cutoff time.Time, dry bool, tally *retirementTally) error {
	for start := 0; start < len(rows); start += retirementBatchSize {
		end := min(start+retirementBatchSize, len(rows))
		if err := i.retireSplitting(ctx, concept, rows[start:end], withChildren, cutoff, dry, tally); err != nil {
			return err
		}
	}
	return nil
}

// retireSplitting retires one batch, halving it while it is more than one
// archive object takes.
//
// A batch is counted in CANDIDATES and an object is bounded in VERSIONS, and a
// candidate is every version of a record -- of a run, every version of its
// steps and approvals as well. A pipeline's run is many: both its rows take a
// version per 30-second heartbeat, and each of its steps three (queued,
// running, finished), so a hundred finished runs of a few dozen steps, or of
// an hour each, pass the 10,000 versions one object holds; a hundred journal
// rows with large payloads pass its 32 MiB. Refused whole, the batch would be
// refused again every night and hold every record behind it for good. So it is
// halved until each half fits. A record that ALONE does not fit cannot be
// archived in one object, so it is kept whole -- never deleted without its
// archive -- named in the result and logged, and the sweep goes on: one
// oversized record is never the reason nothing else is retired.
func (i *Integration) retireSplitting(ctx context.Context, concept string, rows []map[string]any, withChildren bool, cutoff time.Time, dry bool, tally *retirementTally) error {
	archived, deleted, object, err := i.retireVerified(ctx, concept, rows, withChildren, cutoff, dry)
	if errors.Is(err, errRetirementOverBudget) {
		if len(rows) == 1 {
			id := rowString(rows[0], "id")
			tally.oversized = append(tally.oversized, id)
			i.log().Warn("work: a record is more than one retention archive object holds, so it was kept",
				"component", "work.retention", "concept", concept, "id", id,
				"maxVersions", retirementMaxVersions, "maxBytes", retirementMaxBytes)
			return nil
		}
		half := len(rows) / 2
		if err := i.retireSplitting(ctx, concept, rows[:half], withChildren, cutoff, dry, tally); err != nil {
			return err
		}
		return i.retireSplitting(ctx, concept, rows[half:], withChildren, cutoff, dry, tally)
	}
	if err != nil {
		return err
	}
	tally.archived += archived
	tally.deleted += deleted
	if object != "" {
		tally.objects = append(tally.objects, object)
	}
	return nil
}

// Retain existing global-variable settings. Explicit process configuration
// overrides a stored policy; absent configuration uses the documented default.
// staged-data: MUST-NOT-GATE -- hiding an active stored retention policy can
// silently substitute a shorter default and prematurely retire its records.
// These maintenance-only configuration reads still pass selectAdmitted.
func (i *Integration) operationalRetentionWindows(ctx context.Context) (map[string]int, error) {
	names := []string{"WORKER_INVOCATION_RETENTION_DAYS"}
	for _, p := range operationalPolicies {
		if !slices.Contains(names, p.env) {
			names = append(names, p.env)
		}
	}
	rows, err := i.selectAdmitted(ctx, "v1:platform:globalVariable", `WITH latest AS (SELECT DISTINCT ON(id) id,"createdAt" FROM "MemoryNodes" WHERE concept=? ORDER BY id,"createdAt" DESC) SELECT n.id,n."createdAt",n.payload FROM latest l JOIN "MemoryNodes" n ON n.id=l.id AND n."createdAt"=l."createdAt" WHERE n.payload->>'name' IN (?) AND n.payload->>'active'='true'`, "v1:platform:globalVariable", bun.In(names))
	if err != nil {
		return nil, err
	}
	stored := map[string]string{}
	for _, row := range rows {
		stored[rowString(row, "name")] = fmt.Sprint(row["value"])
	}
	if _, ok := stored["MEMQL_WORKER_INVOCATION_RETENTION_DAYS"]; !ok {
		stored["MEMQL_WORKER_INVOCATION_RETENTION_DAYS"] = stored["WORKER_INVOCATION_RETENTION_DAYS"]
	}
	// Keyed by the env name, which is what a window IS: two policies share
	// v1:work:run, and the three pipelines policies (the work run, the
	// v1:pipelines:run row and the pipeline goal) share one variable.
	// Policies sharing a variable share its default too
	// (TestOperationalPoliciesSharingAWindowShareItsDefault), so the answer
	// does not depend on which of them was read last.
	windows := map[string]int{}
	for _, p := range operationalPolicies {
		days := retentionDays(p.env, p.days)
		if strings.TrimSpace(os.Getenv(p.env)) == "" {
			if n, err := strconv.Atoi(strings.TrimSpace(stored[p.env])); err == nil && n > 0 {
				days = n
			}
		}
		windows[p.env] = days
	}
	return windows, nil
}

// loadRetirementVersions reads a batch's versions -- retirementLoads, one after
// the other -- into one list. The versions are counted together against
// retirementMaxVersions and retirementMaxBytes: a batch over either is
// errRetirementOverBudget, raised while the rows are read, before anything is
// archived. Every row loaded must pass admission, or the whole batch is kept.
func (i *Integration) loadRetirementVersions(ctx context.Context, db *bun.DB, concept string, ids []string, withChildren bool) ([]memorynodes.MemoryNode, error) {
	var nodes []memorynodes.MemoryNode
	size := 0
	for _, load := range retirementLoads(concept, ids, withChildren) {
		rows, err := db.QueryContext(ctx, load.query, load.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				break
			}
			size += len(raw)
			if size > retirementMaxBytes || len(nodes) == retirementMaxVersions {
				err = errRetirementOverBudget
				break
			}
			var n memorynodes.MemoryNode
			if err = json.Unmarshal(raw, &n); err != nil {
				break
			}
			if !i.admitRow(ctx, n) {
				err = fmt.Errorf("retention row admission refused; records preserved")
				break
			}
			nodes = append(nodes, n)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

// retirementStatement is a statement and its arguments.
type retirementStatement struct {
	query string
	args  []any
}

// retirementLoads are the reads that load a batch's versions -- every version
// of the candidates, and for a run every version of its steps, approvals and
// journal detail -- in the order they run. Their rows are counted together
// against retirementMaxVersions and retirementMaxBytes.
//
// TWO STATEMENTS, AND THE CHILDREN BY ONE PROBE PER RUN. This was one statement:
//
//	WHERE (concept=? AND id IN (?)) OR (concept IN (...) AND payload->>'runId' IN (?))
//	ORDER BY concept,id,"createdAt" LIMIT 10001
//
// and it is the statement that timed out on 2026-10-10 after the candidates were
// found. The OR leaves no condition to look up by, so the planner walked the
// (concept, id, createdAt) index in order, whole, with the OR as a filter, and
// stopped at the LIMIT it never reached: 3.3 million buffers, and up to 708,400
// rows thrown away in each of seven chunks, to return 700 (2.2 to 8 s on a table
// of 600,000 run versions and 800,000 step versions; the same plan examines all
// 70,000 rows of TestRetireVerifiedStatementsExamineOnlyTheBatch's table).
//
// Split, the parents are an ordinary lookup by (concept, id): 26 ms. The children
// are not so easily saved. `payload->>'runId' IN (...)` as a filter still lost
// (1.0 to 2.1 s): the only index on that expression is partial, the planner does
// not use a partial index's expression statistics, and so it takes the filter to
// match a large share of every chunk and scans for it. Asked of ONE run at a
// time, in a LATERAL subquery, the same condition is a plain equality, estimated
// at the default share of a chunk, and the partial index (runId, concept, id,
// createdAt) wins every time: 70 ms for the 100 runs. The per-run LIMIT is what
// stops the planner flattening the subquery back into the join it just left; it
// is no bound that matters, the loader counts the rows in all.
//
// The rows come back parents first and then children, each in (concept, id,
// createdAt) order. Nothing reads them in any order but a version's own: the
// newest of an id is the last of it.
//
// staged-data: MUST-NOT-GATE -- see retireVerified: hiding staged versions
// loses their archive evidence. Every row loaded passes admitRow.
func retirementLoads(concept string, ids []string, withChildren bool) []retirementStatement {
	loads := []retirementStatement{{
		query: `SELECT row_to_json(n) FROM "MemoryNodes" n WHERE concept=? AND id IN (?) ORDER BY concept,id,"createdAt" LIMIT ?`,
		args:  []any{concept, bun.In(ids), retirementMaxVersions + 1},
	}}
	if withChildren {
		// One probe of the runId index for each run. The per-run LIMIT is not
		// a bound that matters (the loader stops at retirementMaxVersions in
		// all): it keeps the subquery from being flattened into a join, which
		// the planner would answer by scanning every step.
		loads = append(loads, retirementStatement{
			query: `SELECT c.j FROM unnest(ARRAY[?]::text[]) AS r(id) CROSS JOIN LATERAL (SELECT row_to_json(n) AS j,n.concept,n.id,n."createdAt" FROM "MemoryNodes" n WHERE n.concept IN ` + workChildConceptsSQL + ` AND n.payload->>'runId'=r.id LIMIT ?) c ORDER BY c.concept,c.id,c."createdAt" LIMIT ?`,
			args:  []any{bun.In(ids), retirementMaxVersions + 1, retirementMaxVersions + 1},
		})
	}
	return loads
}

// retirementDelete is the guarded delete of a batch's archived keys: only
// those exact versions, and none at all if any record advanced since the
// archive snapshot. parentIDs are the candidates whose children go with them.
//
// The guard that no child of the batch was left out of the archive is the other
// statement that scanned: written `child.payload->>'runId' IN (?)` it cost 865 ms
// of the delete's 937 on a 600,000-run history, for the reason retirementLoads
// gives, and is now the same probe, one run at a time. The delete as a whole went
// from p50 0.31 s to p50 46 ms.
//
// staged-data: MUST-NOT-GATE -- see retireVerified: hiding staged revisions or
// children defeats the concurrent-write guards.
func retirementDelete(concept string, withChildren bool, parentIDs []string, rawKeys string) retirementStatement {
	// Only archived exact keys can be removed. The whole batch is retained if
	// ANY logical row advanced since the archive snapshot.
	guard := ` AND NOT EXISTS (SELECT 1 FROM keys k JOIN "MemoryNodes" n ON n.id=k.id AND n.concept=k.concept AND n."createdAt"=k.at WHERE NOT (` + retentionInactiveParentSQL + `))`
	params := []any{rawKeys}
	if withChildren {
		// No child of an archived run may have been left out of the archive. Found
		// by one probe of the runId index for each run, for retirementLoads'
		// reason: the planner does not know the index's expression is selective,
		// and answers an IN over it with a scan of every step.
		guard += ` AND NOT EXISTS (SELECT 1 FROM unnest(ARRAY[?]::text[]) AS pr(id) CROSS JOIN LATERAL (SELECT 1 FROM "MemoryNodes" child WHERE child.concept IN ` + workChildConceptsSQL + ` AND child.payload->>'runId'=pr.id AND NOT EXISTS (SELECT 1 FROM keys k WHERE k.id=child.id AND k.concept=child.concept AND k.at=child."createdAt") LIMIT 1) c)`
		params = append(params, bun.In(parentIDs))
	}
	if concept == goalConcept {
		// And no run of an archived goal may have been opened since the
		// candidate read. The pipelines driver opens a new run of an old
		// goal by writing the goal first -- a newer version, which the
		// statement's own newer-version check refuses -- and then the run,
		// refused here; a run written on its own is refused here too. Like the
		// candidate read's, this compares every run version (accepted for this
		// release; the goalId index is deferred to release 2).
		guard += ` AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" r WHERE r.concept='v1:work:run' AND ` + goalRefSQL(`r.payload->>'goalId'`) + ` IN (SELECT ` + goalRefSQL(`k.id`) + ` FROM keys k))`
	}
	return retirementStatement{
		query: `WITH keys AS MATERIALIZED (SELECT * FROM jsonb_to_recordset(?::jsonb) AS k(id text,concept text,at timestamptz)), latest AS (SELECT id,concept,max(at) AS at FROM keys GROUP BY id,concept) DELETE FROM "MemoryNodes" n USING keys k WHERE n.id=k.id AND n.concept=k.concept AND n."createdAt"=k.at AND NOT EXISTS (SELECT 1 FROM latest l JOIN "MemoryNodes" newer ON newer.id=l.id AND newer.concept=l.concept AND newer."createdAt">l.at)` + guard,
		args:  params,
	}
}

type retirementKey struct {
	ID      string    `json:"id"`
	Concept string    `json:"concept"`
	At      time.Time `json:"at"`
}

// retireVerified archives ALL versions, including schema/provenance, with a
// content-addressed object name. Read-back must match before an exact-key
// transactional delete. Bounded batches cap both memory and database work, and
// the statements that load a batch and delete it (retirementLoads,
// retirementDelete) find a run's children one run at a time, so a batch costs
// what it holds and not what the table does.
// A concurrent revision, a new child, or -- for a goal -- a new run of it
// leaves the entire batch in place.
// staged-data: MUST-NOT-GATE -- hiding staged versions loses their archive
// evidence, and hiding staged revisions/children defeats the concurrent-write
// guards. Every archived version passes admitRow before any deletion.
func (i *Integration) retireVerified(ctx context.Context, concept string, candidates []map[string]any, withChildren bool, cutoff time.Time, dry bool) (int, int, string, error) {
	if len(candidates) == 0 {
		return 0, 0, "", nil
	}
	if i.admitRow == nil || i.bunDB == nil || i.bunDB() == nil {
		return 0, 0, "", fmt.Errorf("retention requires a database and row admission")
	}
	db := i.bunDB()
	ids := make([]string, 0, len(candidates))
	expected := map[string]time.Time{}
	for _, r := range candidates {
		at, ok := rowTime(r, "createdAt")
		if !ok {
			return 0, 0, "", fmt.Errorf("retention candidate has no version key")
		}
		id := rowString(r, "id")
		ids = append(ids, id)
		expected[id] = at
	}
	nodes, err := i.loadRetirementVersions(ctx, db, concept, ids, withChildren)
	if err != nil {
		return 0, 0, "", err
	}
	// Group children by their parent, and preserve parents with detail under
	// its longer policy, a live approval, a new child, or a child that is not
	// the run's own.
	blocked := map[string]bool{}
	latest := map[string]memorynodes.MemoryNode{}
	for _, n := range nodes {
		latest[n.ID] = n
		if n.Concept == concept {
			if !n.CreatedAt.After(expected[n.ID]) {
				continue
			}
			blocked[n.ID] = true
		}
	}
	// A CHILD GOES WITH ITS RUN ONLY WHEN IT IS THE RUN'S OWN: the same owner,
	// spelled the same way, as the run's newest version. One rule for both run
	// policies. A system run is unowned, so only an unowned child goes with it
	// -- the rule that policy always had; a pipeline's run is its owner's, and
	// component/workjournal writes every one of its steps under that owner. A
	// child somebody else owns, or one carrying no owner at all, keeps the run
	// whole. Exact rather than spelling-insensitive: both rows come through the
	// same canonicalizing write, and a disagreement keeps the run rather than
	// being reasoned past.
	owners := map[string]string{}
	if withChildren {
		for id, n := range latest {
			if n.Concept != concept {
				continue
			}
			var p map[string]any
			if err = json.Unmarshal(n.Payload, &p); err != nil {
				return 0, 0, "", err
			}
			if owner, ok := p["ownerUserId"].(string); ok {
				owners[id] = owner
			}
		}
	}
	for _, n := range latest {
		if n.Concept == concept {
			continue
		}
		var p map[string]any
		if err = json.Unmarshal(n.Payload, &p); err != nil {
			return 0, 0, "", err
		}
		parent := rowString(p, "runId")
		owner, owned := p["ownerUserId"].(string)
		runOwner, runOwned := owners[parent]
		if n.Concept == modelCallConcept || n.Concept == observationConcept || !n.CreatedAt.Before(cutoff) || !owned || !runOwned || owner != runOwner || (n.Concept == "v1:work:approval" && rowString(p, "decision") == "") {
			blocked[parent] = true
		}
	}
	var kept []memorynodes.MemoryNode
	var parentIDs []string
	for _, id := range ids {
		if !blocked[id] {
			parentIDs = append(parentIDs, id)
		}
	}
	for _, n := range nodes {
		parent := n.ID
		if n.Concept != concept {
			var p map[string]any
			_ = json.Unmarshal(n.Payload, &p)
			parent = rowString(p, "runId")
		}
		if !blocked[parent] {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 || dry {
		return 0, 0, "", nil
	}
	archiver := i.archiverRef()
	container := archiveContainer()
	if archiver == nil || container == "" {
		return 0, 0, "", fmt.Errorf("retention cannot archive: configure %s or %s; records preserved", EnvArchiveContainer, envBlobContainer)
	}
	verifier, ok := archiver.(interface {
		DownloadWithLimit(context.Context, string, string, int64) ([]byte, error)
	})
	if !ok {
		return 0, 0, "", fmt.Errorf("archive does not support bounded verification; records preserved")
	}
	encoded := make([]map[string]any, 0, len(kept))
	keys := make([]retirementKey, 0, len(kept))
	for _, n := range kept {
		raw, e := json.Marshal(n)
		if e != nil {
			return 0, 0, "", e
		}
		var row map[string]any
		// Archive numbers verbatim; float64 would round large integer evidence
		// even though the database and the original RawMessage preserve it.
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if e = decoder.Decode(&row); e != nil {
			return 0, 0, "", e
		}
		encoded = append(encoded, row)
		keys = append(keys, retirementKey{n.ID, n.Concept, n.CreatedAt})
	}
	blob, err := ndjsonGzip(encoded)
	if err != nil {
		return 0, 0, "", err
	}
	// Standard SHA-256 is an external recovery checksum: operators verify a
	// downloaded archive with sha256sum without running MemQL's ID engine.
	digest := sha256.Sum256(blob)
	object := fmt.Sprintf("retention/%s/%x.ndjson.gz", i.clock().UTC().Format("2006-01-02"), digest)
	if _, err = archiver.Upload(ctx, container, object, blob, archiveContentType); err != nil {
		return 0, 0, "", fmt.Errorf("archive upload failed; records preserved: %w", err)
	}
	restored, err := verifier.DownloadWithLimit(ctx, container, object, int64(len(blob))+1)
	if err != nil {
		return 0, 0, "", fmt.Errorf("archive verification failed; records preserved: %w", err)
	}
	if !bytes.Equal(restored, blob) {
		return 0, 0, "", fmt.Errorf("archive checksum mismatch; records preserved")
	}
	rawKeys, err := json.Marshal(keys)
	if err != nil {
		return len(kept), 0, object, err
	}
	var deleted int
	del := retirementDelete(concept, withChildren, parentIDs, string(rawKeys))
	err = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		result, e := tx.ExecContext(ctx, del.query, del.args...)
		if e != nil {
			return e
		}
		count, e := result.RowsAffected()
		if e != nil {
			return e
		}
		deleted = int(count)
		// Keep vectors for a concurrent/new version; otherwise remove them in the
		// SAME transaction so a failed vector deletion cannot leave half a retire.
		tables, e := memql.EmbeddingVectorTables(ctx, tx)
		if e != nil {
			return e
		}
		for _, table := range tables {
			_, e = tx.ExecContext(ctx, `WITH keys AS (SELECT * FROM jsonb_to_recordset(?::jsonb) AS k(id text,concept text,at timestamptz)) DELETE FROM `+table+` v WHERE v.id IN (SELECT id FROM keys) AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" n WHERE n.id=v.id)`, string(rawKeys))
			if e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return len(kept), 0, object, err
	}
	return len(kept), deleted, object, nil
}
