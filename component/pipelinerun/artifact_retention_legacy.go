package pipelinerun

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// artifact_retention_legacy.go -- step logs written before logs had upload
// receipts. Without a receipt the retention sweep never sees a file, so its
// bytes and Library rows would never expire. Adoption verifies the existing
// bytes and writes the receipt the file would have had; from then on the file
// is retired by pipelineArtifactRetentionPolicy like any other. Two callers
// share one verified adoption: a person naming one step
// (pipelinesAdoptLegacyLog), and the scheduled enrollment that works through
// the list the 20261108150000 migration seeded (enrollLegacyPipelineLogs).
// Neither uploads, deletes or retires anything.

const (
	// legacyLogEnrollmentPageSize bounds one pass: one producer's files, each
	// read back from storage and hashed before its receipt is written. Legacy
	// logs average about 26 KB and a producer holds about sixteen of them, so a
	// page is normally one whole producer and takes seconds.
	legacyLogEnrollmentPageSize = 32
	// legacyLogEnrollmentTries is how many failed passes a file gets before it
	// is recorded refused. A refusal ends the journal hold that kept its
	// producer provable (integrations/work retentionHeldBackSQL), so the bound
	// also bounds how long a file nobody can verify keeps that journal.
	legacyLogEnrollmentTries = 5
	// legacyLogEnrollmentReasonBytes bounds the stored refusal text.
	legacyLogEnrollmentReasonBytes = 512
)

func (i *Integration) handleAdoptLegacyLog(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	r, err := i.AdoptLegacyLog(handlerContext(ctx), stringArg(args, "runId"), stringArg(args, "stepKey"))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"intentId": r.IntentID, "fileId": r.FileID, "bytes": r.Size}), nil
}

// The caller selects a step, never an object URL or a file to delete. Its
// completed journal supplies the exact log identity and attempt. Adoption
// verifies bytes and enrolls a receipt; it performs no retirement itself.
func (i *Integration) AdoptLegacyLog(ctx context.Context, runID, key string) (pipelinesteps.StoredFileReceipt, error) {
	var zero pipelinesteps.StoredFileReceipt
	owner, err := personFrom(ctx)
	if err != nil {
		return zero, err
	}
	owner = memql.BareShortId(owner)
	if runID == "" || len(runID) > 512 || key == "" || len(key) > 512 {
		return zero, errors.New("legacy log adoption requires a run and step")
	}
	d := i.snapshot()
	if d.Store == nil || d.ArtifactRetention == nil || d.ArtifactRetention.Engine == nil || d.ArtifactRetention.Library == nil {
		return zero, errors.New("native pipeline artifact retention is unavailable")
	}
	ctx, cancel := context.WithTimeout(memql.ContextWithFreshRead(ctx), 2*time.Minute)
	defer cancel()
	r, err := d.Store.RunForOwner(ctx, runID)
	if err != nil {
		return zero, err
	}
	if r == nil || memql.BareShortId(r.OwnerUserID) != owner || r.Status != StatusCompleted {
		return zero, errors.New("log adoption requires the caller's completed pipeline")
	}
	if err := retentionProducerStopped(ctx, d.ArtifactRetention.Engine, owner, *r); err != nil {
		return zero, err
	}
	step, attempt, err := legacyLogStep(ctx, d.ArtifactRetention.Engine, owner, r.WorkRunID, key)
	if err != nil {
		return zero, err
	}
	file, ok := step["logFileId"].(string)
	if !ok || file == "" {
		return zero, errors.New("completed step has no retained log")
	}
	return adoptVerifiedLegacyLog(ctx, d, pipelinesteps.RunFileReceiptScope{OwnerUserID: owner, WorkRunID: r.WorkRunID, StepKey: key, Attempt: attempt}, file)
}

