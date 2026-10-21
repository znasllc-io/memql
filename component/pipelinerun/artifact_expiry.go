package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const artifactExpiryReadLimit = 128

type ExpiredArtifact struct {
	FileID string `json:"fileId"`
	Name   string `json:"name"`
}

func (i *Integration) handleExpiredArtifacts(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	files, _ := stringListArg(args, "fileIds")
	expired, err := i.ExpiredArtifacts(handlerContext(ctx), stringArg(args, "runId"), files)
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"files": expired}), nil
}

// ExpiredArtifacts reports confirmed retirement, never inferring it from a
// missing Library row or an artifact's age. The durable journal supplies this
// answer on any replica, including after the producing work has been swept.
func (i *Integration) ExpiredArtifacts(ctx context.Context, runID string, fileIDs []string) ([]ExpiredArtifact, error) {
	owner, err := personFrom(ctx)
	if err != nil {
		return nil, err
	}
	owner = memql.BareShortId(owner)
	if runID == "" || len(runID) > 512 || len(fileIDs) == 0 || len(fileIDs) > artifactExpiryReadLimit {
		return nil, errors.New("artifact expiry requires a run and 1 to 128 file identities")
	}
	for _, file := range fileIDs {
		if strings.TrimSpace(file) != file || file == "" || len(file) > 256 || memql.BareShortId(file) != file {
			return nil, errors.New("artifact expiry requires bare file identities")
		}
	}
	d := i.snapshot()
	if d.Store == nil || d.ArtifactRetention == nil || d.ArtifactRetention.Database == nil {
		return nil, errors.New("artifact expiry journal is unavailable")
	}
	ctx, cancel := context.WithTimeout(memql.ContextWithFreshRead(ctx), 15*time.Second)
	defer cancel()
	r, err := d.Store.RunForOwner(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r == nil || memql.BareShortId(r.OwnerUserID) != owner || r.WorkRunID == "" {
		return nil, errors.New("artifact expiry requires the caller's pipeline run")
	}
	db := d.ArtifactRetention.Database()
	if db == nil {
		return nil, errors.New("artifact expiry journal is unavailable")
	}
	selection, err := json.Marshal(fileIDs)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT file_id,coalesce(identity->>'Name','')
FROM pipeline_artifact_uploads
WHERE owner_user_id=$1 AND identity->>'OwnerUserID'=$1 AND identity->>'WorkRunID'=$2
 AND file_id IN (SELECT jsonb_array_elements_text($3::jsonb))
 AND state='retired' AND retired_etag<>'' AND retirement_lease_id<>'' AND library_purged_at IS NOT NULL
ORDER BY file_id LIMIT $4`, owner, r.WorkRunID, string(selection), artifactExpiryReadLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ExpiredArtifact, 0)
	for rows.Next() {
		var item ExpiredArtifact
		if err := rows.Scan(&item.FileID, &item.Name); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
