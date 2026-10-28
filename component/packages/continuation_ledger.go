package packages

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/memql"
)

const continuationLease = 90 * time.Second
const continuationMaxBytes = 1 << 20

var errContinuationPending = errors.New("package continuation awaits exact rollout observation")
var errContinuationClaim = errors.New("package continuation no longer owns its live claim")

// The body is immutable. Mutable effect flags and receipts live in separate
// columns; a replacement cannot retarget source, site, bytes or rollout.
type continuationSelection struct {
	DeploymentID  string                               `json:"deploymentId"`
	PackageID     string                               `json:"packageId"`
	OwnerUserID   string                               `json:"ownerUserId"`
	RequestedBy   string                               `json:"requestedBy"`
	Source        map[string]string                    `json:"source"`
	SnapshotID    string                               `json:"snapshotId"`
	SourceVersion string                               `json:"sourceVersion"`
	Report        *Report                              `json:"report"`
	Request       DeployRequest                        `json:"request"`
	Outcome       DeployOutcome                        `json:"outcome"`
	Publications  []memql.PackagePublicationIntent     `json:"publications"`
	Domains       []memql.PackageDomainIntent          `json:"domains"`
	Bundles       []PreparedSiteBundle                 `json:"bundles"`
	ActiveBefore  ActiveSetVersion                     `json:"activeBefore"`
	ActiveAfter   map[string]string                    `json:"activeAfter"`
	Rollouts      []deploycontrol.PackageRolloutIntent `json:"rollouts"`
	Deadline      time.Time                            `json:"deadline"`
}
type continuationState struct {
	PointerAttempted bool              `json:"pointerAttempted"`
	PointerConfirmed *ActiveSetVersion `json:"pointerConfirmed,omitempty"`
	RollAttempted    []bool            `json:"rollAttempted"`
	RollConfirmed    []bool            `json:"rollConfirmed"`
}
type continuationRecord struct {
	Selection         continuationSelection
	SHA, Phase, Lease string
	State             continuationState
	Receipts          map[string]memql.PackagePublicationReceipt
	DomainReceipts    map[string]memql.PackageDomainReceipt
	CancelRequested   bool
}

