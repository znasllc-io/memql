package release

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"

	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// candidateLedger is native authority storage, not a workflow. The preparer
// writes preparing before pinning artifacts, then ready only after verified
// inputs. Approval is a separate owner request naming this exact digest. These
// methods are deliberately unregistered: a DSL argument cannot claim readiness.
type candidateLedger struct{ db func() *sql.DB }

type candidateRecord struct {
	ID         string              `json:"candidateId"`
	Manifest   pl.ReleaseCandidate `json:"manifest"`
	State      string              `json:"state"`
	ApprovalID string              `json:"approvalId,omitempty"`
}

func candidateOwner(ctx context.Context) (string, error) {
	actor, err := requireOwner(ctx)
	if err != nil {
		return "", err
	}
	owner := memql.BareShortId(actor.UserId)
	if owner == "" {
		return "", errors.New("release candidate requires an identified owner")
	}
	return owner, nil
}

func (l *candidateLedger) database() (*sql.DB, error) {
	if l == nil || l.db == nil {
		return nil, errors.New("release candidate journal is unavailable")
	}
	db := l.db()
	if db == nil {
		return nil, errors.New("release candidate journal is unavailable")
	}
	return db, nil
}

// prepare records exact immutable inputs before artifact pinning or any remote
// write. An interrupted preparer can resume the same candidate on another node.
// Its existence is not evidence of readiness or approval.
func (l *candidateLedger) prepare(ctx context.Context, c pl.ReleaseCandidate) (candidateRecord, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateRecord{}, err
	}
	if c.OwnerUserID != owner {
		return candidateRecord{}, errors.New("release candidate belongs to another owner")
	}
	body, key, err := pl.CanonicalReleaseCandidate(c)
	if err != nil {
		return candidateRecord{}, err
	}
	db, err := l.database()
	if err != nil {
		return candidateRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return candidateRecord{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO release_candidates(candidate_id,owner_user_id,manifest,state)
VALUES($1,$2,$3::jsonb,'preparing') ON CONFLICT(candidate_id) DO NOTHING`, key, owner, string(body))
	if err != nil {
		return candidateRecord{}, err
	}
	rec, err := readCandidate(ctx, tx, owner, key)
	if err != nil {
		return candidateRecord{}, err
	}
	if rec.State == "retired" {
		return candidateRecord{}, errors.New("release candidate was permanently retired")
	}
	if err := tx.Commit(); err != nil {
		return candidateRecord{}, err
	}
	return rec, nil
}

type candidateQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Recompute canonical identity on every authority read. Neither corrupted
// storage nor an accidentally broadened writer may reuse an old approval for a
// changed body. Unknown JSON fields refuse, rather than disappearing in decode.
func readCandidate(ctx context.Context, q candidateQuerier, owner, key string) (candidateRecord, error) {
	rec := candidateRecord{ID: key}
	var body []byte
	err := q.QueryRowContext(ctx, `SELECT manifest,state FROM release_candidates
WHERE candidate_id=$1 AND owner_user_id=$2 FOR UPDATE`, key, owner).Scan(&body, &rec.State)
	if err != nil {
		return candidateRecord{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec.Manifest); err != nil {
		return candidateRecord{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return candidateRecord{}, errors.New("candidate manifest has trailing data")
	}
	_, actual, err := pl.CanonicalReleaseCandidate(rec.Manifest)
	if err != nil || actual != key || rec.Manifest.OwnerUserID != owner {
		return candidateRecord{}, errors.New("release candidate journal identity is inconsistent")
	}
	return rec, nil
}

// ready is called only by the trusted preparer after receipts, source,
// compatibility, operator targets and artifact bytes are verified and pinned.
// Repeating it never resets approval; retirement cannot be undone.
func (l *candidateLedger) ready(ctx context.Context, key string) (candidateRecord, error) {
	return l.change(ctx, key, false, false)
}

func (l *candidateLedger) approve(ctx context.Context, key string) (candidateRecord, error) {
	return l.change(ctx, key, true, false)
}

// Retire is a permanent fence for a candidate not yet handed to a publisher.
// A publication intent prevents retirement, including after a lost response;
// retention cannot release its artifacts while an external effect is uncertain.
func (l *candidateLedger) retire(ctx context.Context, key string) (candidateRecord, error) {
	return l.change(ctx, key, false, true)
}

func (l *candidateLedger) change(ctx context.Context, key string, approve, retire bool) (candidateRecord, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateRecord{}, err
	}
	db, err := l.database()
	if err != nil {
		return candidateRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return candidateRecord{}, err
	}
	defer tx.Rollback()
	rec, err := readCandidate(ctx, tx, owner, key)
	if err != nil {
		return candidateRecord{}, err
	}
	if retire {
		var effects bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM release_publication_intents WHERE candidate_id=$1)`, key).Scan(&effects); err != nil {
			return candidateRecord{}, err
		}
		if effects {
			return candidateRecord{}, errors.New("candidate has publication intents; preserve its evidence and reconcile publication")
		}
		rec.State = "retired"
	} else if rec.State == "retired" {
		return candidateRecord{}, errors.New("release candidate was permanently retired")
	} else if approve {
		if rec.State != "ready" && rec.State != "approved" {
			return candidateRecord{}, errors.New("candidate has not completed verification")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO release_candidate_approvals(candidate_id,approval_id,approved_by)
VALUES($1,$2,$3) ON CONFLICT(candidate_id) DO NOTHING`, key, id.NewShortId(), owner)
		if err != nil {
			return candidateRecord{}, err
		}
		var approvedBy string
		err = tx.QueryRowContext(ctx, `SELECT approval_id,approved_by FROM release_candidate_approvals WHERE candidate_id=$1`, key).Scan(&rec.ApprovalID, &approvedBy)
		if err != nil {
			return candidateRecord{}, err
		}
		if rec.ApprovalID == "" || approvedBy != owner {
			return candidateRecord{}, errors.New("candidate approval identity is inconsistent")
		}
		rec.State = "approved"
	} else if rec.State == "preparing" {
		rec.State = "ready"
	}
	_, err = tx.ExecContext(ctx, `UPDATE release_candidates SET state=$2,updated_at=clock_timestamp() WHERE candidate_id=$1`, key, rec.State)
	if err != nil {
		return candidateRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return candidateRecord{}, err
	}
	return rec, nil
}
