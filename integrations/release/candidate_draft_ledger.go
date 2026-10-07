package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

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
	if err != nil || rec.State != "approved" || approvalID == "" || rec.ApprovalID != approvalID {
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
		// Every approved destination must have a complete native receipt before
		// exposing the release. A missing/pending write also fences in-flight
		// uploads after a process dies, when a transaction lock is no longer held.
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

// Hold the native resource lock through one file upload. Promotion takes the
// same lock and refuses pending publication receipts, including lost replies.
// This fences our writers; the remote provider has no cross-resource CAS.
func (l *candidateLedger) withDraftUpload(ctx context.Context, candidateID, approvalID string, plan candidateDraftPlan, write func(int64) error) error {
	tx, owner, err := l.draftTransaction(ctx, candidateID, approvalID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rec, err := readDraft(ctx, tx, owner, candidateID, approvalID, plan)
	if err != nil {
		return err
	}
	if rec.State != "ready" || rec.ReleaseID <= 0 {
		return errors.New("release upload requires a bound draft that is not being promoted")
	}
	if err := write(rec.ReleaseID); err != nil {
		return err
	}
	return tx.Commit()
}
