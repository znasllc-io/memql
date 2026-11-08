package pipelinerun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

type artifactRetentionScan struct {
	owner, work, after, lease string
	// installation: the producer is an installation preparation's source run,
	// not a pipeline's work run (see claimArtifactRetentionScan).
	installation bool
}

// claimArtifactRetentionScan leases the least recently visited producer that
// still has work. Two kinds of producer share the upload journal. A pipeline
// run's steps upload under its v1:work:run, and the pass below resolves that
// run's window. An installation preparation's source captures and their step
// logs upload under the preparation's own source run
// (installation_preparations.source_run_id, matched with its operator), which
// is no work run: no window can be resolved for it, and only the
// preparation's cleanup recipe may fence and retire those files
// (integrations/installation). Such a producer is claimed only while a file
// that cleanup retired still has Library entries, which the pass removes.
// Claiming it for live files was a pass that failed on every rotation, for
// every preparation, forever (found on production 2026-11-08).
func claimArtifactRetentionScan(ctx context.Context, db *sql.DB) (*artifactRetentionScan, error) {
	s := &artifactRetentionScan{lease: id.NewShortId()}
	err := db.QueryRowContext(ctx, `WITH next AS (
 SELECT owner_user_id,work_run_id FROM pipeline_artifact_retention_queue q
 WHERE lease_until<clock_timestamp() AND EXISTS (
  SELECT 1 FROM pipeline_artifact_uploads a WHERE a.owner_user_id=q.owner_user_id
   AND a.identity->>'WorkRunID'=q.work_run_id AND (a.state<>'retired' OR a.library_purged_at IS NULL)
   AND (a.state='retired' OR NOT EXISTS (SELECT 1 FROM installation_preparations p
    WHERE p.source_run_id=q.work_run_id AND p.requested_by=q.owner_user_id)))
 ORDER BY visited_at,owner_user_id,work_run_id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE pipeline_artifact_retention_queue q SET lease_id=$1,
 lease_until=clock_timestamp()+interval '5 minutes',visited_at=clock_timestamp()
FROM next WHERE q.owner_user_id=next.owner_user_id AND q.work_run_id=next.work_run_id
RETURNING q.owner_user_id,q.work_run_id,q.after_intent_id,EXISTS (SELECT 1 FROM installation_preparations p
 WHERE p.source_run_id=q.work_run_id AND p.requested_by=q.owner_user_id)`, s.lease).Scan(&s.owner, &s.work, &s.after, &s.installation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// holdsArtifactRetentionLease is checked immediately before a page's
// effects: the lease may be renewed only by a new pass after this bounded
// context ends, so a pass that outlived its lease must not act.
func holdsArtifactRetentionLease(ctx context.Context, db *sql.DB, scan *artifactRetentionScan) error {
	var held bool
	if err := db.QueryRowContext(ctx, `SELECT lease_id=$3 AND lease_until>clock_timestamp()
FROM pipeline_artifact_retention_queue WHERE owner_user_id=$1 AND work_run_id=$2`, scan.owner, scan.work, scan.lease).Scan(&held); err != nil {
		return err
	}
	if !held {
		return errors.New("artifact cleanup lease was lost")
	}
	return nil
}

// purgeRetiredInstallationArtifacts is the whole pass for an installation
// preparation's producer: one bounded page of files its own cleanup already
// retired, whose Library entries still point at zero-byte tombstones. No
// window applies, because the bytes are gone, and there is no pipeline policy
// snapshot to consult. It never fences or retires (artifactRetentionScope
// refuses both on such a page), and the Library store re-confirms each
// permanent tombstone before deleting anything. It reads no work journal and
// keeps no cursor: a removed file leaves the page's predicate, and an
// unconfirmed removal stops the page, so the next pass, on any replica,
// starts again from the journal. Preparations cancelled before their cleanup
// removed Library entries itself are reached only this way.
func (i *Integration) purgeRetiredInstallationArtifacts(ctx context.Context, d Deps, db *sql.DB, scan *artifactRetentionScan) (map[string]any, error) {
	if d.ArtifactRetention.Library == nil {
		return nil, errors.New("native pipeline artifact retention is unavailable")
	}
	// ctx carries the internal origin SweepArtifacts requires, and borrowing
	// the owner's actor keeps it: the Library store needs both, and this pass
	// adds no stamp of its own (internal_origin_test.go).
	ownerCtx := memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, scan.owner))
	items, more, err := readArtifactJournalPage(ownerCtx, db, scan.owner, scan.work, "", retiredAwaitingLibraryCleanup)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		// Read retired from the journal; PurgeRetiredRunFile still requires
		// the confirmed tombstone and its lease before it removes anything.
		item.Retired = true
	}
	result := ArtifactRetentionResult{WorkRunID: scan.work, Inspected: len(items), HasMore: more}
	if len(items) > 0 {
		result.AfterID = items[len(items)-1].ID
	}
	if err := holdsArtifactRetentionLease(ownerCtx, db, scan); err != nil {
		return nil, err
	}
	scope := &artifactRetentionScope{library: d.ArtifactRetention.Library, items: items, result: result, retiredElsewhere: true}
	if _, err := workflowhost.Run(ownerCtx, "purgeRetiredPipelineArtifacts", nil, workflowhost.Options{
		Load: d.LoadWorkflow, Operations: scope.operations(), Logger: d.Logger,
	}); err != nil {
		return nil, err
	}
	if !scope.observed {
		return nil, errors.New("artifact cleanup recipe did not report native outcomes")
	}
	return retentionResultMap(scope.result), nil
}

