package library

// Optional retrieval can repeatedly time out before the embedding cache has a
// value to reuse. Reuse that *availability observation*, never an empty answer.
// The existing owned journal is durable across replicas. Source heads and the
// binding are read anew; no stored observation grants access to a passage.
import (
	"context"
	"encoding/json"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// A bounded fingerprint is conservative: an oversized corpus disables reuse,
// not retrieval. Gate original rows before deriving any fingerprint from them.
func (i *Integration) trainingAvailabilityKey(ctx context.Context, run common.RunContext, query, provider string) (string, error) {
	rows, err := i.training.db().QueryContext(ctx, "SELECT * FROM ("+trainingSourceSQL+") s ORDER BY id LIMIT 1025", memql.BareShortId(run.OwnerUserId))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var heads []string
	for rows.Next() {
		var c trainingCandidate
		if err := rows.Scan(&c.node.ID, &c.node.Payload, &c.node.CreatedAt, &c.artifactID, &c.fileID, &c.artifact.Payload, &c.file.Payload); err != nil {
			return "", err
		}
		c.node.Concept = "v1:knowledge:documentChunk"
		var payload map[string]any
		if err := json.Unmarshal(c.node.Payload, &payload); err != nil {
			return "", err
		}
		if data, ok := payload["data"].(map[string]any); ok && data["trainingLesson"] != nil {
			c.node.Concept = "v1:work:observation"
		}
		c.artifact.ID, c.artifact.Concept = c.artifactID, "v1:library:artifact"
		c.file.ID, c.file.Concept = c.fileID, "v1:library:file"
		for _, node := range []memorynodes.MemoryNode{c.node, c.artifact, c.file} {
			if node.ID != "" && !i.training.admit(ctx, node) {
				return "", nil
			}
		}
		heads = append(heads, trainingDigest(c.node.ID+string(c.node.Payload)+c.node.CreatedAt.Format(time.RFC3339Nano)+string(c.artifact.Payload)+string(c.file.Payload)))
		if len(heads) > 1024 {
			return "", nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	binding, err := memql.CurrentEmbedderBinding(ctx)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal([]any{query, provider, binding, heads})
	return trainingDigest(string(raw)), err
}

// Timeout observations expire at the original retry time. Reading them does
// not slide that time forward. The complete latest row is authorized before
// its status is used; a model-authored note cannot satisfy this native schema.
// staged-data: GATE -- readOwnedTrainingLessons withholds staged Work observations
// before this read; the complete returned row is admitted before status reuse.
func (i *Integration) recentTrainingTimeout(ctx context.Context, run common.RunContext, key string, now time.Time) (trainingSelectionReceipt, string, error) {
	var prior trainingSelectionReceipt
	if key == "" {
		return prior, "", nil
	}
	rows, err := i.training.db().QueryContext(ctx, `SELECT id,payload,"createdAt" FROM (
 SELECT DISTINCT ON(id) id,payload,"createdAt" FROM "MemoryNodes"
 WHERE concept='v1:work:observation' AND payload->>'ownerUserId' IN ($1,'v1:identity:user:'||$1)
 AND payload->>'runId' IN ($2,'v1:work:run:'||$2) ORDER BY id,"createdAt" DESC
) r WHERE payload->'data'->'trainingSelection'->>'availabilityKey'=$3
 ORDER BY "createdAt" DESC LIMIT 1`, memql.BareShortId(run.OwnerUserId), memql.BareShortId(run.RunId), key)
	if err != nil {
		return prior, "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return prior, "", rows.Err()
	}
	node := memorynodes.MemoryNode{Concept: "v1:work:observation"}
	if err := rows.Scan(&node.ID, &node.Payload, &node.CreatedAt); err != nil {
		return prior, "", err
	}
	if !i.training.admit(ctx, node) {
		return prior, "", nil
	}
	var envelope struct {
		Data struct {
			TrainingSelection trainingSelectionReceipt `json:"trainingSelection"`
		}
	}
	if err := json.Unmarshal(node.Payload, &envelope); err != nil {
		return prior, "", err
	}
	prior = envelope.Data.TrainingSelection
	retry, err := time.Parse(time.RFC3339Nano, prior.RetryAt)
	if err != nil || prior.SchemaVersion != 1 || (prior.Status != "embedding-timeout" && prior.Status != "embedding-backoff") || !retry.After(now) || retry.Sub(node.CreatedAt) > 15*time.Minute || node.CreatedAt.After(now) {
		return trainingSelectionReceipt{}, "", nil
	}
	return prior, node.ID, nil
}
