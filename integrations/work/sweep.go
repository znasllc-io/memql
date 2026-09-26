package work

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/num"
)

// sweep.go -- the two scheduled sweeps (design record sections D "Waits" and
// "Retention", and E "Execute").
//
//	sweepWaiting    resume runs whose timer is due; close runs whose node died
//	retentionSweep  fold the summary, archive the journal, then delete it
//
// ===========================================================================
// BOTH RUN UNDER THE CLUSTER'S MAINTENANCE PRINCIPAL
// ===========================================================================
// component/auth/maintenance_actor.go names sweepWaitingWorkRuns and
// workJournalRetentionSweep and says why: v1:work:run declares the composite
// owner tier, so under the default RoleReader system actor the owned branch
// matches nothing, the cluster-owner escape does not apply, and every read
// here answers ZERO ROWS AND NO ERROR. A sweep that resumes nothing is
// indistinguishable from a cluster with nothing parked, and the symptom a
// person reports is that their goal simply stopped.
//
// The handlers assert requireClusterOwner rather than minting an actor of
// their own. The maintenance principal carries RoleOwner and clears it; so
// does an owner running the sweep by hand; nothing else does. Minting the
// actor here instead would make the elevation available to whoever can reach
// the builtin, which is the escape hatch maintenance_actor.go exists to avoid.
//
// ===========================================================================
// WHY THESE READS ARE HAND-ROLLED SQL
// ===========================================================================
// Both sweeps ask questions dsl/work/queries.memql cannot express: "every run
// in flight, whoever owns it" and "every journal row past its window". The
// namespace has workRunsForAutomation (by name) and per-run journal reads (by
// run), and neither is a cross-owner scan.
//
// A hand-rolled SELECT passes through neither the parser nor the filter path,
// so NOTHING IS INJECTED INTO IT and PluginContext.AdmitSourceRow is the whole
// of the enforcement -- applied to the rows AS FETCHED, before anything is
// folded or repacked, because both of the engine's row-authz mechanisms
// resolve the tier from a CONCEPT and a repacked summary carries a made-up one.
//
// The DSL queries that would replace this are listed in goal.go's gaps note.

const (
	modelCallConcept   = "v1:work:modelCall"
	observationConcept = "v1:work:observation"

	// EnvModelCallRetentionDays and EnvObservationRetentionDays are the two
	// windows the design names. Observations are kept twice as long as model
	// calls because they are the EPISODIC MEMORY the recall builtin reads,
	// while a model call is evidence about one request.
	EnvModelCallRetentionDays   = "MEMQL_WORK_MODELCALL_RETENTION_DAYS"
	EnvObservationRetentionDays = "MEMQL_WORK_OBSERVATION_RETENTION_DAYS"

	// EnvArchiveContainer is the blob container the retention sweep writes
	// to, falling back to the cluster's own container. Shared with the log
	// store's archive by design: one deployment, one archive.
	EnvArchiveContainer = "MEMQL_WORK_ARCHIVE_CONTAINER"
	envBlobContainer    = "MEMQL_AZURE_BLOB_CONTAINER"

	DefaultModelCallRetentionDays   = 90
	DefaultObservationRetentionDays = 180

	// DefaultAbandonedAfterSeconds is 60s: TWICE the 30s fleet heartbeat
	// window, so a run is abandoned only after two missed beats. The
	// scheduled automation passes this explicitly; the default is here for a
	// hand call.
	DefaultAbandonedAfterSeconds = 60

	archivePrefix      = "journal/"
	archiveSuffix      = ".ndjson.gz"
	archiveContentType = "application/gzip"

	// sweepPageSize bounds one read, and sweepMaxRows bounds one RUN. A
	// store that fell far behind catches up over several nights rather than
	// in one pass that holds the cron lock for hours.
	sweepPageSize = 2000
	sweepMaxRows  = 50000
)

// Archiver is the narrow slice of object storage the retention sweep needs.
// integrations/azureblob's uploader satisfies it; tests hold a map.
type Archiver interface {
	Upload(ctx context.Context, container, object string, data []byte, contentType string) (string, error)
}

// ---------------------------------------------------------------------------
// sweepWaiting
// ---------------------------------------------------------------------------

// WaitSweepResult is what one pass found.
type WaitSweepResult struct {
	Checked   int `json:"checked"`
	Resumed   int `json:"resumed"`
	Abandoned int `json:"abandoned"`
	// Redispatched counts runs that looked abandoned and were handed back to
	// a live replica instead of being closed. See redispatchStale.
	Redispatched        int `json:"redispatched"`
	OrphanedWaitsClosed int `json:"orphanedWaitsClosed"`
}

