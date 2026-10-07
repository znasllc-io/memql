package release

import (
	"context"
	"encoding/json"
	"errors"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// This is an internal effect scope, never decoded from a DSL or client request.
// The registry adapter consumes its exact approved target and artifact. A
// pending intent survives lost replies and controller replacement; repeating
// begin returns the same effect identity rather than starting a new effect.
type candidatePublication struct {
	EffectID, CandidateID, ApprovalID, State string
	Target                                   pl.ReleaseDestination
	Artifact                                 pl.ReleaseArtifact
}

type candidatePublicationReceipt struct {
	TargetDigest  string `json:"targetDigest"`
	ArchiveDigest string `json:"archiveDigest"`
	ImageDigest   string `json:"imageDigest"`
	Platform      string `json:"platform"`
}

func (l *candidateLedger) beginPublication(ctx context.Context, key, approvalID, target, component, artifact string) (candidatePublication, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidatePublication{}, err
	}
	db, err := l.database()
	if err != nil {
		return candidatePublication{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return candidatePublication{}, err
	}
	defer tx.Rollback()
	rec, err := readCandidate(ctx, tx, owner, key)
	if err != nil {
		return candidatePublication{}, err
	}
	if rec.State != "approved" {
		return candidatePublication{}, errors.New("publication requires an approved candidate")
	}
	var storedApproval, approvedBy string
	err = tx.QueryRowContext(ctx, `SELECT approval_id,approved_by FROM release_candidate_approvals WHERE candidate_id=$1`, key).Scan(&storedApproval, &approvedBy)
	if err != nil {
		return candidatePublication{}, err
	}
	if approvalID == "" || approvalID != storedApproval || approvedBy != owner {
		return candidatePublication{}, errors.New("publication approval does not match this candidate")
	}
	intent, err := candidatePublicationFor(rec, target, component, artifact)
	if err != nil {
		return candidatePublication{}, err
	}
	intent.ApprovalID = approvalID
	_, err = tx.ExecContext(ctx, `INSERT INTO release_publication_intents(effect_id,candidate_id,target_id,component_name,artifact_name,approval_id,state)
VALUES($1,$2,$3,$4,$5,$6,'pending') ON CONFLICT(candidate_id,target_id,component_name,artifact_name) DO NOTHING`, id.NewShortId(), key, target, component, artifact, approvalID)
	if err != nil {
		return candidatePublication{}, err
	}
	var storedReceipt []byte
	err = tx.QueryRowContext(ctx, `SELECT effect_id,state,receipt FROM release_publication_intents WHERE candidate_id=$1 AND target_id=$2 AND component_name=$3 AND artifact_name=$4 AND approval_id=$5`, key, target, component, artifact, approvalID).Scan(&intent.EffectID, &intent.State, &storedReceipt)
	if err != nil {
		return candidatePublication{}, err
	}
	if intent.State == "complete" {
		var proof candidatePublicationReceipt
		want := candidatePublicationReceipt{TargetDigest: intent.Target.TargetDigest, ArchiveDigest: intent.Artifact.Digest, ImageDigest: intent.Artifact.ImageDigest, Platform: intent.Artifact.Platform}
		if json.Unmarshal(storedReceipt, &proof) != nil || proof != want {
			return candidatePublication{}, errors.New("stored publication proof disagrees with candidate")
		}
	}
	if err := tx.Commit(); err != nil {
		return candidatePublication{}, err
	}
	return intent, nil
}

func candidatePublicationFor(rec candidateRecord, target, component, artifact string) (candidatePublication, error) {
	intent := candidatePublication{CandidateID: rec.ID}
	for _, d := range rec.Manifest.Destinations {
		if d.TargetID == target && d.Component == component && d.Artifact == artifact && d.Operation == "publish" {
			intent.Target = d
		}
	}
	for _, c := range rec.Manifest.Components {
		if c.Name == component {
			for _, a := range c.Artifacts {
				if a.Name == artifact {
					intent.Artifact = a
				}
			}
		}
	}
	if intent.Target.TargetID == "" || intent.Artifact.Name == "" {
		return candidatePublication{}, errors.New("publication target or artifact is outside approved candidate")
	}
	return intent, nil
}

// Finish is called only after the native publication adapter independently
// reads back the digest. Neither a successful request nor a client-supplied
// receipt is proof of publication. The stored result cannot later be replaced.
func (l *candidateLedger) finishPublication(ctx context.Context, intent candidatePublication, proof candidatePublicationReceipt) error {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return err
	}
	db, err := l.database()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rec, err := readCandidate(ctx, tx, owner, intent.CandidateID)
	if err != nil {
		return err
	}
	if rec.State != "approved" {
		return errors.New("publication candidate is no longer approved")
	}
	expected, err := candidatePublicationFor(rec, intent.Target.TargetID, intent.Target.Component, intent.Target.Artifact)
	if err != nil {
		return err
	}
	want := candidatePublicationReceipt{TargetDigest: expected.Target.TargetDigest, ArchiveDigest: expected.Artifact.Digest, ImageDigest: expected.Artifact.ImageDigest, Platform: expected.Artifact.Platform}
	if proof != want {
		return errors.New("publication receipt does not match the approved artifact and destination")
	}
	var state string
	var old []byte
	err = tx.QueryRowContext(ctx, `SELECT state,receipt FROM release_publication_intents WHERE effect_id=$1 AND candidate_id=$2 AND target_id=$3 AND component_name=$4 AND artifact_name=$5 AND approval_id=$6 FOR UPDATE`, intent.EffectID, intent.CandidateID, expected.Target.TargetID, expected.Target.Component, expected.Target.Artifact, intent.ApprovalID).Scan(&state, &old)
	if err != nil {
		return err
	}
	if state == "complete" {
		var prior candidatePublicationReceipt
		if json.Unmarshal(old, &prior) != nil || prior != proof {
			return errors.New("stored publication proof disagrees")
		}
		return tx.Commit()
	}
	if state != "pending" {
		return errors.New("publication intent has an invalid state")
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE release_publication_intents SET state='complete',receipt=$2::jsonb,updated_at=clock_timestamp() WHERE effect_id=$1`, intent.EffectID, string(encoded))
	if err != nil {
		return err
	}
	return tx.Commit()
}
