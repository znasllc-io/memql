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

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
		"checked":   res.Checked,
		"resumed":   res.Resumed,
		"abandoned": res.Abandoned,
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

		if status == runStatusWaiting {
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
		if err := st.updateRun(writeCtx, runId, map[string]any{
			"status":       runStatusAbandoned,
			"errorCode":    "run_abandoned",
			"errorMessage": abandonedMessage(run, last),
			"finishedAt":   rfc(now),
		}); err != nil {
			// One run that will not close must not stop the rest.
			i.log().Warn("work: could not close an abandoned run",
				"component", "work.sweep", "run", runId, "err", err)
			continue
		}
		i.log().Info("work: closed a run whose node stopped answering",
			"component", "work.sweep", "run", runId,
			"lastHeartbeat", last.UTC().Format(time.RFC3339), "node", rowString(run, "nodeId"))
		res.Abandoned++
	}
	return res, nil
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

// abandonedMessage says the three things a person needs and nothing the sweep
// cannot know. It does NOT say the run failed: the cluster lost the node, and
// whether the work was about to succeed is not something this can see.
func abandonedMessage(run map[string]any, last time.Time) string {
	where := ""
	if node := rowString(run, "nodeId"); node != "" {
		where = fmt.Sprintf(" (%s)", node)
	}
	return fmt.Sprintf(
		"this cluster lost the node that was running this%s; it was last heard from at %s. Completed steps are in the journal, so a resume serves them rather than running them again.",
		where, last.UTC().Format(time.RFC3339))
}

// ---------------------------------------------------------------------------
// retentionSweep
// ---------------------------------------------------------------------------

