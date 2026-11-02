package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/githubrelease"
)

type candidateDraftRecord struct {
	IntentID    string                      `json:"intentId"`
	CandidateID string                      `json:"candidateId"`
	ApprovalID  string                      `json:"approvalId"`
	State       string                      `json:"state"`
	ReleaseID   int64                       `json:"releaseId,omitempty"`
	Targets     []string                    `json:"targets"`
	Receipt     *githubrelease.ReleaseState `json:"receipt,omitempty"`
}

func draftRecordTargets(plan candidateDraftPlan) []string {
	out := make([]string, 0, len(plan.Assets))
	for _, a := range plan.Assets {
		out = append(out, a.Destination.TargetID)
	}
	return out
}

func (l *candidateLedger) draftTransaction(ctx context.Context, candidateID, approvalID string) (*sql.Tx, string, error) {
	tx, owner, err := l.draftHistoryTransaction(ctx, candidateID, approvalID)
	if err != nil {
		return nil, "", err
	}
	rec, err := readCandidate(ctx, tx, owner, candidateID)
	if err != nil || rec.State != "approved" {
		_ = tx.Rollback()
		return nil, "", errors.New("release effects require an active approved candidate")
	}
	if err := requireCurrentCandidate(ctx, tx, candidateID); err != nil {
		_ = tx.Rollback()
		return nil, "", err
	}
	return tx, owner, nil
}

func (l *candidateLedger) draftHistoryTransaction(ctx context.Context, candidateID, approvalID string) (*sql.Tx, string, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return nil, "", err
	}
	db, err := l.database()
	if err != nil {
		return nil, "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	rec, err := readCandidate(ctx, tx, owner, candidateID)
	if err == nil {
		rec, err = readCandidateApproval(ctx, tx, owner, rec)
	}
	if err != nil || (rec.State != "approved" && rec.State != "archived") || approvalID == "" || rec.ApprovalID != approvalID {
		_ = tx.Rollback()
		return nil, "", errors.New("release effect requires its exact owner-approved candidate")
	}
	return tx, owner, nil
}

func readDraft(ctx context.Context, tx *sql.Tx, owner, candidateID, approvalID string, plan candidateDraftPlan) (candidateDraftRecord, error) {
	var rec candidateDraftRecord
	var planDigest string
	var stored, receipt []byte
	var releaseID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT intent_id,candidate_id,approval_id,plan_digest,plan,state,release_id,receipt FROM release_draft_intents
WHERE resource_key=$1 AND owner_user_id=$2 FOR UPDATE`, plan.ResourceKey, owner).Scan(&rec.IntentID, &rec.CandidateID, &rec.ApprovalID, &planDigest, &stored, &rec.State, &releaseID, &receipt)
	if err != nil {
		return rec, err
	}
	var decoded candidateDraftPlan
	if err := decodeCandidateObject(stored, &decoded); err != nil {
		return rec, err
	}
	actual, _, err := decoded.identity()
	if err != nil {
		return rec, err
	}
	want, _, err := plan.identity()
	if err != nil {
		return rec, err
	}
	if rec.IntentID == "" || rec.CandidateID != candidateID || rec.ApprovalID != approvalID || actual != planDigest || actual != want {
		return rec, errors.New("remote release is bound to a different candidate, approval or draft plan")
	}
	if rec.State != "prepared" && rec.State != "creating" && rec.State != "ready" && rec.State != "promoting" && rec.State != "published" {
		return rec, errors.New("invalid release lifecycle state")
	}
	if releaseID.Valid {
		rec.ReleaseID = releaseID.Int64
	}
	if (rec.State == "prepared" || rec.State == "creating") != (rec.ReleaseID == 0) || rec.ReleaseID < 0 {
		return rec, errors.New("release lifecycle identity is inconsistent")
	}
	if plan.Target.ReleaseID != 0 && rec.ReleaseID != plan.Target.ReleaseID {
		return rec, errors.New("stored release differs from configured release")
	}
	rec.Targets = draftRecordTargets(plan)
	if rec.State == "published" {
		var proof githubrelease.ReleaseState
		if err := decodeCandidateObject(receipt, &proof); err != nil {
			return rec, err
		}
		if err := validateDraftProof(plan, rec.ReleaseID, proof); err != nil {
			return rec, err
		}
		rec.Receipt = &proof
	}
	return rec, nil
}

func (l *candidateLedger) beginDraft(ctx context.Context, candidateID, approvalID string, plan candidateDraftPlan) (candidateDraftRecord, error) {
	tx, owner, err := l.draftTransaction(ctx, candidateID, approvalID)
	if err != nil {
		return candidateDraftRecord{}, err
	}
	defer tx.Rollback()
	digest, body, err := plan.identity()
	if err != nil {
		return candidateDraftRecord{}, err
	}
	state := "prepared"
	var releaseID any
	if plan.Target.ReleaseID != 0 {
		state = "ready"
		releaseID = plan.Target.ReleaseID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO release_draft_intents(resource_key,intent_id,owner_user_id,candidate_id,approval_id,plan_digest,plan,state,release_id)
VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9) ON CONFLICT(resource_key) DO NOTHING`, plan.ResourceKey, id.NewShortId(), owner, candidateID, approvalID, digest, string(body), state, releaseID)
	if err != nil {
		return candidateDraftRecord{}, err
	}
	rec, err := readDraft(ctx, tx, owner, candidateID, approvalID, plan)
	if err != nil {
		return rec, err
	}
	return rec, tx.Commit()
}