func (i *Integration) handleSweepWaiting(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireClusterOwner(ctx); err != nil {
		return nil, err
	}
	olderThan := time.Duration(argInt(args, "olderThanSeconds", DefaultAbandonedAfterSeconds)) * time.Second
	res, err := i.SweepWaiting(ctx, olderThan)
	if err != nil {
		return nil, err
	}
	return i.resultNode(map[string]any{
		"checked":             res.Checked,
		"resumed":             res.Resumed,
		"abandoned":           res.Abandoned,
		"redispatched":        res.Redispatched,
		"orphanedWaitsClosed": res.OrphanedWaitsClosed,
	}), nil
}

// SweepWaiting resumes due timers and closes runs whose node stopped
// answering.
func (i *Integration) SweepWaiting(ctx context.Context, olderThan time.Duration) (WaitSweepResult, error) {
	if i.rowsInFlight == nil {
		return WaitSweepResult{}, fmt.Errorf("work: no in-flight run source is wired; refusing rather than reporting a cluster with nothing parked")
	}
	rows, err := i.rowsInFlight(ctx)
	if err != nil {
		return WaitSweepResult{}, err
	}
	now := i.clock().UTC()
	cutoff := now.Add(-olderThan)
	res := WaitSweepResult{Checked: len(rows)}
	st := i.store()

	for _, run := range rows {
		runId := rowString(run, "id")
		if runId == "" {
			continue
		}
		owner := rowString(run, "ownerUserId")
		// Borrowed authority. The write guard ignores the clusterOwner arm,
		// so even the maintenance principal writes an owned row AS its
		// owner; the value comes off the row just read.
		writeCtx := ownerActor(ctx, owner)
		status := rowString(run, "status")
		if status == runStatusCompiling {
			// Events can be lost while planners are unavailable. Compilation
			// uses the same durable claim for this recovery and eager delivery.
			// Only a LOCAL compiler can recover here: EnableCompileViaEvent makes
			// createGoal honest on a BFF, but dispatchCompile then returns true
			// without claiming — counting that as Redispatched would skip the
			// abandon path forever (memql#5262 fold of #5268).
			if i.compilerRef() != nil && i.dispatchCompile(writeCtx, CompileRequest{RunId: runId, OwnerUserId: owner}) {
				res.Redispatched++
				continue
			}
			// Another planner may have won since rowsInFlight took its
			// snapshot. Judge its current heartbeat, never that old snapshot.
			current, readErr := st.runForOwner(writeCtx, runId)
			if readErr != nil || current == nil || rowString(current, "status") != runStatusCompiling {
				continue
			}
			run = current
			if argBool(run, "cancelRequested") {
				if err := st.updateRun(writeCtx, runId, map[string]any{"status": runStatusCancelled, "finishedAt": rfc(now)}); err != nil {
					i.log().Warn("work: could not cancel a compiling run", "run", runId, "error", err)
				}
				continue
			}
		}

		if status == runStatusWaiting {
			if closed, err := i.closeOrphanedSystemApprovalWait(ctx, run, now); err != nil {
				i.log().Warn("work: could not check a system approval wait", "run", runId, "error", err)
				continue
			} else if closed {
				res.OrphanedWaitsClosed++
				continue
			}
			// A CLASSIFIED FAILURE'S ACT IS SERVED FIRST (epic memql#5127).
			// It is checked before the inference park and the timer because
			// those two ask different questions of the same field: a `retry`
			// wait carries a resumeAt exactly as a timer does, and reaching
			// timerDue first would release it to `running` WITHOUT a dispatch
			// -- a run at `running` that nobody is executing, which the
			// abandoned sweep then closes saying the node stopped answering.
			if i.serveFailureWait(ctx, run, runId, owner, now, &res) {
				continue
			}

			// A RUN PARKED ON A SHUT INFERENCE DOOR IS RE-TRIED
			// (epic memql#5096, design D9), and it is the ONE approval kind
			// that is. Every other kind waits on a person, and handing one
			// back to the cluster would run the work behind their back --
			// which is the whole reason the kind is checked rather than the
			// wait shape. Here nobody has to decide anything: a lid opens or
			// somebody signs into Claude Code and the answer changes, so the
			// run tries again and parks again if it is still shut.
			if due, ok := inferenceRetryDue(run, now); ok {
				if !due {
					continue
				}
				if i.redispatchStale(writeCtx, run, runId, owner) {
					i.log().Info("work: handed a run parked on a shut inference door back to the cluster",
						"component", "work.sweep", "run", runId, "owner", owner)
					res.Redispatched++
					continue
				}
				// No dispatcher here (a bff replica running the sweep), or
				// the claim lease is still held. Leaving it parked is right:
				// the next pass tries again, and nothing about the run has
				// changed.
				continue
			}

			// A PARKED RUN IS NEVER ABANDONED. It is silent on purpose --
			// no process is held open for a wait -- so judging it by its
			// heartbeat would close every run waiting on a person.
			// Only a DUE TIMER releases it.
			due, ok := timerDue(run, now)
			if !ok || !due {
				continue
			}
			if err := st.updateRun(writeCtx, runId, map[string]any{
				"status":      runStatusRunning,
				"waitingOn":   map[string]any{},
				"heartbeatAt": rfc(now),
			}); err != nil {
				i.log().Warn("work: could not resume a run whose timer was due",
					"component", "work.sweep", "run", runId, "err", err)
				continue
			}
			i.log().Info("work: resumed a run whose timer wait came due",
				"component", "work.sweep", "run", runId, "owner", owner)
			res.Resumed++
			// Goal events can dispatch eagerly, but ordinary scheduler journals
			// require this explicit recovery path. Both share the run claim.
			i.DispatchRun(ctx, runId, owner)
			continue
		}

		last, ok := lastHeartbeat(run)
		if !ok {
			// A run with no heartbeat AND no start time cannot be judged, so
			// it is left alone. Sweeping on an absent timestamp would close
			// every row written before the field existed -- including ones
			// somebody is looking at.
			continue
		}
		if last.After(cutoff) {
			continue
		}
		// ONE ATTEMPT TO HAND IT BACK BEFORE CLOSING IT. A run goes silent
		// for two reasons that look identical from here: the replica running
		// it died, or nothing ever picked it up (every agent replica was down
		// when it flipped to `running`, so the dispatch event reached
		// nobody). The second is recoverable and was being closed as though
		// it were the first.
		//
		// THE CLAIM LEASE IS WHAT BOUNDS THIS, and it is why there is no
		// attempt counter on the row. runClaimTTL is 4x this sweep's window,
		// so a replica that takes the run and immediately dies still HOLDS
		// the claim at the next pass -- the re-dispatch is refused there and
		// the run is abandoned as it would have been. A run can therefore be
		// handed back at most once per lease, and a run nobody can execute
		// still reaches `abandoned` a pass later rather than being retried
		// forever.
		//
		// A takeover RESUMES rather than restarts: the seam loads the run's
		// journal and resumes from the step that was in flight, so the steps
		// that already ran are not re-executed.
		if status != runStatusCompiling && i.redispatchStale(writeCtx, run, runId, owner) {
			res.Redispatched++
			continue
		}
		code := "run_abandoned"
		if status == runStatusCompiling && rowString(run, "heartbeatAt") == "" {
			// A planner can have won arbitration and still be writing its
			// first heartbeat. Closing an unclaimed run participates in the
			// same arbitration, so neither side can invalidate the other's
			// read in that gap. An interrupted claim expires normally.
			if !i.claimCompile(writeCtx, runId) {
				continue
			}
			current, err := st.runForOwner(writeCtx, runId)
			if err != nil || rowString(current, "status") != runStatusCompiling || rowString(current, "heartbeatAt") != "" {
				continue
			}
			run = current
			code = "compile_unclaimed"
		}
		if err := st.updateRun(writeCtx, runId, map[string]any{
			"status":       runStatusAbandoned,
			"errorCode":    code,
			"errorMessage": abandonedMessage(run, last),
			"finishedAt":   rfc(now),
		}); err != nil {
			// One run that will not close must not stop the rest.
			i.log().Warn("work: could not close an abandoned run",
				"component", "work.sweep", "run", runId, "err", err)
			continue
		}
		if neverReachedCompile(run) {
			i.log().Info("work: closed a run that never reached a compile surface",
				"component", "work.sweep", "run", runId,
				"lastHeartbeat", last.UTC().Format(time.RFC3339), "node", rowString(run, "nodeId"),
				"status", status, "automationName", rowString(run, "automationName"))
		} else {
			i.log().Info("work: closed a run whose node stopped answering",
				"component", "work.sweep", "run", runId,
				"lastHeartbeat", last.UTC().Format(time.RFC3339), "node", rowString(run, "nodeId"))
		}
		res.Abandoned++
	}
	return res, nil
}