// legacyLogStep is the producing step as its owner's journal records it: the
// one row of this work run with this key, finished, and the attempt the
// receipt scope will name. Legacy logs predate step retries (2026-10-29), so
// that attempt is the step's only one.
func legacyLogStep(ctx context.Context, engine Engine, owner, work, key string) (map[string]any, int, error) {
	raw, err := engine.Execute(ctx, "query workStepForOwnerRun(runId: "+langparser.QuoteString(work)+", stepKey: "+langparser.QuoteString(key)+")")
	if err != nil {
		return nil, 0, err
	}
	rows := rowsOf(raw)
	if len(rows) != 1 {
		return nil, 0, errors.New("legacy log adoption requires its authorized step journal")
	}
	step := rows[0]
	if memql.BareShortId(fmt.Sprint(step["ownerUserId"])) != owner || memql.BareShortId(fmt.Sprint(step["runId"])) != memql.BareShortId(work) || step["key"] != key {
		return nil, 0, errors.New("log journal differs from its pipeline")
	}
	switch step["status"] {
	case "done", "failed", "cancelled":
	default:
		return nil, 0, errors.New("legacy log producer is not terminal")
	}
	// Decode the integer exactly; a fractional or oversized attempt is refused.
	data, err := json.Marshal(step["attempt"])
	var attempt int
	if err != nil || json.Unmarshal(data, &attempt) != nil || attempt < 1 {
		return nil, 0, errors.New("legacy log has no valid producer attempt")
	}
	return step, attempt, nil
}

// adoptVerifiedLegacyLog is the one internal-origin call both callers reach,
// after their owner-scoped reads proved the run, its closed journal and the
// step. The Library store then proves the file: the owner's own pipeline row
// naming this run and step, a bounded text log, and stored bytes that hash to
// the row's digest at the object path derived from owner, file and name.
func adoptVerifiedLegacyLog(ctx context.Context, d Deps, scope pipelinesteps.RunFileReceiptScope, file string) (pipelinesteps.StoredFileReceipt, error) {
	return d.ArtifactRetention.Library.AdoptLegacyRunLog(auth.ContextWithInternalOrigin(ctx), scope, memql.BareShortId(file))
}

func (i *Integration) handleEnrollLegacyLogs(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	out, err := i.EnrollLegacyLogs(handlerContext(ctx))
	if err != nil {
		return nil, err
	}
	return resultNode(out), nil
}

type legacyLogEnrollment struct {
	file, owner, work, step string
}