func (s *store) continuationDB() (*sql.DB, error) {
	if s.directDB == nil || s.directDB() == nil {
		return nil, fmt.Errorf("package continuation requires native database")
	}
	return s.directDB(), nil
}
func encodeContinuation(v continuationSelection) ([]byte, string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	if len(raw) > continuationMaxBytes || v.DeploymentID == "" || v.PackageID == "" || v.RequestedBy == "" || v.SnapshotID == "" || v.SourceVersion == "" || v.Report == nil || len(v.Rollouts) == 0 || len(v.Rollouts) > 32 || len(v.Publications) != len(v.Bundles) || len(v.Publications) > 128 || v.Deadline.IsZero() {
		return nil, "", fmt.Errorf("invalid or oversized package continuation selection")
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}
func (s *store) createContinuation(ctx context.Context, v continuationSelection) (*continuationRecord, error) {
	db, err := s.continuationDB()
	if err != nil {
		return nil, err
	}
	raw, digest, err := encodeContinuation(v)
	if err != nil {
		return nil, err
	}
	lease := rand.Text()
	state := continuationState{RollAttempted: make([]bool, len(v.Rollouts)), RollConfirmed: make([]bool, len(v.Rollouts))}
	stateRaw, _ := json.Marshal(state)
	result, err := db.ExecContext(ctx, `INSERT INTO package_deployment_continuations (deployment_id,owner_user_id,requested_by,package_id,selection_sha256,body,phase,state,deadline,lease_id,lease_until) VALUES ($1,$2,$3,$4,$5,$6,'prepared',$7,$8,$9,clock_timestamp()+interval '90 seconds') ON CONFLICT DO NOTHING`, v.DeploymentID, v.OwnerUserID, v.RequestedBy, v.PackageID, digest, string(raw), string(stateRaw), v.Deadline, lease)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, fmt.Errorf("package continuation already exists; observe the original selection")
	}
	return &continuationRecord{Selection: v, SHA: digest, Phase: "prepared", Lease: lease, State: state, Receipts: map[string]memql.PackagePublicationReceipt{}, DomainReceipts: map[string]memql.PackageDomainReceipt{}}, nil
}
func (s *store) readContinuation(ctx context.Context, id string) (*continuationRecord, error) {
	db, err := s.continuationDB()
	if err != nil {
		return nil, err
	}
	var raw, state, receipts, domainReceipts []byte
	var r continuationRecord
	var owner, requester, pkg string
	err = db.QueryRowContext(ctx, `SELECT owner_user_id,requested_by,package_id,selection_sha256,body,phase,state,publications,domain_receipts,cancel_requested,lease_id FROM package_deployment_continuations WHERE deployment_id=$1`, memql.BareShortId(id)).Scan(&owner, &requester, &pkg, &r.SHA, &raw, &r.Phase, &state, &receipts, &domainReceipts, &r.CancelRequested, &r.Lease)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > continuationMaxBytes || json.Unmarshal(raw, &r.Selection) != nil || json.Unmarshal(state, &r.State) != nil || json.Unmarshal(receipts, &r.Receipts) != nil || json.Unmarshal(domainReceipts, &r.DomainReceipts) != nil {
		return nil, fmt.Errorf("invalid native continuation record")
	}
	// PostgreSQL jsonb normalizes key order and spacing. Re-encode the typed
	// immutable selection using the SAME canonical encoder as creation.
	_, digest, encodeErr := encodeContinuation(r.Selection)
	if encodeErr != nil || digest != r.SHA || r.Selection.DeploymentID != memql.BareShortId(id) || r.Selection.OwnerUserID != owner || r.Selection.RequestedBy != requester || r.Selection.PackageID != pkg || len(r.State.RollAttempted) != len(r.Selection.Rollouts) || len(r.State.RollConfirmed) != len(r.Selection.Rollouts) {
		return nil, fmt.Errorf("native continuation identity differs")
	}
	return &r, nil
}
func (s *store) claimContinuation(ctx context.Context, id string) (*continuationRecord, error) {
	db, err := s.continuationDB()
	if err != nil {
		return nil, err
	}
	lease := rand.Text()
	result, err := db.ExecContext(ctx, `UPDATE package_deployment_continuations SET lease_id=$2,lease_until=clock_timestamp()+interval '90 seconds',updated_at=clock_timestamp() WHERE deployment_id=$1 AND phase NOT IN ('completed','refused') AND lease_until<=clock_timestamp()`, memql.BareShortId(id), lease)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	r, err := s.readContinuation(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil || r.Lease != lease {
		return nil, errContinuationClaim
	}
	return r, nil
}
func (s *store) saveContinuation(ctx context.Context, r *continuationRecord, phase string) error {
	order := map[string]int{"prepared": 0, "rolling": 1, "publishing": 2, "completed": 3, "refused": 3}
	next, nextOK := order[phase]
	current, currentOK := order[r.Phase]
	if !nextOK || !currentOK || next < current {
		return fmt.Errorf("continuation phase cannot move backwards")
	}
	db, err := s.continuationDB()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(r.State)
	if err != nil {
		return err
	}
	if len(raw) > continuationMaxBytes {
		return fmt.Errorf("continuation state exceeds bound")
	}
	// The selected body/publications are never overwritten by a stale in-memory
	// copy. All claim writers take only this SQL row; none call graph mutations
	// while holding it (the publication transaction locks graph then journal).
	result, err := db.ExecContext(ctx, `UPDATE package_deployment_continuations SET phase=$4,state=$5,lease_until=clock_timestamp()+interval '90 seconds',updated_at=clock_timestamp() WHERE deployment_id=$1 AND selection_sha256=$2 AND lease_id=$3 AND lease_until>clock_timestamp() AND phase=$6 AND phase NOT IN ('completed','refused')`, r.Selection.DeploymentID, r.SHA, r.Lease, phase, string(raw), r.Phase)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errContinuationClaim
	}
	r.Phase = phase
	return nil
}
func (s *store) releaseContinuation(ctx context.Context, r *continuationRecord) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	db, err := s.continuationDB()
	if err != nil {
		return
	}
	_, _ = db.ExecContext(ctx, `UPDATE package_deployment_continuations SET lease_until='epoch',updated_at=clock_timestamp() WHERE deployment_id=$1 AND lease_id=$2`, r.Selection.DeploymentID, r.Lease)
}

func (s *store) renewContinuation(ctx context.Context, r *continuationRecord) error {
	db, err := s.continuationDB()
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE package_deployment_continuations SET lease_until=clock_timestamp()+interval '90 seconds',updated_at=clock_timestamp() WHERE deployment_id=$1 AND lease_id=$2 AND lease_until>clock_timestamp() AND deadline>clock_timestamp() AND phase NOT IN ('completed','refused')`, r.Selection.DeploymentID, r.Lease)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errContinuationClaim
	}
	return nil
}