// Earlier journals parked failed maintenance runs even when approval creation
// failed validation. Those waits have no answerable gate and no human owner.
// Close only this proven orphan shape, preserving the run, error and history.
// A read failure is never treated as a missing approval; user goals and real
// approvals keep their existing lifecycle.
func (i *Integration) closeOrphanedSystemApprovalWait(ctx context.Context, run map[string]any, now time.Time) (bool, error) {
	if owner, present := run["ownerUserId"]; !present || owner != "" || rowString(run, "goalId") != "" || rowString(run, "triggeredBy") != "schedule" {
		return false, nil
	}
	waiting := rowMap(run, "waitingOn")
	if rowString(waiting, "kind") != "approval" || rowString(waiting, "subject") == "" {
		return false, nil
	}
	since, ok := rowTime(waiting, "since")
	if !ok || since.After(now.Add(-time.Minute)) {
		return false, nil
	}
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || !actor.IsClusterOwner() {
		return false, fmt.Errorf("work: only cluster maintenance may inspect orphaned system waits")
	}
	st := i.store()
	approvals, err := st.queryInternal(ctx, "query "+call("workApprovalById", map[string]any{"approvalId": rowString(waiting, "subject")}))
	if err != nil || len(approvals) != 0 {
		return false, err
	}
	err = st.updateRun(ctx, rowString(run, "id"), map[string]any{
		"status":       runStatusFailed,
		"finishedAt":   rfc(now),
		"waitingOn":    map[string]any{},
		"errorCode":    "approval_missing",
		"errorMessage": "The system task failed and its approval was not saved. This orphaned wait was closed; the next scheduled execution retries the maintenance task.",
	})
	return err == nil, err
}

