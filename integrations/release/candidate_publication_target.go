package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// The full approved configuration stays private. Historical catalog projection
// reads this frozen evidence, never today's operator configuration. Secret
// references are included in the approval hash; secret material is never stored.
type candidatePublicationTarget struct {
	Registry *candidateRegistryTarget `json:"registry,omitempty"`
	File     *candidateFileTarget     `json:"file,omitempty"`
}

func (t candidatePublicationTarget) validate(d pl.ReleaseDestination) error {
	var key, component, artifact, digest string
	var err error
	if t.Registry != nil && t.File == nil {
		key, component, artifact = t.Registry.ID, t.Registry.Component, t.Registry.Artifact
		_, _, digest, err = t.Registry.snapshot()
	} else if t.File != nil && t.Registry == nil {
		key, component, artifact = t.File.ID, t.File.Component, t.File.Artifact
		_, _, digest, err = t.File.snapshot()
	} else {
		return errors.New("publication must freeze exactly one target")
	}
	if err != nil {
		return err
	}
	if d.Operation != "publish" || key != d.TargetID || component != d.Component || artifact != d.Artifact || digest != d.TargetDigest {
		return errors.New("frozen publication target differs from approval")
	}
	return nil
}

func (r candidateTargetReader) publicationTarget(ctx context.Context, d pl.ReleaseDestination) (candidatePublicationTarget, error) {
	if _, ok := r.files[d.TargetID]; ok {
		t, _, err := r.resolveFile(ctx, d)
		return candidatePublicationTarget{File: &t}, err
	}
	t, _, err := r.resolve(ctx, d)
	return candidatePublicationTarget{Registry: &t}, err
}

func (l *candidateLedger) freezePublicationTarget(ctx context.Context, intent candidatePublication, target candidatePublicationTarget) error {
	if err := target.validate(intent.Target); err != nil {
		return err
	}
	tx, owner, err := l.draftTransaction(ctx, intent.CandidateID, intent.ApprovalID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rec, err := readCandidate(ctx, tx, owner, intent.CandidateID)
	if err != nil {
		return err
	}
	expected, err := candidatePublicationFor(rec, intent.Target.TargetID, intent.Target.Component, intent.Target.Artifact)
	if err != nil {
		return err
	}
	if err := target.validate(expected.Target); err != nil {
		return err
	}
	var body []byte
	err = tx.QueryRowContext(ctx, `SELECT target_snapshot FROM release_publication_intents WHERE effect_id=$1 AND candidate_id=$2 AND approval_id=$3 AND target_id=$4 AND component_name=$5 AND artifact_name=$6 FOR UPDATE`, intent.EffectID, rec.ID, intent.ApprovalID, expected.Target.TargetID, expected.Target.Component, expected.Target.Artifact).Scan(&body)
	if err != nil {
		return err
	}
	if len(body) != 0 {
		var stored candidatePublicationTarget
		if err := decodeCandidateObject(body, &stored); err != nil {
			return err
		}
		if err := stored.validate(expected.Target); err != nil {
			return err
		}
		return tx.Commit()
	}
	body, err = json.Marshal(target)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE release_publication_intents SET target_snapshot=$2::jsonb WHERE effect_id=$1`, intent.EffectID, string(body))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func readPublicationTarget(ctx context.Context, tx *sql.Tx, candidateID, approvalID string, d pl.ReleaseDestination) (candidatePublicationTarget, candidatePublicationReceipt, error) {
	var target candidatePublicationTarget
	var proof candidatePublicationReceipt
	var state, approval string
	var snapshot, receipt []byte
	err := tx.QueryRowContext(ctx, `SELECT state,approval_id,target_snapshot,receipt FROM release_publication_intents WHERE candidate_id=$1 AND target_id=$2 AND component_name=$3 AND artifact_name=$4`, candidateID, d.TargetID, d.Component, d.Artifact).Scan(&state, &approval, &snapshot, &receipt)
	if err != nil {
		return target, proof, err
	}
	if state != "complete" || approval != approvalID {
		return target, proof, errors.New("catalog requires every exact approved publication complete")
	}
	if err := decodeCandidateObject(snapshot, &target); err != nil {
		return target, proof, err
	}
	if err := target.validate(d); err != nil {
		return target, proof, err
	}
	if err := decodeCandidateObject(receipt, &proof); err != nil {
		return target, proof, err
	}
	return target, proof, nil
}
