package release

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// effects reads durable publication state without starting or retrying an
// effect. A missing entry means no recorded intent, not confirmed remote absence.
// An entry marked complete is historical verified readback, not a fresh probe.
func (l *candidateLedger) effects(ctx context.Context, key string) ([]candidatePublication, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(key, "sha256:") || !candidateArtifactDigest(strings.TrimPrefix(key, "sha256:")) {
		return nil, errors.New("exact candidate digest is required")
	}
	db, err := l.database()
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Keep the candidate lock through the effect read, so a concurrent approval
	// cannot turn a legitimate new intent into an apparent journal mismatch.
	record, err := readCandidate(ctx, tx, owner, key)
	if err != nil {
		return nil, err
	}
	record, err = readCandidateApproval(ctx, tx, owner, record)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT effect_id,target_id,component_name,artifact_name,approval_id,state,receipt
FROM release_publication_intents WHERE candidate_id=$1 ORDER BY target_id,component_name,artifact_name LIMIT 1025`, record.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []candidatePublication{}
	for rows.Next() {
		var effect, target, component, artifact, approval, state string
		var body []byte
		if err := rows.Scan(&effect, &target, &component, &artifact, &approval, &state, &body); err != nil {
			return nil, err
		}
		if len(out) == 1024 || effect == "" || record.State != "approved" || approval == "" || approval != record.ApprovalID || (state != "pending" && state != "complete") {
			return nil, errors.New("candidate publication journal is inconsistent")
		}
		entry, err := candidatePublicationFor(record, target, component, artifact)
		if err != nil {
			return nil, err
		}
		if state == "complete" {
			var proof candidatePublicationReceipt
			want := candidatePublicationReceipt{TargetDigest: entry.Target.TargetDigest, ArchiveDigest: entry.Artifact.Digest, ImageDigest: entry.Artifact.ImageDigest, Platform: entry.Artifact.Platform}
			if json.Unmarshal(body, &proof) != nil || proof != want {
				return nil, errors.New("stored publication proof disagrees with candidate")
			}
		}
		entry.EffectID, entry.ApprovalID, entry.State = effect, approval, state
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (i *Integration) handleCandidateEffects(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	key := asString(args["candidateId"])
	effects, err := p.ledger.effects(ctx, key)
	if err != nil {
		return nil, err
	}
	return resultNode("publications", key, map[string]any{"candidateId": key, "publications": effects})
}
