package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server/fileversion"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

var _ pipelinesteps.LibraryArtifactPresentationRetirement = (*pipelinesLibraryStore)(nil)

func pipelineLibraryArtifactID(fileID string) string {
	hash := sha256.Sum256([]byte("v1:library:file:" + fileID))
	return "v1:library:artifact:artifact-" + hex.EncodeToString(hash[:])
}

func pipelineLibraryRow(ctx context.Context, tx *sql.Tx, concept, id string) (map[string]any, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT payload || jsonb_build_object('id',id) FROM "MemoryNodes"
WHERE concept=$1 AND id=$2 ORDER BY "createdAt" DESC LIMIT 1`, concept, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	return row, nil
}

func emptyPipelineLibraryList(value any) bool {
	if value == nil {
		return true
	}
	items, ok := value.([]any)
	return ok && len(items) == 0
}

// Called under the upload owner's transaction lock. The additional metadata
// lock is shared by the Library write fence. The ordinary finalizer must not
// take this lock before its independent engine writes, which take it themselves.
// A user's filing/edit wins before retirement, or the permanent
// retirement fence wins and the delayed edit is refused. No stale cache or
// process-local state decides which bytes may disappear.
func checkPipelineLibraryRetirement(ctx context.Context, tx *sql.Tx, intent pipelineUploadIntent) error {
	key := sha256.Sum256([]byte("pipeline-artifact-library:" + intent.FileID))
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(key[:8]))); err != nil {
		return err
	}
	file, err := pipelineLibraryRow(ctx, tx, "v1:library:file", "v1:library:file:"+intent.FileID)
	if err != nil {
		return err
	}
	kept := func(reason string) error {
		return fmt.Errorf("%w: %s", pipelinesteps.ErrArtifactRetained, reason)
	}
	// Do not expose an older, differently owned/source-stamped version as the
	// head by deleting only the generated versions. Such history is retained.
	var foreignHistory bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "MemoryNodes"
WHERE ((concept='v1:library:file' AND id=$1) OR (concept='v1:library:artifact' AND id=$2))
AND (regexp_replace(payload->>'ownerUserId','^v1:identity:user:','') IS DISTINCT FROM $3
 OR payload->>'source' IS DISTINCT FROM 'pipeline'))`, "v1:library:file:"+intent.FileID, pipelineLibraryArtifactID(intent.FileID), intent.Identity.OwnerUserID).Scan(&foreignHistory); err != nil {
		return err
	}
	if foreignHistory {
		return kept("the Library history includes a different owner or source")
	}
	// Supersession records version one before advancing the head. Its native
	// deterministic ID also protects an interrupted supersession, without a
	// broad history scan or a new index over the whole memory graph.
	history, err := pipelineLibraryRow(ctx, tx, "v1:library:fileVersion", "v1:library:fileVersion:"+fileversion.DerivedVersionId(intent.FileID, 1))
	if err != nil {
		return err
	}
	if history != nil {
		return kept("the Library file has retained history")
	}
	if file != nil {
		if err := matchingPipelineLibraryRow(file, intent); err != nil {
			return kept("the Library file differs from its producing receipt")
		}
		if version := file["versionNumber"]; version != nil && !pipelineSizeMatches(version, 1) {
			return kept("the Library file has retained versions")
		}
		if !emptyPipelineLibraryList(file["trainedIntoDomainIds"]) {
			return kept("the file was added to a knowledge domain")
		}
		if file["status"] != "ready" {
			return kept("the Library file is not ready")
		}
	}
	artifact, err := pipelineLibraryRow(ctx, tx, "v1:library:artifact", pipelineLibraryArtifactID(intent.FileID))
	if err != nil {
		return err
	}
	if artifact != nil {
		if memql.BareShortId(fmt.Sprint(artifact["ownerUserId"])) != intent.Identity.OwnerUserID ||
			artifact["source"] != pipelineFileSource || artifact["sourceConceptRef"] != "v1:library:file:"+intent.FileID ||
			artifact["title"] != intent.Identity.Name {
			return kept("the Library entry differs from the generated file")
		}
		if folder := artifact["folderId"]; folder != nil && folder != "" {
			return kept("the file was filed in a folder")
		}
		if !emptyPipelineLibraryList(artifact["labels"]) || !emptyPipelineLibraryList(artifact["accountIds"]) {
			return kept("the Library entry was labelled")
		}
	}
	return nil
}

// PurgeRetiredRunFile does not decide age, release policy or producer state.
// Its authority is the exact scoped journal plus confirmed permanent storage
// retirement. Presentation deletion never substitutes for that byte receipt.
func (s *pipelinesLibraryStore) PurgeRetiredRunFile(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, intentID string) error {
	var err error
	if scope, err = validatePipelineReceiptScope(ctx, scope, []string{intentID}); err != nil {
		return err
	}
	db, err := s.pipelineReceiptDB()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockPipelineUploadOwner(ctx, tx, scope.OwnerUserID); err != nil {
		return err
	}
	intent, err := readPipelineIntent(ctx, tx, intentID)
	if err != nil {
		return err
	}
	if !pipelineIntentInScope(intent, scope) || intent.State != "retired" {
		return errors.New("Library cleanup requires a retired artifact in the exact owner scope")
	}
	var confirmed bool
	if err := tx.QueryRowContext(ctx, `SELECT retired_etag<>'' AND retirement_lease_id<>''
FROM pipeline_artifact_uploads WHERE intent_id=$1`, intentID).Scan(&confirmed); err != nil {
		return err
	}
	if !confirmed {
		return errors.New("Library cleanup requires a confirmed permanent storage fence")
	}
	if err := checkPipelineLibraryRetirement(ctx, tx, intent); err != nil {
		return err
	}
	// Both identities derive from the immutable receipt. Deleting every version
	// prevents an older append-only version from becoming visible as the head.
	// The journal (including provenance and retirement receipt) stays intact.
	_, err = tx.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE
 ((concept='v1:library:file' AND id=$1) OR (concept='v1:library:artifact' AND id=$2))
 AND regexp_replace(payload->>'ownerUserId','^v1:identity:user:','')=$3
 AND payload->>'source'='pipeline'`, "v1:library:file:"+intent.FileID, pipelineLibraryArtifactID(intent.FileID), scope.OwnerUserID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pipeline_artifact_uploads
SET library_purged_at=coalesce(library_purged_at,clock_timestamp()) WHERE intent_id=$1`, intentID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.notifyRetired != nil {
		s.notifyRetired(ctx, scope.OwnerUserID, intent.FileID)
	}
	return nil
}

func (a *App) notifyRetiredPipelineFile(ctx context.Context, owner, fileID string) {
	a.engine.InvalidateCacheForConcept("v1:library:file")
	a.engine.InvalidateCacheForConcept("v1:library:artifact")
	bus := a.engine.EventBus()
	if bus == nil {
		return
	}
	concept := "v1:library:artifact"
	payload := map[string]any{"id": pipelineLibraryArtifactID(fileID), "concept": concept,
		"ownerUserId": owner, "source": pipelineFileSource, "kind": "file", "lens": "artifact",
		"sourceConceptRef": "v1:library:file:" + fileID}
	event := events.NewEvent(events.BuildTopicWithConcept(events.TopicGraphNodeDeleted, concept), events.KindNodeDeleted, payload).WithMetadata("actor", owner)
	if cause, ok := events.CauseFromContext(ctx); ok {
		event = event.WithCause(cause)
	}
	bus.Publish(event)
}