// RetentionResult is what one retention pass did.
type RetentionResult struct {
	BoundaryModelCall   string   `json:"boundaryModelCall"`
	BoundaryObservation string   `json:"boundaryObservation"`
	RunsSummarized      int      `json:"runsSummarized"`
	RowsArchived        int      `json:"rowsArchived"`
	RowsDeleted         int      `json:"rowsDeleted"`
	Objects             []string `json:"objects,omitempty"`
	Container           string   `json:"container,omitempty"`
	DryRun              bool     `json:"dryRun,omitempty"`
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
		res.RowsArchived += len(rows)
	}
	if res.RowsArchived == 0 {
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

	// 2. ARCHIVE, per (UTC day, concept). Only a group that UPLOADED is
	//    eligible for deletion, which is what makes the rule hold at the
	//    granularity of the rows rather than of the run.
	deletable := map[string][]string{}
	archived := 0
	for concept, rows := range expired {
		for day, group := range groupByDay(rows) {
			blob, err := ndjsonGzip(group)
			if err != nil {
				i.log().Warn("work: could not encode a journal archive; the rows stay",
					"component", "work.retention", "concept", concept, "day", day, "err", err)
				continue
			}
			object := archivePrefix + day + "/" + conceptLeaf(concept) + archiveSuffix
			if _, err := archiver.Upload(ctx, container, object, blob, archiveContentType); err != nil {
				// The rows for THIS day stay. Deleting them now would be
				// exactly the "no archive, no delete" violation.
				i.log().Warn("work: a journal archive upload failed; those rows were not deleted",
					"component", "work.retention", "concept", concept, "day", day, "err", err)
				continue
			}
			res.Objects = append(res.Objects, object)
			archived += len(group)
			for _, row := range group {
				if idv := rowString(row, "id"); idv != "" {
					deletable[concept] = append(deletable[concept], idv)
				}
			}
		}
	}
	sort.Strings(res.Objects)
	res.RowsArchived = archived

	// 3. DELETE, and only what step 2 uploaded.
	deleted, err := i.deleteJournalRows(ctx, deletable)
	res.RowsDeleted = deleted
	res.Took = time.Since(started).String()
	if err != nil {
		return res, err
	}
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
			// A run whose summary will not write is a run whose detail must
			// NOT be deleted -- but the detail rows are grouped by day
			// across runs, so refusing the whole day would starve the sweep
			// on one bad row. The fold failure is logged loudly and the rows
			// are kept by the NEXT pass finding them again: the delete below
			// only removes what uploaded, and the next run re-folds.
			i.log().Warn("work: could not fold a run's journal summary; its detail is archived but the summary is missing",
				"component", "work.retention", "run", runId, "err", err)
			continue
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
// DISTINCT ON (id) ... ORDER BY id, "createdAt" DESC picks the newest version
// of each row, because MemQL is append-only: without it a run that advanced
// five times would be judged on its first version's status.
//
// staged-data: MUST-NOT-GATE -- a staged run SKIPPED HERE IS NEVER SWEPT. This
// is the only reader allowed to close a run whose node died and the only one
// that resumes a due timer, and nothing looks at that run again: it sits at
// `running` or `waiting` forever while the person who asked for it watches a
// goal that simply stopped, with no error anywhere. The staged tier withholds
// a concept's rows from READERS until it is trained; it was never meant to
// withhold a row from the cluster's own recovery, and the two sweeps are the
// recovery. Row-level authorization is applied instead, per row as fetched
// (selectAdmitted below), which is the question that IS answerable here.
const runsInFlightSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id) id, "createdAt", payload
    FROM "MemoryNodes"
    WHERE concept = $1
    ORDER BY id, "createdAt" DESC
)
SELECT id, "createdAt", payload
FROM latest
WHERE COALESCE(payload->>'status', '') NOT IN ('succeeded', 'failed', 'cancelled', 'abandoned')
ORDER BY "createdAt" ASC
LIMIT $2
`

func (i *Integration) runsInFlight(ctx context.Context) ([]map[string]any, error) {
	return i.selectAdmitted(ctx, runConcept, runsInFlightSQL, runConcept, sweepPageSize)
}

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
    SELECT DISTINCT ON (id) id, "createdAt", payload
    FROM "MemoryNodes"
    WHERE concept = $1
    ORDER BY id, "createdAt" DESC
)
SELECT id, "createdAt", payload
FROM latest
WHERE "createdAt" < $2
ORDER BY "createdAt" ASC
LIMIT $3
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
	rows, err := i.bunDB().QueryContext(ctx, query, params...)
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

// deleteJournalRows removes exactly the ids step 2 uploaded, plus their
// embedding vectors.
//
// The vectors go too, and must: node_vectors is keyed by node id, so leaving
// them behind would let recall return a hit whose row no longer exists -- a
// memory with no evidence, which is worse than a missing one.
//
// staged-data: MUST-NOT-GATE -- this is a DELETE of rows the pass above has
// already archived, addressed by the exact ids that uploaded. Gating it would
// leave a row whose only copy is now in blob storage present in the table
// forever while the archive claims it was retired, so the two records would
// disagree about what the cluster holds -- and the archive, not the gate, is
// what a restore reads.
func (i *Integration) deleteJournalRows(ctx context.Context, byConcept map[string][]string) (int, error) {
	if len(byConcept) == 0 {
		return 0, nil
	}
	if i.bunDB == nil || i.bunDB() == nil {
		return 0, fmt.Errorf("work: the retention sweep needs a database handle")
	}
	db := i.bunDB()
	total := 0
	for concept, ids := range byConcept {
		for _, batch := range chunk(ids, sweepPageSize) {
			if len(batch) == 0 {
				continue
			}
			args, placeholders := inList(batch, 2)
			res, err := db.ExecContext(ctx,
				`DELETE FROM "MemoryNodes" WHERE concept = $1 AND id IN (`+placeholders+`)`,
				append([]any{concept}, args...)...)
			if err != nil {
				return total, fmt.Errorf("work: delete %s: %w", concept, err)
			}
			n, _ := res.RowsAffected()
			total += int(n)

			vargs, vplaceholders := inList(batch, 1)
			if _, err := db.ExecContext(ctx,
				`DELETE FROM node_vectors WHERE id IN (`+vplaceholders+`)`, vargs...); err != nil {
				// The row is gone and its vector is not. Loud, and not
				// fatal: a stale vector is a recall hit that resolves to
				// nothing, which the recall join already drops.
				i.log().Warn("work: a journal row was deleted but its embedding was not",
					"component", "work.retention", "concept", concept, "err", err)
			}
		}
	}
	return total, nil
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

func chunk(ids []string, size int) [][]string {
	if size <= 0 {
		return [][]string{ids}
	}
	var out [][]string
	for start := 0; start < len(ids); start += size {
		out = append(out, ids[start:min(start+size, len(ids))])
	}
	return out
}

// inList builds ($n, $n+1, ...) placeholders starting at `from`, with the
// matching args. Parameterized rather than interpolated: a row id is a value,
// and a value never belongs in the statement text.
func inList(ids []string, from int) ([]any, string) {
	args := make([]any, 0, len(ids))
	var b strings.Builder
	for n, idv := range ids {
		if n > 0 {
			b.WriteString(", ")
		}
		b.WriteString("$")
		b.WriteString(strconv.Itoa(from + n))
		args = append(args, idv)
	}
	return args, b.String()
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
