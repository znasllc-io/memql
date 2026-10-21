package release

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

func requireCurrentCandidate(ctx context.Context, tx *sql.Tx, key string) error {
	var replaced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM release_candidate_successions WHERE previous_candidate_id=$1)`, key).Scan(&replaced); err != nil {
		return err
	}
	if replaced {
		return errors.New("release candidate was superseded by a fresh owner approval")
	}
	return nil
}

// Only the explicit, freshly verified draft workflow may renew authority.
// Upload and promotion paths keep beginDraft's strict identity match. A policy
// upgrade never carries approval forward: both immutable candidates must have
// separate approvals, and everything except their workflow digest must match.
func (l *candidateLedger) beginReviewedDraft(ctx context.Context, key, approval string, plan candidateDraftPlan) (candidateDraftRecord, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateDraftRecord{}, err
	}
	db, err := l.database()
	if err != nil {
		return candidateDraftRecord{}, err
	}
	var previous string
	err = db.QueryRowContext(ctx, `SELECT candidate_id FROM release_draft_intents WHERE resource_key=$1 AND owner_user_id=$2`, plan.ResourceKey, owner).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && previous == key) {
		return l.beginDraft(ctx, key, approval, plan)
	}
	if err != nil {
		return candidateDraftRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return candidateDraftRecord{}, err
	}
	defer tx.Rollback()
	// All effects lock their candidate before its draft. Lock both candidates in
	// stable order, including competing renewals on different replicas.
	keys := []string{key, previous}
	slices.Sort(keys)
	records := make(map[string]candidateRecord, 2)
	for _, candidateID := range keys {
		rec, err := readCandidate(ctx, tx, owner, candidateID)
		if err == nil {
			rec, err = readCandidateApproval(ctx, tx, owner, rec)
		}
		if err != nil || rec.State != "approved" || rec.ApprovalID == "" {
			return candidateDraftRecord{}, errors.New("draft renewal requires two owner-approved candidates")
		}
		records[candidateID] = rec
	}
	next, old := records[key], records[previous]
	if next.ApprovalID != approval {
		return candidateDraftRecord{}, errors.New("draft renewal requires the new candidate's exact approval")
	}
	if err := requireCurrentCandidate(ctx, tx, key); err != nil {
		return candidateDraftRecord{}, err
	}
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT candidate_id FROM release_draft_intents WHERE resource_key=$1 AND owner_user_id=$2 FOR UPDATE`, plan.ResourceKey, owner).Scan(&current); err != nil {
		return candidateDraftRecord{}, err
	}
	if current == key { // A peer completed this exact renewal while we waited.
		rec, err := readDraft(ctx, tx, owner, key, approval, plan)
		if err != nil {
			return rec, err
		}
		return rec, tx.Commit()
	}
	if current != previous {
		return candidateDraftRecord{}, errors.New("draft authority changed while preparing renewal")
	}
	if err := requireCurrentCandidate(ctx, tx, previous); err != nil {
		return candidateDraftRecord{}, err
	}
	if err := unchangedFileRelease(old, next); err != nil {
		return candidateDraftRecord{}, err
	}
	var effects bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM release_publication_intents WHERE candidate_id=$1) OR EXISTS(SELECT 1 FROM release_draft_intents WHERE candidate_id=$1) OR EXISTS(SELECT 1 FROM release_candidate_successions WHERE candidate_id=$1)`, key).Scan(&effects); err != nil {
		return candidateDraftRecord{}, err
	}
	if effects {
		return candidateDraftRecord{}, errors.New("renewal candidate already has publication authority")
	}
	// Compare the newly verified plan before moving any binding or receipt.
	binding, err := readDraft(ctx, tx, owner, previous, old.ApprovalID, plan)
	if err != nil {
		return binding, err
	}
	if err := requireReadyDrafts(ctx, tx, owner, old); err != nil {
		return binding, err
	}
	if err := inheritFilePublications(ctx, tx, old, next); err != nil {
		return binding, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO release_candidate_successions(previous_candidate_id,candidate_id,previous_approval_id,approval_id) VALUES($1,$2,$3,$4)`, previous, key, old.ApprovalID, approval)
	if err != nil {
		return binding, err
	}
	// Preserve remote IDs, marker/intent IDs and plans. The succession journal
	// links the old proofs to the fresh approval without rewriting their history.
	_, err = tx.ExecContext(ctx, `UPDATE release_draft_intents SET candidate_id=$2,approval_id=$3,updated_at=clock_timestamp() WHERE candidate_id=$1 AND owner_user_id=$4`, previous, key, approval, owner)
	if err != nil {
		return binding, err
	}
	binding.CandidateID, binding.ApprovalID = key, approval
	return binding, tx.Commit()
}