// EnrollLegacyLogs receipts one leased page of the pre-receipt step logs the
// migration listed: up to legacyLogEnrollmentPageSize files of one producer.
//
// WHY THE LIST AND NOT THE STEP. 2,500 of the 8,620 legacy logs on production
// (measured 2026-11-08) belong to steps that were cancelled or failed by the
// driver while the runner was storing their log, so no version of the step
// names the file; pipelinesAdoptLegacyLog, which reads the step's logFileId,
// cannot reach them. The file row's create-only producedByRunId and
// producedByStepKey, written server-side by recordStoredPipelineFile, are what
// bind such a log to its step, and the Library store checks both against the
// receipt scope before it verifies the bytes.
//
// WHAT IS STILL PROVED, under the owner's actor and never from the list row:
// the work journal and the pipeline run it names are the owner's, the run is
// completed and the journal closed (as the retention sweep requires), and the
// step is a finished row of that journal with a valid attempt.
//
// A FENCED SCOPE STILL TAKES THE RECEIPT. Retention fenced the scope of every
// step whose receipted artifacts it had retired, and 264 of the 555 legacy
// producers on production streamed such artifacts. The scope guard admits
// exactly adoption's insert, 'ready' on the file's own .memql-legacy-log/ path,
// and still refuses every producer admission (migration 20261108150100).
//
// ACROSS REPLICAS AND RESTARTS. The page is leased row by row (SKIP LOCKED, a
// five-minute lease beyond the four-minute pass), so a second replica takes
// other rows or nothing, and a pass that dies leaves its rows to the next one
// once the lease lapses. Adoption is idempotent: a file whose receipt was
// committed before a crash resolves to that receipt and its row is deleted
// then. A failed file is retried on later passes, behind every other producer,
// and recorded refused after legacyLogEnrollmentTries; a file its owner kept
// (filed, labelled, edited) is refused at once, as retention would keep it.
func (i *Integration) EnrollLegacyLogs(ctx context.Context) (map[string]any, error) {
	if err := requireInternalOrigin(ctx, "enrollLegacyLogs"); err != nil {
		return nil, err
	}
	d := i.snapshot()
	if d.ArtifactRetention == nil || d.ArtifactRetention.Database == nil || d.ArtifactRetention.Engine == nil ||
		d.ArtifactRetention.Library == nil || d.Store == nil {
		return nil, errors.New("native pipeline artifact retention is unavailable")
	}
	db := d.ArtifactRetention.Database()
	if db == nil {
		return nil, errors.New("legacy log enrollment journal is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	lease := id.NewShortId()
	page, err := claimLegacyLogPage(ctx, db, lease)
	if err != nil {
		return nil, err
	}
	if len(page) == 0 {
		return map[string]any{"inspected": 0}, nil
	}
	// Rows this pass never reached (its time ran out) go back unleased and
	// uncounted. Rows it settled no longer carry the lease.
	defer func() {
		c, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if _, e := db.ExecContext(c, `UPDATE pipeline_legacy_log_enrollment SET lease_id='',lease_until='epoch' WHERE lease_id=$1`, lease); e != nil {
			d.Logger.Warn("pipelines: legacy log enrollment lease release failed", "error", e)
		}
	}()
	owner, work := page[0].owner, page[0].work
	out := map[string]any{"workRunId": work, "inspected": len(page), "enrolled": 0, "refused": 0, "retrying": 0}
	ownerCtx := memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, owner))
	producerErr := legacyEnrollmentProducer(ownerCtx, d, owner, work)
	for _, e := range page {
		if ctx.Err() != nil {
			break
		}
		err := producerErr
		if err == nil {
			var attempt int
			if _, attempt, err = legacyLogStep(ownerCtx, d.ArtifactRetention.Engine, owner, work, e.step); err == nil {
				_, err = adoptVerifiedLegacyLog(ownerCtx, d, pipelinesteps.RunFileReceiptScope{OwnerUserID: owner, WorkRunID: work, StepKey: e.step, Attempt: attempt}, e.file)
			}
		}
		if ctx.Err() != nil {
			// The pass ran out of time during this file, so the failure is not
			// the file's: it goes back unleased and uncounted with the rest.
			break
		}
		outcome, recordErr := settleLegacyLogEnrollment(ctx, db, lease, e, err)
		if recordErr != nil {
			// The deferred release returns the row, uncounted, with the rest of
			// the page; a receipt already committed resolves to itself next pass.
			return nil, recordErr
		}
		out[outcome] = out[outcome].(int) + 1
		if outcome == "refused" {
			d.Logger.Warn("pipelines: a pre-receipt step log was not enrolled in retention; its bytes and Library rows stay",
				"fileId", e.file, "workRunId", work, "stepKey", e.step, "reason", boundedReason(err))
		}
	}
	return out, nil
}

