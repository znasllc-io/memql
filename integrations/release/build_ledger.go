package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// Native rows retain the original selection and one-way effect fences. Neither
// editable graph rows nor a fresh replica can authorize a replacement POST.
func (l *candidateLedger) saveBuild(ctx context.Context, record releaseBuildRecord) (releaseBuildRecord, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return releaseBuildRecord{}, err
	}
	if err := record.validate(owner, record.ID); err != nil {
		return releaseBuildRecord{}, err
	}
	body, err := json.Marshal(record)
	if err != nil || len(body) > 1<<20 {
		return releaseBuildRecord{}, errors.New("release selection is too large")
	}
	db, err := l.database()
	if err != nil {
		return releaseBuildRecord{}, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO release_build_intents(intent_id,owner_user_id,body) VALUES($1,$2,$3::jsonb) ON CONFLICT(intent_id) DO NOTHING`, record.ID, owner, string(body))
	if err != nil {
		return releaseBuildRecord{}, err
	}
	return l.build(ctx, record.ID)
}

func decodeBuild(body []byte, owner, key string) (releaseBuildRecord, error) {
	var record releaseBuildRecord
	if len(body) > 1<<20 {
		return record, errors.New("oversized release journal")
	}
	if err := decodeCandidateObject(body, &record); err != nil {
		return record, err
	}
	return record, record.validate(owner, key)
}

func (l *candidateLedger) build(ctx context.Context, key string) (releaseBuildRecord, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return releaseBuildRecord{}, err
	}
	db, err := l.database()
	if err != nil {
		return releaseBuildRecord{}, err
	}
	var body []byte
	if err := db.QueryRowContext(ctx, `SELECT body FROM release_build_intents WHERE intent_id=$1 AND owner_user_id=$2`, key, owner).Scan(&body); err != nil {
		return releaseBuildRecord{}, err
	}
	return decodeBuild(body, owner, key)
}

func (l *candidateLedger) builds(ctx context.Context, limit int) ([]releaseBuildRecord, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("release history limit must be between 1 and 100")
	}
	db, err := l.database()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT intent_id,body FROM release_build_intents WHERE owner_user_id=$1 ORDER BY created_at DESC,intent_id LIMIT $2`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []releaseBuildRecord{}
	for rows.Next() {
		var key string
		var body []byte
		if err := rows.Scan(&key, &body); err != nil {
			return nil, err
		}
		record, err := decodeBuild(body, owner, key)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

type releaseBuildLease struct {
	ledger *candidateLedger
	record releaseBuildRecord
	token  string
}

// The lease is longer than the bounded workflow call. A crashed holder retains
// its attempted fences; expiry permits observation, never replay of a POST.
func (l *candidateLedger) claimBuild(ctx context.Context, key string) (*releaseBuildLease, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return nil, err
	}
	db, err := l.database()
	if err != nil {
		return nil, err
	}
	token := id.NewShortId()
	var body []byte
	err = db.QueryRowContext(ctx, `UPDATE release_build_intents SET lease_id=$3,lease_until=clock_timestamp()+interval '6 minutes'
WHERE intent_id=$1 AND owner_user_id=$2 AND lease_until<clock_timestamp() RETURNING body`, key, owner, token).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("release is unavailable or another replica is already handling it")
	}
	if err != nil {
		return nil, err
	}
	record, err := decodeBuild(body, owner, key)
	if err != nil {
		return nil, err
	}
	return &releaseBuildLease{l, record, token}, nil
}

func (s *releaseBuildLease) check(ctx context.Context) error {
	owner, err := candidateOwner(ctx)
	if err != nil || owner != s.record.Selection.Owner {
		return errors.New("release lease belongs to another owner")
	}
	db, err := s.ledger.database()
	if err != nil {
		return err
	}
	var present bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM release_build_intents WHERE intent_id=$1 AND owner_user_id=$2 AND lease_id=$3 AND lease_until>clock_timestamp())`, s.record.ID, owner, s.token).Scan(&present)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("release lease is no longer held")
	}
	return nil
}

func (s *releaseBuildLease) save(ctx context.Context) error {
	if err := s.record.validate(s.record.Selection.Owner, s.record.ID); err != nil {
		return err
	}
	s.record.UpdatedAt = time.Now().UTC()
	body, err := json.Marshal(s.record)
	if err != nil || len(body) > 1<<20 {
		return errors.New("invalid release journal update")
	}
	db, err := s.ledger.database()
	if err != nil {
		return err
	}
	// The last condition is the storage half of adoptableBuildRun
	// (build_adopt.go): the recorded run keeps its ID unless the new run's
	// chain names it as an attempt that finished without success, before any
	// candidate or publication. A replica holding an older view can therefore
	// never write an earlier run back over an adopted re-run.
	result, err := db.ExecContext(ctx, `UPDATE release_build_intents SET body=$4::jsonb,updated_at=clock_timestamp()
WHERE intent_id=$1 AND owner_user_id=$2 AND lease_id=$3 AND lease_until>clock_timestamp()
AND body->'selection'=$4::jsonb->'selection'
AND (NOT (body->>'tagAttempted')::boolean OR ($4::jsonb->>'tagAttempted')::boolean)
AND (NOT (body->>'tagConfirmed')::boolean OR ($4::jsonb->>'tagConfirmed')::boolean)
AND (NOT (body->>'runAttempted')::boolean OR ($4::jsonb->>'runAttempted')::boolean)
AND (NOT (body->>'publicationStarted')::boolean OR ($4::jsonb->>'publicationStarted')::boolean)
AND (COALESCE(body->>'candidateId','')='' OR body->>'candidateId'=$4::jsonb->>'candidateId')
AND (COALESCE(body->>'approvalId','')='' OR body->>'approvalId'=$4::jsonb->>'approvalId')
AND (body->'run' IS NULL OR body->'run'->>'ID'=$4::jsonb->'run'->>'ID'
  OR (COALESCE(body->>'candidateId','')='' AND NOT (body->>'publicationStarted')::boolean
    AND EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE($4::jsonb->'run'->'Chain','[]'::jsonb)) AS earlier
      WHERE earlier->>'ID'=body->'run'->>'ID' AND earlier->>'Status'='completed'
      AND (earlier->>'Conclusion'<>'success' OR (earlier->>'CancelRequested')::boolean))))`, s.record.ID, s.record.Selection.Owner, s.token, string(body))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("release update lost its immutable selection or lease")
	}
	return nil
}

func (s *releaseBuildLease) close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if db, err := s.ledger.database(); err == nil {
		_, _ = db.ExecContext(ctx, `UPDATE release_build_intents SET lease_id='',lease_until='epoch' WHERE intent_id=$1 AND owner_user_id=$2 AND lease_id=$3`, s.record.ID, s.record.Selection.Owner, s.token)
	}
}