func unchangedFileRelease(old, next candidateRecord) error {
	manifest := old.Manifest
	manifest.WorkflowDigest = next.Manifest.WorkflowDigest
	_, equivalent, err := pl.CanonicalReleaseCandidate(manifest)
	if err != nil || equivalent != next.ID {
		return errors.New("draft renewal cannot change artifacts, evidence, sources, versions or destinations")
	}
	for _, d := range next.Manifest.Destinations {
		entry, err := candidatePublicationFor(next, d.TargetID, d.Component, d.Artifact)
		if err != nil || entry.Artifact.Kind != "file" {
			return errors.New("draft renewal requires an unchanged file-only release")
		}
	}
	return nil
}

func requireReadyDrafts(ctx context.Context, tx *sql.Tx, owner string, old candidateRecord) error {
	rows, err := tx.QueryContext(ctx, `SELECT plan FROM release_draft_intents WHERE candidate_id=$1 AND owner_user_id=$2 ORDER BY resource_key LIMIT 1025 FOR UPDATE`, old.ID, owner)
	if err != nil {
		return err
	}
	plans := []candidateDraftPlan{}
	for rows.Next() {
		var body []byte
		var plan candidateDraftPlan
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return err
		}
		if err := decodeCandidateObject(body, &plan); err != nil {
			rows.Close()
			return err
		}
		plans = append(plans, plan)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(plans) == 0 || len(plans) > 1024 {
		return errors.New("draft renewal exceeds its bound")
	}
	for _, plan := range plans {
		rec, err := readDraft(ctx, tx, owner, old.ID, old.ApprovalID, plan)
		if err != nil {
			return err
		}
		if rec.State != "ready" {
			return errors.New("draft renewal requires all previous drafts ready, with no uncertain or published effects")
		}
	}
	return nil
}

func inheritFilePublications(ctx context.Context, tx *sql.Tx, old, next candidateRecord) error {
	var count, pending int
	err := tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE state<>'complete') FROM release_publication_intents WHERE candidate_id=$1`, old.ID).Scan(&count, &pending)
	if err != nil {
		return err
	}
	if pending != 0 {
		return errors.New("draft renewal refuses unfinished publication attempts")
	}
	verified := 0
	for _, d := range old.Manifest.Destinations {
		target, proof, err := readPublicationTarget(ctx, tx, old.ID, old.ApprovalID, d)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		entry, err := candidatePublicationFor(next, d.TargetID, d.Component, d.Artifact)
		if err != nil {
			return err
		}
		want := candidatePublicationReceipt{TargetDigest: d.TargetDigest, ArchiveDigest: entry.Artifact.Digest, ImageDigest: entry.Artifact.ImageDigest, Platform: entry.Artifact.Platform}
		if target.File == nil || proof != want {
			return errors.New("previous publication proof differs from the approved file release")
		}
		// Historical verified receipts may be reused, just as on an ordinary
		// idempotent resume. Promotion still reads back every remote asset anew.
		_, err = tx.ExecContext(ctx, `INSERT INTO release_publication_intents(effect_id,candidate_id,target_id,component_name,artifact_name,approval_id,state,receipt,target_snapshot)
SELECT $1,$2,target_id,component_name,artifact_name,$3,state,receipt,target_snapshot FROM release_publication_intents WHERE candidate_id=$4 AND target_id=$5 AND component_name=$6 AND artifact_name=$7`, id.NewShortId(), next.ID, next.ApprovalID, old.ID, d.TargetID, d.Component, d.Artifact)
		if err != nil {
			return err
		}
		verified++
	}
	if verified != count {
		return errors.New("previous publication ledger contains effects outside the approved release")
	}
	return nil
}