// Claim survives loss of this process and its transaction connection. A second
// host gets false and may only observe; absent readback never resets the fence.
func (l *candidateLedger) claimDraftEffect(ctx context.Context, candidateID, approvalID string, plan candidateDraftPlan, promotion bool) (candidateDraftRecord, bool, error) {
	tx, owner, err := l.draftTransaction(ctx, candidateID, approvalID)
	if err != nil {
		return candidateDraftRecord{}, false, err
	}
	defer tx.Rollback()
	rec, err := readDraft(ctx, tx, owner, candidateID, approvalID, plan)
	if err != nil {
		return rec, false, err
	}
	before, after := "prepared", "creating"
	if promotion {
		before, after = "ready", "promoting"
	}
	if rec.State != before {
		return rec, false, tx.Commit()
	}
	if promotion {
		if err := requireNoDraftUpload(ctx, tx, plan.ResourceKey); err != nil {
			return rec, false, err
		}
		// Every approved destination must have a complete native receipt before
		// exposing the release. A missing/pending write also fences in-flight
		// uploads after a process dies, before its upload lease has lapsed.
		candidate, err := readCandidate(ctx, tx, owner, candidateID)
		if err != nil {
			return rec, false, err
		}
		for _, d := range candidate.Manifest.Destinations {
			entry, err := candidatePublicationFor(candidate, d.TargetID, d.Component, d.Artifact)
			if err != nil {
				return rec, false, err
			}
			var state, approved string
			var body []byte
			err = tx.QueryRowContext(ctx, `SELECT state,approval_id,receipt FROM release_publication_intents WHERE candidate_id=$1 AND target_id=$2 AND component_name=$3 AND artifact_name=$4`, candidateID, d.TargetID, d.Component, d.Artifact).Scan(&state, &approved, &body)
			want := candidatePublicationReceipt{TargetDigest: d.TargetDigest, ArchiveDigest: entry.Artifact.Digest, ImageDigest: entry.Artifact.ImageDigest, Platform: entry.Artifact.Platform}
			var proof candidatePublicationReceipt
			if err != nil || state != "complete" || approved != approvalID || json.Unmarshal(body, &proof) != nil || proof != want {
				return rec, false, errors.New("release promotion requires every approved publication to be verified complete")
			}
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE release_draft_intents SET state=$2,updated_at=clock_timestamp() WHERE resource_key=$1`, plan.ResourceKey, after)
	if err != nil {
		return rec, false, err
	}
	rec.State = after
	return rec, true, tx.Commit()
}

func (l *candidateLedger) recordDraft(ctx context.Context, candidateID, approvalID string, plan candidateDraftPlan, state githubrelease.ReleaseState) (candidateDraftRecord, error) {
	if state.ReleaseID <= 0 || !state.Draft {
		return candidateDraftRecord{}, errors.New("draft binding requires native verified draft readback")
	}
	tx, owner, err := l.draftTransaction(ctx, candidateID, approvalID)
	if err != nil {
		return candidateDraftRecord{}, err
	}
	defer tx.Rollback()
	rec, err := readDraft(ctx, tx, owner, candidateID, approvalID, plan)
	if err != nil {
		return rec, err
	}
	if rec.State == "ready" && rec.ReleaseID == state.ReleaseID {
		return rec, tx.Commit()
	}
	if rec.State != "creating" || rec.ReleaseID != 0 {
		return rec, errors.New("draft cannot be rebound or recorded before its attempt")
	}
	_, err = tx.ExecContext(ctx, `UPDATE release_draft_intents SET state='ready',release_id=$2,updated_at=clock_timestamp() WHERE resource_key=$1`, plan.ResourceKey, state.ReleaseID)
	if err != nil {
		return rec, err
	}
	rec.State, rec.ReleaseID = "ready", state.ReleaseID
	return rec, tx.Commit()
}

func validateDraftProof(plan candidateDraftPlan, releaseID int64, proof githubrelease.ReleaseState) error {
	if plan.Target.Draft == nil || proof.LatestVerified != plan.Target.Draft.Latest {
		return errors.New("release readback differs from approved latest selection")
	}
	if proof.ReleaseID != releaseID || proof.Draft || len(proof.Assets) != len(plan.Assets) {
		return errors.New("release readback differs from approved publication")
	}
	want := plan.expected()
	got := make([]githubrelease.AssetExpectation, 0, len(proof.Assets))
	ids := map[int64]bool{}
	for _, a := range proof.Assets {
		if a.ID <= 0 || ids[a.ID] {
			return errors.New("invalid release readback asset identity")
		}
		ids[a.ID] = true
		got = append(got, a.AssetExpectation)
	}
	less := func(a, b githubrelease.AssetExpectation) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	}
	slices.SortFunc(want, less)
	slices.SortFunc(got, less)
	if !slices.Equal(want, got) {
		return errors.New("release readback inventory differs from approval")
	}
	return nil
}

func (l *candidateLedger) recordPromotion(ctx context.Context, candidateID, approvalID string, plan candidateDraftPlan, proof githubrelease.ReleaseState) (candidateDraftRecord, error) {
	tx, owner, err := l.draftTransaction(ctx, candidateID, approvalID)
	if err != nil {
		return candidateDraftRecord{}, err
	}
	defer tx.Rollback()
	rec, err := readDraft(ctx, tx, owner, candidateID, approvalID, plan)
	if err != nil {
		return rec, err
	}
	if err := validateDraftProof(plan, rec.ReleaseID, proof); err != nil {
		return rec, err
	}
	if rec.State == "published" {
		if !slices.Equal(rec.Receipt.Assets, proof.Assets) {
			return rec, errors.New("published release receipt changed")
		}
		return rec, tx.Commit()
	}
	if rec.State != "promoting" {
		return rec, errors.New("release was not claimed for promotion")
	}
	body, err := json.Marshal(proof)
	if err != nil {
		return rec, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE release_draft_intents SET state='published',receipt=$2::jsonb,updated_at=clock_timestamp() WHERE resource_key=$1`, plan.ResourceKey, string(body))
	if err != nil {
		return rec, err
	}
	rec.State, rec.Receipt = "published", &proof
	return rec, tx.Commit()
}

// errDraftUploadRunning refuses a promotion, renewal or second upload while the
// release's upload lease is live. A holder that died releases it when it lapses.
var errDraftUploadRunning = errors.New("an upload to this release is still running; try again once it has finished")

// withDraftUpload fences one file upload with a renewed durable lease. Promotion
// and renewal refuse while it is live, and promotion also refuses pending
// publication receipts, including lost replies. This fences our writers; the
// remote provider has no cross-resource CAS.
//
// It deliberately holds no transaction across write. The request pool ends a
// session idle in a transaction after 60 s, which failed an upload after it had
// succeeded and silently dropped the lock, and every candidate read waited on the
// row lock until its 8 s limit, which blinded the Releases screen for the whole
// upload. What survives a retry or replica change is the lease row; write's context
// ends if that lease is lost.
func (l *candidateLedger) withDraftUpload(ctx context.Context, candidateID, approvalID string, plan candidateDraftPlan, write func(context.Context, int64) error) error {
	lease, err := l.claimDraftUpload(ctx, candidateID, approvalID, plan)
	if err != nil {
		return err
	}
	defer lease.close(ctx)
	work, stop := keepLease(ctx, uploadLeaseTiming, lease.renew)
	defer stop()
	return write(work, lease.releaseID)
}

type draftUploadLease struct {
	ledger                    *candidateLedger
	resourceKey, owner, token string
	releaseID                 int64
}

func (l *candidateLedger) claimDraftUpload(ctx context.Context, candidateID, approvalID string, plan candidateDraftPlan) (*draftUploadLease, error) {
	tx, owner, err := l.draftTransaction(ctx, candidateID, approvalID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rec, err := readDraft(ctx, tx, owner, candidateID, approvalID, plan)
	if err != nil {
		return nil, err
	}
	if rec.State != "ready" || rec.ReleaseID <= 0 {
		return nil, errors.New("release upload requires a bound draft that is not being promoted")
	}
	token := id.NewShortId()
	result, err := tx.ExecContext(ctx, `UPDATE release_draft_intents SET upload_lease_id=$2,upload_lease_until=clock_timestamp()+($3::bigint*interval '1 millisecond')
WHERE resource_key=$1 AND upload_lease_until<=clock_timestamp()`, plan.ResourceKey, token, uploadLeaseTiming.ttl.Milliseconds())
	if err != nil {
		return nil, err
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if claimed != 1 {
		return nil, errDraftUploadRunning
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &draftUploadLease{l, plan.ResourceKey, owner, token, rec.ReleaseID}, nil
}

func (s *draftUploadLease) renew(ctx context.Context) error {
	db, err := s.ledger.database()
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE release_draft_intents SET upload_lease_until=clock_timestamp()+($4::bigint*interval '1 millisecond')
WHERE resource_key=$1 AND owner_user_id=$2 AND upload_lease_id=$3 AND upload_lease_until>clock_timestamp()`, s.resourceKey, s.owner, s.token, uploadLeaseTiming.ttl.Milliseconds())
	if err != nil {
		return err
	}
	renewed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if renewed != 1 {
		return errLeaseLost
	}
	return nil
}

// close is best effort: a lease it fails to release lapses on its own.
func (s *draftUploadLease) close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if db, err := s.ledger.database(); err == nil {
		_, _ = db.ExecContext(ctx, `UPDATE release_draft_intents SET upload_lease_id='',upload_lease_until='epoch' WHERE resource_key=$1 AND owner_user_id=$2 AND upload_lease_id=$3`, s.resourceKey, s.owner, s.token)
	}
}

// requireNoDraftUpload runs under the caller's lock on the draft row, which is
// the lock an upload claim takes, so the answer stays true until that commit.
func requireNoDraftUpload(ctx context.Context, tx *sql.Tx, resourceKey string) error {
	var running bool
	if err := tx.QueryRowContext(ctx, `SELECT upload_lease_until>clock_timestamp() FROM release_draft_intents WHERE resource_key=$1`, resourceKey).Scan(&running); err != nil {
		return err
	}
	if running {
		return errDraftUploadRunning
	}
	return nil
}