// redispatchStale offers a silent run back to the cluster and reports whether
// a replica took it.
//
// It is a no-op on every node that runs no steps: DispatchRun returns false
// with no dispatcher installed, so a bff replica running this sweep abandons
// exactly as it did before. That is the honest default -- the alternative,
// treating "I cannot dispatch" as "somebody else will", would leave dead runs
// open forever on a cluster whose agent nodes are gone.
func (i *Integration) redispatchStale(ctx context.Context, run map[string]any, runId, owner string) bool {
	// Only a run that HAS an automation to execute can be handed back. A run
	// at `running` with no template is the compile-failed shape, and
	// dispatching it would claim a run the seam then refuses -- burning a
	// lease and delaying the close by one pass for nothing. The compile
	// sentinel (`work.compile`) is the same shape: it names no template, and
	// handing it to the executor would claim a run that then fails for
	// "automation not runnable".
	name := rowString(run, "automationName")
	if name == "" || name == compilingAutomationName {
		return false
	}
	if !i.dispatchRun(ctx, DispatchRequest{RunId: runId, OwnerUserId: owner, GoalId: rowString(run, "goalId"), Status: rowString(run, "status"), Recovery: true}) {
		return false
	}
	i.log().Info("work: handed a silent run back to the cluster instead of abandoning it",
		"component", "work.sweep", "run", runId, "owner", owner,
		"node", rowString(run, "nodeId"))
	return true
}

// timerDue reads waitingOn and answers (due, isTimer).
//
// resumeAt is read off waitingOn.resumeAt, falling back to the step's own
// resumeAt when the wait recorded only a subject. An UNPARSEABLE timestamp is
// NOT due: a run resumed on a timestamp nobody could read would run early with
// no way to tell that it had.
func timerDue(run map[string]any, now time.Time) (due bool, isTimer bool) {
	waiting := rowMap(run, "waitingOn")
	if waiting == nil {
		return false, false
	}
	kind, _ := waiting["kind"].(string)
	if trim(kind) != "timer" {
		return false, false
	}
	raw, _ := waiting["resumeAt"].(string)
	at, ok := parseTime(trim(raw))
	if !ok {
		return false, true
	}
	return !at.After(now), true
}

