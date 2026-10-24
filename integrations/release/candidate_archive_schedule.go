package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// Enrollment is explicit and owner-only. Neither a graph row nor a maintenance
// actor can create the persisted authority used by the scheduled operation.
func (l *candidateLedger) setArchiveSchedule(ctx context.Context, enabled bool) error {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return err
	}
	grant, err := auth.CaptureExecutionAuthority(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	db, err := l.database()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO release_input_archive_schedules(owner_user_id,authority,enabled)
VALUES($1,$2::jsonb,$3) ON CONFLICT(owner_user_id) DO UPDATE SET authority=EXCLUDED.authority,enabled=EXCLUDED.enabled`, owner, string(body), enabled)
	return err
}

type candidateArchiveScan struct {
	owner, lease, after, candidate string
	grant                          map[string]any
	afterCreated, created, now     time.Time
}

func claimCandidateArchiveScan(ctx context.Context, db *sql.DB) (*candidateArchiveScan, error) {
	scan := &candidateArchiveScan{lease: id.NewShortId()}
	var authority []byte
	err := db.QueryRowContext(ctx, `WITH next AS (
 SELECT owner_user_id FROM release_input_archive_schedules
 WHERE enabled AND lease_until<clock_timestamp()
 ORDER BY visited_at,owner_user_id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE release_input_archive_schedules s SET lease_id=$1,
 lease_until=clock_timestamp()+interval '6 minutes',visited_at=clock_timestamp()
FROM next WHERE s.owner_user_id=next.owner_user_id
RETURNING s.owner_user_id,s.authority,s.after_created_at,s.after_candidate_id`, scan.lease).Scan(&scan.owner, &authority, &scan.afterCreated, &scan.after)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := decodeCandidateObject(authority, &scan.grant); err != nil || len(scan.grant) == 0 {
		return nil, errors.New("archive schedule has no captured owner authority")
	}
	return scan, nil
}

func nextCandidateArchive(ctx context.Context, db *sql.DB, scan *candidateArchiveScan) error {
	// The catalog's immutable creation position survives ordinary candidate
	// updates. Completed archived inputs leave this scan, but their catalog and
	// publication history remain available for installation and diagnosis.
	err := db.QueryRowContext(ctx, `SELECT c.candidate_id,p.created_at,clock_timestamp()
FROM release_candidates c JOIN release_catalog p ON p.candidate_id=c.candidate_id
LEFT JOIN release_candidate_archives a ON a.candidate_id=c.candidate_id
WHERE c.owner_user_id=$1 AND c.state IN ('approved','archived')
 AND a.inputs_released_at IS NULL AND (p.created_at,c.candidate_id)>($2::timestamptz,$3)
ORDER BY p.created_at,c.candidate_id LIMIT 1`, scan.owner, scan.afterCreated, scan.after).Scan(&scan.candidate, &scan.created, &scan.now)
	if errors.Is(err, sql.ErrNoRows) {
		scan.afterCreated, scan.after = time.Unix(0, 0).UTC(), ""
		return nil
	}
	if err == nil {
		scan.afterCreated, scan.after = scan.created, scan.candidate
	}
	return err
}

func finishCandidateArchiveScan(ctx context.Context, db *sql.DB, scan *candidateArchiveScan) error {
	result, err := db.ExecContext(ctx, `UPDATE release_input_archive_schedules
SET after_created_at=$3,after_candidate_id=$4,lease_id='',lease_until='epoch'
WHERE owner_user_id=$1 AND lease_id=$2`, scan.owner, scan.lease, scan.afterCreated, scan.after)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("archive schedule lease was lost")
	}
	return nil
}

func (i *Integration) handleArchiveSchedule(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	enabled, ok := args["enabled"].(bool)
	if !ok {
		return nil, errors.New("archive scheduling requires an explicit enabled boolean")
	}
	if err := p.ledger.setArchiveSchedule(ctx, enabled); err != nil {
		return nil, err
	}
	return resultNode("archiveSchedule", "", map[string]any{"enabled": enabled})
}

func (i *Integration) handleSweepPublishedInputs(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if auth.OriginFromContext(ctx) != auth.OriginInternal {
		return nil, errors.New("published input maintenance requires internal origin")
	}
	deps := i.candidateDeps.Load()
	if deps == nil || deps.Database == nil || i.store == nil || i.store.engine == nil {
		return nil, errors.New("published input maintenance is unavailable")
	}
	db := deps.Database()
	if db == nil {
		return nil, errors.New("published input maintenance journal is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	scan, err := claimCandidateArchiveScan(ctx, db)
	if err != nil {
		return nil, err
	}
	if scan == nil {
		return resultNode("archiveSweep", "", map[string]any{"inspected": 0})
	}
	defer func() {
		finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if err := finishCandidateArchiveScan(finish, db, scan); err != nil {
			i.logger.Warn("published input archive cursor release failed", "error", err)
		}
	}()
	resolver := auth.NewIdentityResolver(auth.QueryRunnerFunc(func(c context.Context, query string) (any, error) {
		result, err := i.store.engine.Execute(memql.ContextWithFreshRead(c), query)
		if err != nil || result == nil {
			return nil, err
		}
		return result.OutputPayload(), nil
	}), i.logger)
	ownerCtx, err := auth.ContextWithPersistedOwner(ctx, "v1:identity:user:"+scan.owner, scan.grant, resolver)
	if err != nil {
		return nil, err
	}
	owner, err := candidateOwner(ownerCtx)
	if err != nil || owner != scan.owner {
		return nil, errors.New("published input maintenance owner authority is no longer available")
	}
	if err := nextCandidateArchive(ownerCtx, db, scan); err != nil {
		return nil, err
	}
	if scan.candidate == "" {
		return resultNode("archiveSweep", "", map[string]any{"inspected": 0})
	}
	p, err := i.configuredCandidateFor(ownerCtx, scan.candidate)
	if err != nil {
		return nil, err
	}
	_, config, trust, err := i.catalog(ownerCtx)
	if err != nil {
		return nil, err
	}
	called := false
	_, err = workflowhost.Run(ownerCtx, "releasePublishedInputRetentionPolicy", map[string]any{
		"ageSeconds": max(int64(scan.now.Sub(scan.created)/time.Second), 0),
	}, workflowhost.Options{Logger: i.logger, Operations: map[string]workflowhost.Operation{
		"releaseRetentionArchiveInputs": func(callCtx context.Context, args map[string]any) (any, error) {
			if called || len(args) != 0 {
				return nil, errors.New("archive maintenance permits one bound candidate per pass")
			}
			called = true
			var held bool
			if err := db.QueryRowContext(callCtx, `SELECT enabled AND lease_id=$2 AND lease_until>clock_timestamp()
FROM release_input_archive_schedules WHERE owner_user_id=$1`, scan.owner, scan.lease).Scan(&held); err != nil || !held {
				return nil, errors.New("archive maintenance no longer holds its enabled lease")
			}
			_, err := p.archive(callCtx, scan.candidate, config.Publisher, trust)
			return map[string]any{"archived": err == nil}, err
		},
	}})
	if err != nil {
		return nil, err
	}
	return resultNode("archiveSweep", scan.candidate, map[string]any{"inspected": 1, "archived": called})
}