func (i *Integration) handleSweepArtifacts(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	out, err := i.SweepArtifacts(handlerContext(ctx))
	if err != nil {
		return nil, err
	}
	return resultNode(out), nil
}

// SweepArtifacts admits a single leased producer to the installed policy.
// The durable cursor provides fairness and recovery; no caller supplies an
// owner to impersonate, a work identity, or an age threshold.
func (i *Integration) SweepArtifacts(ctx context.Context) (map[string]any, error) {
	if err := requireInternalOrigin(ctx, "sweepArtifacts"); err != nil {
		return nil, err
	}
	d := i.snapshot()
	if d.ArtifactRetention == nil || d.ArtifactRetention.Database == nil || d.ArtifactRetention.Engine == nil || d.Store == nil {
		return nil, errors.New("native pipeline artifact retention is unavailable")
	}
	db := d.ArtifactRetention.Database()
	if db == nil {
		return nil, errors.New("artifact retention journal is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	scan, err := claimArtifactRetentionScan(ctx, db)
	if err != nil {
		return nil, err
	}
	if scan == nil {
		return map[string]any{"inspected": 0}, nil
	}
	// A failed pass leaves its cursor unchanged. Its new visit time lets other
	// producers proceed, and the lease's expiry recovers a vanished process.
	defer func() {
		c, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_, e := db.ExecContext(c, `UPDATE pipeline_artifact_retention_queue SET lease_until='epoch',lease_id=''
WHERE owner_user_id=$1 AND work_run_id=$2 AND lease_id=$3`, scan.owner, scan.work, scan.lease)
		if e != nil {
			d.Logger.Warn("pipeline artifact cleanup lease release failed", "error", e)
		}
	}()
	if scan.installation {
		return i.purgeRetiredInstallationArtifacts(ctx, d, db, scan)
	}
	ownerCtx := memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, scan.owner))
	raw, err := d.ArtifactRetention.Engine.Execute(ownerCtx, "query workRunForOwner(runId: "+langparser.QuoteString(scan.work)+")")
	if err != nil {
		return nil, err
	}
	rows := rowsOf(raw)
	if len(rows) != 1 || memql.BareShortId(fmt.Sprint(rows[0]["ownerUserId"])) != scan.owner || memql.BareShortId(fmt.Sprint(rows[0]["id"])) != scan.work {
		return nil, errors.New("artifact maintenance cannot resolve its owner-scoped work journal")
	}
	input, ok := rows[0]["input"].(map[string]any)
	if !ok {
		return nil, errors.New("artifact maintenance requires its pipeline input")
	}
	runID, ok := input["pipelineRunId"].(string)
	if !ok || runID == "" {
		return nil, errors.New("artifact maintenance requires its pipeline identity")
	}
	run, err := d.Store.RunForOwner(ownerCtx, runID)
	if err != nil {
		return nil, err
	}
	if run == nil || memql.BareShortId(run.OwnerUserID) != scan.owner || run.WorkRunID != scan.work || input["pipelineId"] != run.PipelineID {
		return nil, errors.New("artifact maintenance run differs from its producing work")
	}
	terminal := run.Status == StatusCompleted && !run.FinishedAt.IsZero() && retentionProducerStopped(ownerCtx, d.ArtifactRetention.Engine, scan.owner, *run) == nil
	ageSeconds := int64(0)
	if terminal {
		ageSeconds = max(int64(d.now().Sub(run.FinishedAt)/time.Second), 0)
	}
	answer := map[string]any{"runId": run.ID, "inspected": 0, "purged": 0, "bytes": int64(0)}
	called := false
	ops := map[string]workflowhost.Operation{
		"pipelineRetentionSweepPage": func(callCtx context.Context, args map[string]any) (any, error) {
			if len(args) != 0 || called || !terminal {
				return nil, errors.New("cleanup requires one terminal producer page")
			}
			called = true
			if err := holdsArtifactRetentionLease(callCtx, db, scan); err != nil {
				return nil, err
			}
			out, err := i.RetireArtifacts(callCtx, run.ID, scan.after, false)
			if err != nil {
				return nil, err
			}
			after := ""
			if out.HasMore {
				after = out.AfterID
			}
			res, err := db.ExecContext(callCtx, `UPDATE pipeline_artifact_retention_queue SET after_intent_id=$4
WHERE owner_user_id=$1 AND work_run_id=$2 AND lease_id=$3`, scan.owner, scan.work, scan.lease, after)
			if err != nil {
				return nil, err
			}
			if n, e := res.RowsAffected(); e != nil || n != 1 {
				return nil, errors.New("artifact cleanup cursor lease was lost")
			}
			answer = retentionResultMap(out)
			return answer, nil
		},
	}
	_, err = workflowhost.RunPhase(ownerCtx, run.PolicySnapshot, pipelinePolicyContract, "pipelineArtifactRetentionPolicy", map[string]any{
		"terminal": terminal, "ageSeconds": ageSeconds, "eventName": string(run.Event), "conclusion": run.Conclusion,
	}, workflowhost.Options{Load: d.LoadWorkflow, Operations: ops, Logger: d.Logger})
	if err != nil {
		return nil, err
	}
	return answer, nil
}