// lastHeartbeat is the newest evidence a run was alive: heartbeatAt, else
// startedAt, else createdAt. A run written before heartbeatAt existed, or one
// that died before its first beat, still has a start time -- and "it started
// nine days ago and never finished" is enough to act on.
func lastHeartbeat(run map[string]any) (time.Time, bool) {
	for _, key := range []string{"heartbeatAt", "startedAt", "createdAt"} {
		if t, ok := rowTime(run, key); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// abandonedMessage says what the sweep can actually know. It does NOT say the
// run failed: whether the work was about to succeed is not something this can
// see, and an append-only row cannot be corrected.
//
// Two shapes share the abandoned status and must NOT share a sentence:
//
//   - A run that was executing and whose node went silent -- "lost the node".
//   - A run that never left `compiling` / still carries the work.compile
//     sentinel -- compile never ran. Saying the node was lost sends the reader
//     at infrastructure for a goal the bff accepted with no compile surface.
func abandonedMessage(run map[string]any, last time.Time) string {
	if neverReachedCompile(run) {
		return fmt.Sprintf(
			"this run never reached a compile surface; it was still compiling (no planner compiled it) when the sweep closed it at %s. Re-create the goal once a planner is available, or resume only after compile can run.",
			last.UTC().Format(time.RFC3339))
	}
	where := ""
	if node := rowString(run, "nodeId"); node != "" {
		where = fmt.Sprintf(" (%s)", node)
	}
	return fmt.Sprintf(
		"this cluster lost the node that was running this%s; it was last heard from at %s. Completed steps are in the journal, so a resume serves them rather than running them again.",
		where, last.UTC().Format(time.RFC3339))
}

// neverReachedCompile reports a run that was abandoned before any template was
// chosen: still in `compiling`, or still carrying the compile sentinel name.
func neverReachedCompile(run map[string]any) bool {
	if rowString(run, "status") == runStatusCompiling {
		return true
	}
	return rowString(run, "automationName") == compilingAutomationName
}

// ---------------------------------------------------------------------------
// retentionSweep
// ---------------------------------------------------------------------------

// RetentionResult is what one retention pass did.
type RetentionResult struct {
	JournalCandidates   int                          `json:"journalCandidates"`
	Operational         []OperationalRetentionResult `json:"operational,omitempty"`
	BoundaryModelCall   string                       `json:"boundaryModelCall"`
	BoundaryObservation string                       `json:"boundaryObservation"`
	RunsSummarized      int                          `json:"runsSummarized"`
	RowsArchived        int                          `json:"rowsArchived"`
	RowsDeleted         int                          `json:"rowsDeleted"`
	Objects             []string                     `json:"objects,omitempty"`
	Container           string                       `json:"container,omitempty"`
	DryRun              bool                         `json:"dryRun,omitempty"`
	// Refused says why nothing was deleted, when nothing was. It is a
	// SENTENCE rather than a flag because the operator response differs: no
	// container is a configuration choice, a failed upload is an incident.
	Refused string `json:"refused,omitempty"`
	Took    string `json:"took"`
}

func (i *Integration) handleRetentionSweep(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireClusterOwner(ctx); err != nil {
		return nil, err
	}
	res, err := i.RetentionSweep(ctx, argBool(args, "dryRun"))
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(res)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return i.resultNode(out), nil
}

// RetentionSweep folds, archives, then deletes -- in that order, and no
// further than the order allows.
//
// # The fold comes FIRST, and that is the one ordering that destroys evidence
// if reversed
//
// A run's summary is folded onto the run row BEFORE its detail goes, so a run
// stays readable after (design section D, "Retention"). Deleting first would
// leave a run that never got its summary and whose detail is gone -- there is
// no recovery from that inside the cluster.
//
// # NO ARCHIVE MEANS NO DELETE
//
// The log store's rule, reused. A cluster with no archive container keeps its
// journal and this says so, every night, in Refused. There is deliberately no
// TimescaleDB retention policy on these rows for exactly this reason: a policy
// cannot be told to wait for an upload.
func (i *Integration) RetentionSweep(ctx context.Context, dryRun bool) (RetentionResult, error) {
	if err := requireClusterOwner(ctx); err != nil {
		return RetentionResult{}, err
	}
	result, err := i.journalRetentionSweep(ctx, dryRun)
	if err != nil {
		return result, err
	}
	result.Operational, err = i.operationalRetention(ctx, dryRun)
	for _, p := range result.Operational {
		result.RowsArchived += p.ArchivedVersions
		result.RowsDeleted += p.DeletedVersions
		result.Objects = append(result.Objects, p.Objects...)
	}
	i.log().Info("work: operational retention pass complete", "component", "work.retention", "dryRun", dryRun, "policies", result.Operational, "error", err)
	return result, err
}

func (i *Integration) journalRetentionSweep(ctx context.Context, dryRun bool) (RetentionResult, error) {
	started := time.Now()
	now := i.clock().UTC()
	res := RetentionResult{DryRun: dryRun}

	mcBoundary := now.AddDate(0, 0, -retentionDays(EnvModelCallRetentionDays, DefaultModelCallRetentionDays))
	obBoundary := now.AddDate(0, 0, -retentionDays(EnvObservationRetentionDays, DefaultObservationRetentionDays))
	res.BoundaryModelCall = mcBoundary.Format(time.RFC3339)
	res.BoundaryObservation = obBoundary.Format(time.RFC3339)

	expired := map[string][]map[string]any{}
	for _, spec := range []struct {
		concept  string
		boundary time.Time
	}{
		{modelCallConcept, mcBoundary},
		{observationConcept, obBoundary},
	} {
		rows, err := i.expiredJournalRows(ctx, spec.concept, spec.boundary)
		if err != nil {
			return res, err
		}
		expired[spec.concept] = rows
		res.JournalCandidates += len(rows)
	}
	if res.JournalCandidates == 0 {
		res.RowsArchived = 0
		res.Took = time.Since(started).String()
		return res, nil
	}

	// 1. FOLD. Every run whose detail is about to age out gets its summary
	//    written first.
	summarized, err := i.foldSummaries(ctx, expired, dryRun)
	if err != nil {
		return res, err
	}
	res.RunsSummarized = summarized

	container := archiveContainer()
	archiver := i.archiverRef()
	if dryRun {
		res.RunsSummarized = 0
		res.Container = container
		res.Refused = "dry run: nothing was archived and nothing was deleted"
		res.Took = time.Since(started).String()
		return res, nil
	}
	if archiver == nil || container == "" {
		// The refusal is the DESIGNED behaviour, not a failure, so it is
		// reported rather than returned as an error: the sweep ran, found
		// expired rows, and kept them.
		res.RowsArchived = 0
		res.Refused = "no archive container is configured (" + EnvArchiveContainer + " or " + envBlobContainer + "), so nothing was deleted -- no archive means no delete"
		i.log().Warn("work: the journal retention sweep refused to delete because there is nowhere to archive to",
			"component", "work.retention", "expiredRows", len(expired[modelCallConcept])+len(expired[observationConcept]))
		res.Took = time.Since(started).String()
		return res, nil
	}
	res.Container = container

	// Archive complete versions in immutable batches and verify the bytes before
	// an exact-key transactional delete. A second partial batch cannot overwrite
	// the first day's evidence, and concurrent updates keep their history.
	res.RowsArchived = 0
	for concept, rows := range expired {
		for start := 0; start < len(rows); start += retirementBatchSize {
			end := min(start+retirementBatchSize, len(rows))
			archived, deleted, object, err := i.retireVerified(ctx, concept, rows[start:end], false, now, false)
			if err != nil {
				return res, err
			}
			res.RowsArchived += archived
			res.RowsDeleted += deleted
			if object != "" {
				res.Objects = append(res.Objects, object)
			}
		}
	}
	sort.Strings(res.Objects)
	res.Took = time.Since(started).String()

	i.log().Info("work: journal retention pass complete",
		"component", "work.retention", "archived", res.RowsArchived, "deleted", res.RowsDeleted,
		"runsSummarized", res.RunsSummarized, "objects", len(res.Objects))
	return res, nil
}

// foldSummaries writes each affected run's counts onto its run row.
//
// The summary is deliberately MODEST: how many calls and observations the run
// had, what it spent on them, and the window they covered. Those are facts the
// rows being deleted actually carry. Anything richer -- a narrative, a
// verdict -- would be a claim invented at the moment the evidence for it is
// destroyed.
func (i *Integration) foldSummaries(ctx context.Context, expired map[string][]map[string]any, dryRun bool) (int, error) {
	type fold struct {
		owner       string
		modelCalls  int
		observation int
		tokensIn    int64
		tokensOut   int64
		cost        float64
		earliest    time.Time
		latest      time.Time
	}
	byRun := map[string]*fold{}
	for concept, rows := range expired {
		for _, row := range rows {
			runId := rowString(row, "runId")
			if runId == "" {
				continue
			}
			f := byRun[runId]
			if f == nil {
				f = &fold{owner: rowString(row, "ownerUserId")}
				byRun[runId] = f
			}
			if concept == modelCallConcept {
				f.modelCalls++
				f.tokensIn += rowInt64(row, "inputTokens")
				f.tokensOut += rowInt64(row, "outputTokens")
				f.cost += rowFloat(row, "cost")
			} else {
				f.observation++
			}
			if at, ok := rowTime(row, "createdAt"); ok {
				if f.earliest.IsZero() || at.Before(f.earliest) {
					f.earliest = at
				}
				if at.After(f.latest) {
					f.latest = at
				}
			}
		}
	}
	if dryRun {
		return len(byRun), nil
	}

	st := i.store()
	written := 0
	for runId, f := range byRun {
		summary := map[string]any{
			"journalFoldedAt":   rfc(i.clock().UTC()),
			"modelCalls":        f.modelCalls,
			"observations":      f.observation,
			"inputTokens":       f.tokensIn,
			"outputTokens":      f.tokensOut,
			"cost":              f.cost,
			"journalFrom":       rfcOrEmpty(f.earliest),
			"journalTo":         rfcOrEmpty(f.latest),
			"journalRetiredFor": "retention",
		}
		if err := st.updateRun(ownerActor(ctx, f.owner), runId, map[string]any{"summary": summary}); err != nil {
			return written, fmt.Errorf("work: summary fold failed for %s; journal retained: %w", runId, err)
		}
		written++
	}
	return written, nil
}

// ---------------------------------------------------------------------------
// The hand-rolled reads (see the header for why they are hand-rolled)
// ---------------------------------------------------------------------------

// runsInFlight is every run at a non-terminal status, whoever owns it.
//
// staged-data: MUST-NOT-GATE -- recovery must see staged runs too. The actual
// canonical rows still pass row admission before they leave this integration.
// Current heads are maintained from the transactional dirty queue, so this
// read never scans the completed history or every current terminal ID.
const runsInFlightSQL = `
WITH candidates AS MATERIALIZED (
    SELECT id, "createdAt" FROM work_run_heads
    WHERE status NOT IN ('succeeded', 'failed', 'cancelled', 'abandoned')
    ORDER BY "createdAt", id LIMIT ?
)
SELECT n.id,n."createdAt",n.payload
FROM candidates c
JOIN LATERAL (
    SELECT id,"createdAt",payload FROM "MemoryNodes" n
    WHERE n.id=c.id AND n."createdAt"=c."createdAt" AND n.concept=?
    LIMIT 1
) n ON true
ORDER BY n."createdAt", n.id
`

// expiredJournalRowsSQL is the retention read. The boundary is applied to the
// LATEST version's createdAt, so a row rewritten inside the window is kept.
//
// staged-data: MUST-NOT-GATE -- a staged journal row SKIPPED HERE SURVIVES ITS
// RETENTION WINDOW FOREVER, and unlike a skipped run there is nothing left to
// find it: no later pass looks at a row this one did not return, so the table
// grows past the window an operator configured and past whatever the
// deployment promised about how long a model request is kept. It is the
// argument integrations/shopify's PurgeStore makes about shop/redact, with a
// retention window in place of a legal deadline. Row-level authorization still
// applies, per row as fetched.
const expiredJournalRowsSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id) id, "createdAt"
    FROM "MemoryNodes"
    WHERE concept = ?
    ORDER BY id, "createdAt" DESC
), expired AS MATERIALIZED (
    SELECT id, "createdAt" FROM latest
    WHERE "createdAt" < ?
    ORDER BY "createdAt" ASC
    LIMIT ?
)
SELECT n.id, n."createdAt", n.payload
FROM expired e JOIN "MemoryNodes" n
    ON n.id = e.id AND n."createdAt" = e."createdAt"
