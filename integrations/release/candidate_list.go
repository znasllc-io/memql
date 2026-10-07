package release

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type candidateCursor struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

type candidateComponentSummary struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}

type candidateSummary struct {
	ID            string                      `json:"candidateId"`
	State         string                      `json:"state"`
	CreatedAt     time.Time                   `json:"createdAt"`
	Components    []candidateComponentSummary `json:"components"`
	EvidenceCount int                         `json:"evidenceCount"`
}

type candidatePage struct {
	Candidates []candidateSummary `json:"candidates"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// List is an owner-scoped journal read, including interrupted preparations.
// Cursor order uses immutable creation time and identity, not a mutable status.
// The page omits large manifests; get revalidates one exact manifest for review.
func (l *candidateLedger) list(ctx context.Context, cursor string, limit int) (candidatePage, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidatePage{}, err
	}
	if limit < 1 || limit > 50 || len(cursor) > 1024 {
		return candidatePage{}, errors.New("candidate list requires a limit from 1 to 50 and a bounded cursor")
	}
	var before any
	var position candidateCursor
	if cursor != "" {
		body, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || decodeCandidateObject(body, &position) != nil || position.CreatedAt.IsZero() || len(position.ID) != 71 || position.ID[:7] != "sha256:" || !candidateArtifactDigest(position.ID[7:]) {
			return candidatePage{}, errors.New("invalid candidate cursor")
		}
		before = position.CreatedAt
	}
	db, err := l.database()
	if err != nil {
		return candidatePage{}, err
	}
	rows, err := db.QueryContext(ctx, `SELECT candidate_id,created_at FROM release_candidates
WHERE owner_user_id=$1 AND ($2::timestamptz IS NULL OR (created_at,candidate_id)<($2::timestamptz,$3))
ORDER BY created_at DESC,candidate_id DESC LIMIT $4`, owner, before, position.ID, limit+1)
	if err != nil {
		return candidatePage{}, err
	}
	positions := []candidateCursor{}
	for rows.Next() {
		var item candidateCursor
		if err := rows.Scan(&item.ID, &item.CreatedAt); err != nil {
			rows.Close()
			return candidatePage{}, err
		}
		positions = append(positions, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return candidatePage{}, err
	}
	page := candidatePage{Candidates: []candidateSummary{}}
	if len(positions) > limit {
		positions = positions[:limit]
		body, err := json.Marshal(positions[len(positions)-1])
		if err != nil {
			return candidatePage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(body)
	}
	for _, position := range positions {
		record, err := l.get(ctx, position.ID)
		if err != nil {
			return candidatePage{}, err
		}
		item := candidateSummary{ID: record.ID, State: record.State, CreatedAt: position.CreatedAt, EvidenceCount: len(record.Manifest.Evidence), Components: []candidateComponentSummary{}}
		for _, component := range record.Manifest.Components {
			item.Components = append(item.Components, candidateComponentSummary{Name: component.Name, Version: component.Version, Repository: component.Repository, Commit: component.Commit})
		}
		page.Candidates = append(page.Candidates, item)
	}
	return page, nil
}

func (i *Integration) handleCandidateList(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	limit := 20
	if value, ok := args["limit"]; ok && value != nil {
		encoded, err := json.Marshal(value)
		if err != nil || json.Unmarshal(encoded, &limit) != nil {
			return nil, errors.New("candidate list limit must be an integer")
		}
	}
	cursor := ""
	if value, ok := args["cursor"]; ok && value != nil {
		var valid bool
		cursor, valid = value.(string)
		if !valid {
			return nil, errors.New("candidate list cursor must be a string")
		}
	}
	page, err := p.ledger.list(ctx, cursor, limit)
	if err != nil {
		return nil, err
	}
	return resultNode("candidates", "", page)
}
