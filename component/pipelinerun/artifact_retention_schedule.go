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
}

func claimArtifactRetentionScan(ctx context.Context, db *sql.DB) (*artifactRetentionScan, error) {
	s := &artifactRetentionScan{lease: id.NewShortId()}
	err := db.QueryRowContext(ctx, `WITH next AS (
 SELECT owner_user_id,work_run_id FROM pipeline_artifact_retention_queue q
 WHERE lease_until<clock_timestamp() AND EXISTS (
  SELECT 1 FROM pipeline_artifact_uploads a WHERE a.owner_user_id=q.owner_user_id
   AND a.identity->>'WorkRunID'=q.work_run_id AND (a.state<>'retired' OR a.library_purged_at IS NULL))
 ORDER BY visited_at,owner_user_id,work_run_id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE pipeline_artifact_retention_queue q SET lease_id=$1,
 lease_until=clock_timestamp()+interval '5 minutes',visited_at=clock_timestamp()
FROM next WHERE q.owner_user_id=next.owner_user_id AND q.work_run_id=next.work_run_id
RETURNING q.owner_user_id,q.work_run_id,q.after_intent_id`, s.lease).Scan(&s.owner, &s.work, &s.after)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
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
			// The lease may be renewed only by a new pass after this bounded
			// context ends; check its ownership immediately before effects.
			var held bool
			if err := db.QueryRowContext(callCtx, `SELECT lease_id=$3 AND lease_until>clock_timestamp()
FROM pipeline_artifact_retention_queue WHERE owner_user_id=$1 AND work_run_id=$2`, scan.owner, scan.work, scan.lease).Scan(&held); err != nil {
				return nil, err
			}
			if !held {
				return nil, errors.New("artifact cleanup lease was lost")
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
	_, err = workflowhost.Run(ownerCtx, "pipelineArtifactRetentionPolicy", map[string]any{
		"terminal": terminal, "ageSeconds": ageSeconds, "eventName": string(run.Event), "conclusion": run.Conclusion,
	}, workflowhost.Options{Load: d.LoadWorkflow, Operations: ops, Logger: d.Logger})
	if err != nil {
		return nil, err
	}
	return answer, nil
}