// claimLegacyLogPage leases up to one page of the least recently visited
// producer's pending files. Visiting stamps the time first, so a producer that
// keeps failing waits behind every other one instead of holding the head.
func claimLegacyLogPage(ctx context.Context, db *sql.DB, lease string) ([]legacyLogEnrollment, error) {
	rows, err := db.QueryContext(ctx, `WITH producer AS (
 SELECT owner_user_id,work_run_id FROM pipeline_legacy_log_enrollment
 WHERE state='pending' AND lease_until<clock_timestamp()
 ORDER BY visited_at,owner_user_id,work_run_id LIMIT 1
), page AS (
 SELECT e.file_id FROM pipeline_legacy_log_enrollment e JOIN producer p
  ON p.owner_user_id=e.owner_user_id AND p.work_run_id=e.work_run_id
 WHERE e.state='pending' AND e.lease_until<clock_timestamp()
 ORDER BY e.file_id FOR UPDATE OF e SKIP LOCKED LIMIT $2
)
UPDATE pipeline_legacy_log_enrollment e SET lease_id=$1,
 lease_until=clock_timestamp()+interval '5 minutes',visited_at=clock_timestamp()
FROM page WHERE e.file_id=page.file_id
RETURNING e.file_id,e.owner_user_id,e.work_run_id,e.step_key`, lease, legacyLogEnrollmentPageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []legacyLogEnrollment
	for rows.Next() {
		var e legacyLogEnrollment
		if err := rows.Scan(&e.file, &e.owner, &e.work, &e.step); err != nil {
			return nil, err
		}
		page = append(page, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(page, func(a, b legacyLogEnrollment) int { return strings.Compare(a.file, b.file) })
	for _, e := range page {
		if e.owner != page[0].owner || e.work != page[0].work || e.file == "" || e.step == "" {
			return nil, errors.New("legacy log enrollment leased an inconsistent page")
		}
	}
	return page, nil
}

// legacyEnrollmentProducer resolves the listed producer under its owner the
// way the retention sweep resolves one: the owner's work journal names its
// pipeline run, the run is the owner's completed run of that journal, and the
// journal is closed. The list supplies identities to look up, never facts.
func legacyEnrollmentProducer(ctx context.Context, d Deps, owner, work string) error {
	raw, err := d.ArtifactRetention.Engine.Execute(ctx, "query workRunForOwner(runId: "+langparser.QuoteString(work)+")")
	if err != nil {
		return err
	}
	rows := rowsOf(raw)
	if len(rows) != 1 || memql.BareShortId(fmt.Sprint(rows[0]["ownerUserId"])) != owner || memql.BareShortId(fmt.Sprint(rows[0]["id"])) != work {
		return errors.New("legacy log enrollment cannot resolve its owner-scoped work journal")
	}
	input, _ := rows[0]["input"].(map[string]any)
	runID, _ := input["pipelineRunId"].(string)
	if runID == "" {
		return errors.New("legacy log enrollment requires its pipeline identity")
	}
	run, err := d.Store.RunForOwner(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil || memql.BareShortId(run.OwnerUserID) != owner || memql.BareShortId(run.WorkRunID) != work ||
		input["pipelineId"] != run.PipelineID || run.Status != StatusCompleted {
		return errors.New("legacy log enrollment requires the owner's completed pipeline run")
	}
	return retentionProducerStopped(ctx, d.ArtifactRetention.Engine, owner, *run)
}

// settleLegacyLogEnrollment records one file's outcome under this pass's
// lease: a receipt deletes the row (the receipt is the record from then on),
// a file its owner kept is refused at once, and any other failure counts a
// try, refusing the file once legacyLogEnrollmentTries is reached.
func settleLegacyLogEnrollment(ctx context.Context, db *sql.DB, lease string, e legacyLogEnrollment, failure error) (string, error) {
	if failure == nil {
		res, err := db.ExecContext(ctx, `DELETE FROM pipeline_legacy_log_enrollment WHERE file_id=$1 AND lease_id=$2`, e.file, lease)
		if err != nil {
			return "", err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return "", errors.New("legacy log enrollment lease was lost")
		}
		return "enrolled", nil
	}
	kept := errors.Is(failure, pipelinesteps.ErrArtifactRetained)
	var state string
	err := db.QueryRowContext(ctx, `UPDATE pipeline_legacy_log_enrollment SET tries=tries+1,reason=$3,
 state=CASE WHEN $4 OR tries+1>=$5 THEN 'refused' ELSE 'pending' END,lease_id='',lease_until='epoch'
WHERE file_id=$1 AND lease_id=$2 RETURNING state`, e.file, lease, boundedReason(failure), kept, legacyLogEnrollmentTries).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("legacy log enrollment lease was lost")
	}
	if err != nil {
		return "", err
	}
	if state == "refused" {
		return "refused", nil
	}
	return "retrying", nil
}

// boundedReason is the stored refusal text: valid UTF-8, which Postgres
// requires of text, cut at a character boundary.
func boundedReason(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToValidUTF8(err.Error(), "?")
	if len(s) > legacyLogEnrollmentReasonBytes {
		n := legacyLogEnrollmentReasonBytes
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s
}