WHERE ` + retentionInactiveParentSQL + `
ORDER BY n."createdAt" ASC
`

func (i *Integration) expiredJournalRows(ctx context.Context, concept string, boundary time.Time) ([]map[string]any, error) {
	return i.selectAdmitted(ctx, concept, expiredJournalRowsSQL, concept, boundary.UTC(), sweepMaxRows)
}

// selectAdmitted runs one hand-rolled read and applies the row-authz gate to
// every row AS FETCHED.
//
// The admission is NOT a formality here even though the caller is a cluster
// owner: it is the seam that keeps this read honest if the sweeps are ever
// reachable by anyone else, and it is the documented requirement for a
// hand-rolled read (component/memql/plugins.go). Fail-CLOSED: a nil gate is
// refused at construction, and an undecidable tier is denied by the gate
// itself.
func (i *Integration) selectAdmitted(ctx context.Context, concept, query string, params ...any) ([]map[string]any, error) {
	// The admission gate is checked FIRST, and the order is the fail-closed
	// direction: "there is no database" is a configuration answer, while "we
	// cannot tell who may see this row" is the one that must never be
	// resolved by proceeding.
	if i.admitRow == nil {
		return nil, fmt.Errorf("work: no row-admission gate is wired; refusing a hand-rolled read rather than admitting every row")
	}
	if i.bunDB == nil || i.bunDB() == nil {
		return nil, fmt.Errorf("work: the sweeps need a database handle")
	}
	return i.selectAdmittedFrom(ctx, i.bunDB(), concept, query, params...)
}

// selectAdmittedFrom also supports the recovery snapshot transaction.
func (i *Integration) selectAdmittedFrom(ctx context.Context, db bun.IDB, concept, query string, params ...any) ([]map[string]any, error) {
	if i.admitRow == nil {
		return nil, fmt.Errorf("work: no row-admission gate is wired")
	}
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, fmt.Errorf("work: read %s: %w", concept, err)
	}
	defer func() { _ = rows.Close() }()

	var out []map[string]any
	for rows.Next() {
		var (
			idv       string
			createdAt time.Time
			payload   []byte
		)
		if err := rows.Scan(&idv, &createdAt, &payload); err != nil {
			return nil, fmt.Errorf("work: scan %s: %w", concept, err)
		}
		node := memorynodes.MemoryNode{
			ID:        idv,
			Concept:   concept,
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: createdAt,
			Payload:   payload,
		}
		if !i.admitRow(ctx, node) {
			continue
		}
		row := map[string]any{"id": idv, "concept": concept, "createdAt": createdAt.UTC().Format(time.RFC3339Nano)}
		if len(payload) > 0 {
			var fields map[string]any
			if err := json.Unmarshal(payload, &fields); err == nil {
				for k, v := range fields {
					if k == "id" || k == "concept" || k == "createdAt" {
						continue
					}
					row[k] = v
				}
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("work: read %s: %w", concept, err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// retentionDays reads a window. A non-positive or unparseable value takes the
// default rather than becoming zero: zero days would archive and delete the
// whole journal on the next pass.
func retentionDays(env string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// archiveContainer is the work archive's container, falling back to the
// cluster's own.
func archiveContainer() string {
	if c := strings.TrimSpace(os.Getenv(EnvArchiveContainer)); c != "" {
		return c
	}
	return strings.TrimSpace(os.Getenv(envBlobContainer))
}

// groupByDay buckets rows by the UTC day of their createdAt, which is the
// archive's addressing unit: journal/<day>/<concept>.ndjson.gz.
func groupByDay(rows []map[string]any) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, row := range rows {
		at, ok := rowTime(row, "createdAt")
		if !ok {
			// A row with no readable timestamp has no day to file under, so
			// it is left where it is rather than archived to a guess.
			continue
		}
		day := at.UTC().Format("2006-01-02")
		out[day] = append(out[day], row)
	}
	return out
}

// ndjsonGzip renders one archive object: one JSON row per line, gzipped.
func ndjsonGzip(rows []map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			_ = zw.Close()
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// conceptLeaf is the last segment of a concept id, for the archive object
// name: v1:work:modelCall -> modelCall.
func conceptLeaf(concept string) string {
	if i := strings.LastIndex(concept, ":"); i >= 0 && i+1 < len(concept) {
		return concept[i+1:]
	}
	return concept
}

func parseTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// rowInt64 narrows a decoded payload number to int64 for the summary fold.
//
// The float arm goes through num.ClampFloat64ToInt64 rather than a bare
// int64(v): float64 -> int64 IS the implementation-defined conversion, so a
// 64-bit build does not make it safe, it makes it the only width that matters
// (core/num, memql#4779). SATURATION is the answer here rather than zero,
// because these are token COUNTS -- a magnitude, where saturating preserves
// the ordering and zeroing would report a run that burned an absurd number of
// tokens as having spent none.
func rowInt64(row map[string]any, key string) int64 {
	if row == nil {
		return 0
	}
	switch v := row[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return num.ClampFloat64ToInt64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	}
	return 0
}

func rowFloat(row map[string]any, key string) float64 {
	if row == nil {
		return 0
	}
	switch v := row[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return 0
}

var _ = sql.ErrNoRows

// inferenceRetryDue answers (due, isInferencePark) for a waiting run.
//
// It keys on the APPROVAL KIND on the wait, not on the wait's own kind. The
// wait is `approval` for every human gate, and a run parked on a side-effect
// approval must never be re-dispatched -- somebody is deciding about it. The
// kind is what distinguishes a gate that waits on a PERSON from one that waits
// on a CONDITION.
//
// A park with NO resumeAt is not due, ever, and that is deliberate: a
// ceiling refusal carries none, because only a person changes a ceiling and
// re-dispatching every five minutes would burn a dispatch to rediscover a
// number nobody touched. An UNPARSEABLE resumeAt is also not due, for the
// reason timerDue gives about its own: a run resumed on a timestamp nobody
// could read would run early with no way to tell that it had.
func inferenceRetryDue(run map[string]any, now time.Time) (due bool, isInferencePark bool) {
	waiting := rowMap(run, "waitingOn")
	if waiting == nil {
		return false, false
	}
	kind, _ := waiting["approvalKind"].(string)
	if trim(kind) != work.ApprovalKindInferenceUnavailable {
		return false, false
	}
	raw, _ := waiting["resumeAt"].(string)
	at, ok := parseTime(trim(raw))
	if !ok {
		return false, true
	}
	return !at.After(now), true
}
