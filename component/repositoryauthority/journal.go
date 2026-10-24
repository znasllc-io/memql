package repositoryauthority

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type record struct {
	MergeGroupReports           map[string]mergeGroupReviewRecord
	ID                          string
	Fingerprint                 string
	Request                     Request
	Requester                   string
	Source                      Source
	RepositoryID                int64
	Route                       route
	Maintainer                  Source
	RequesterCanWrite           bool
	GoalPublished               bool
	Immediate                   bool
	RemoteRequestID             string
	GoalID, RunID, ApprovalID   string
	Proposal                    *Proposal
	ProposalHash, ProposalState string
	ProposalCreatedAt           time.Time
	ProposalReceipt             *ProposalReceipt
	ReviewInput                 *ReviewInput
	ReviewHash                  string
	ReviewReceipt               *ReviewReceipt
	ReviewReportIntent          *ReviewReportIntent
	ReviewReportReceipt         *ReviewReportReceipt
	DeliveryState               string
	DeliveryAdmitted            bool
	DeliveryReceipt             *DeliveryReceipt
	DeliveryLease               string
	DeliveryLeaseUntil          time.Time
	State, Reason               string
	CreatedAt, ExpiresAt        time.Time
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (r record) receipt(actor string) Receipt {
	out := Receipt{ReviewReport: r.ReviewReportReceipt, Proposal: r.ProposalReceipt, Operation: r.Request.Operation, Review: r.ReviewReceipt, Delivery: r.DeliveryReceipt, ID: r.ID, Repository: r.Source.Repository, RepositoryID: r.RepositoryID, Statement: r.Request.Statement, Paths: r.Request.Paths, State: r.State, Reason: r.Reason, GoalID: r.GoalID, RunID: r.RunID, ApprovalID: r.ApprovalID, ExpiresAt: r.ExpiresAt.UTC().Format(time.RFC3339Nano), ParentRequestID: r.Request.ParentRequestID}
	// Nexus IDs belong to this installation and this owner. A request status
	// is shareable with its requester; the maintainer's private journal is not.
	if r.Route.Remote != "" || !same(actor, r.Maintainer.OwnerUserID) {
		out.GoalID, out.RunID, out.ApprovalID = "", "", ""
	}
	return out
}
func (i *Integration) database() (*sql.DB, error) {
	db := i.deps().DB
	if db == nil {
		return nil, errors.New("repository change journal is unavailable")
	}
	return db, nil
}
func (i *Integration) get(ctx context.Context, id, actor string) (record, error) {
	var r record
	db, err := i.database()
	if err != nil {
		return r, err
	}
	var b []byte
	// Owner predicates precede decoding. Cluster role alone cannot read a request.
	query := `SELECT body FROM repository_change_requests WHERE id=$1`
	args := []any{id}
	if actor != "" {
		query += ` AND (requester=$2 OR maintainer=$2)`
		args = append(args, actor)
	}
	if err = db.QueryRowContext(ctx, query, args...).Scan(&b); err != nil {
		return r, err
	}
	err = json.Unmarshal(b, &r)
	return r, err
}
func (i *Integration) create(ctx context.Context, r record) (record, error) {
	db, err := i.database()
	if err != nil {
		return r, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO repository_change_requests(id,requester,maintainer,source_id,fingerprint,body,created_at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7) ON CONFLICT(id) DO NOTHING`, r.ID, r.Requester, r.Maintainer.OwnerUserID, r.Request.SourceID, r.Fingerprint, string(b), r.CreatedAt)
	if err != nil {
		return r, err
	}
	existing, err := i.get(ctx, r.ID, r.Requester)
	if err != nil {
		return r, err
	}
	if existing.Fingerprint != r.Fingerprint {
		return r, errors.New("requestId was already used for a different immutable request")
	}
	return existing, nil
}
func (i *Integration) update(ctx context.Context, id string, fn func(*record) error) (record, error) {
	var r record
	db, err := i.database()
	if err != nil {
		return r, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	var b []byte
	if err = tx.QueryRowContext(ctx, `SELECT body FROM repository_change_requests WHERE id=$1 FOR UPDATE`, id).Scan(&b); err != nil {
		return r, err
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	if err = fn(&r); err != nil {
		return r, err
	}
	b, err = json.Marshal(r)
	if err != nil {
		return r, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE repository_change_requests SET body=$2::jsonb,updated_at=clock_timestamp() WHERE id=$1`, id, string(b))
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}
func terminal(state string) bool {
	return state == "revoked" || state == "declined" || state == "expired"
}
func (i *Integration) List(ctx context.Context, sourceID string) ([]Receipt, error) {
	actor, err := person(ctx, "read", "app:pipelines")
	if err != nil {
		return nil, err
	}
	d := i.deps()
	if d.Source == nil {
		return nil, errors.New("source access is unavailable")
	}
	if _, err = d.Source(ctx, sourceID, false); err != nil {
		return nil, err
	}
	db, err := i.database()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT body FROM repository_change_requests WHERE source_id=$1 AND (requester=$2 OR maintainer=$2) ORDER BY created_at DESC LIMIT 100`, sourceID, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Receipt{}
	for rows.Next() {
		var b []byte
		var r record
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		out = append(out, r.receipt(actor))
	}
	return out, rows.Err()
}
