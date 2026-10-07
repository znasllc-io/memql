package release

import (
	"context"
	"errors"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (i *Integration) handleCandidateDraftCreate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return i.handleCandidateDraft(ctx, args, false)
}
func (i *Integration) handleCandidateDraftPromote(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return i.handleCandidateDraft(ctx, args, true)
}
func (i *Integration) handleCandidateDraft(ctx context.Context, args map[string]any, promote bool) ([]memorynodes.MemoryNode, error) {
	p, err := i.configuredCandidate(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := p.draft(ctx, candidateDraftRequest{CandidateID: asString(args["candidateId"]), ApprovalID: asString(args["approvalId"]), TargetID: asString(args["targetId"])}, promote)
	if err != nil {
		return nil, err
	}
	return resultNode("draft", rec.IntentID, rec)
}

// History does not depend on current configuration, credentials or Library
// availability. Stored proofs are historical observations, not fresh probes.
func (l *candidateLedger) drafts(ctx context.Context, key string) ([]candidateDraftRecord, error) {
	rec, err := l.get(ctx, key)
	if err != nil {
		return nil, err
	}
	if rec.State != "approved" {
		return []candidateDraftRecord{}, nil
	}
	tx, owner, err := l.draftTransaction(ctx, key, rec.ApprovalID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT plan FROM release_draft_intents WHERE candidate_id=$1 AND owner_user_id=$2 ORDER BY resource_key LIMIT 1025`, key, owner)
	if err != nil {
		return nil, err
	}
	plans := []candidateDraftPlan{}
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return nil, err
		}
		var plan candidateDraftPlan
		if err := decodeCandidateObject(body, &plan); err != nil {
			rows.Close()
			return nil, err
		}
		plans = append(plans, plan)
		if len(plans) > 1024 {
			rows.Close()
			return nil, errors.New("draft history exceeds its bound")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]candidateDraftRecord, 0, len(plans))
	for _, plan := range plans {
		record, err := readDraft(ctx, tx, owner, key, rec.ApprovalID, plan)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, tx.Commit()
}

func (i *Integration) handleCandidateDrafts(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	key := asString(args["candidateId"])
	drafts, err := p.ledger.drafts(ctx, key)
	if err != nil {
		return nil, err
	}
	return resultNode("drafts", key, map[string]any{"candidateId": key, "drafts": drafts})
}
