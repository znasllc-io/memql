package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

var errPipelineUploadStale = errors.New("artifact upload generation has been replaced; stale writer refused")

type pipelineUploadIntent struct {
	ID, FileID, Object, State, URL, ETag string
	Generation                           int64
	Identity                             pipelineStreamIdentity
}

func readPipelineIntent(ctx context.Context, tx *sql.Tx, key string) (pipelineUploadIntent, error) {
	x := pipelineUploadIntent{ID: key}
	var identity []byte
	err := tx.QueryRowContext(ctx, `SELECT identity, file_id, object_key, generation, state, blob_url, etag
FROM pipeline_artifact_uploads WHERE intent_id=$1 FOR UPDATE`, key).Scan(
		&identity, &x.FileID, &x.Object, &x.Generation, &x.State, &x.URL, &x.ETag)
	if err == nil {
		err = json.Unmarshal(identity, &x.Identity)
	}
	return x, err
}

func (s *pipelinesLibraryStore) reserveStream(ctx context.Context, db *sql.DB, identity pipelineStreamIdentity) (pipelineUploadIntent, error) {
	x := pipelineUploadIntent{ID: pipelineIntentID(identity), Identity: identity}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return x, err
	}
	defer tx.Rollback()
	// Serialize this journal's admissions per owner across replicas. The
	// legacy browser/byte[] writers do not yet participate in this lock; this
	// is not a claim of atomic quota enforcement across those other paths.
	if err := lockPipelineUploadOwner(ctx, tx, identity.OwnerUserID); err != nil {
		return x, err
	}
	existing, err := readPipelineIntent(ctx, tx, x.ID)
	if err == nil {
		if existing.Identity != identity {
			return x, errors.New("artifact intent already exists with different immutable metadata")
		}
		if existing.State == "retiring" || existing.State == "retired" {
			return x, errors.New("artifact intent was permanently retired")
		}
		if err := tx.QueryRowContext(ctx, `UPDATE pipeline_artifact_uploads SET generation=generation+1,
updated_at=clock_timestamp() WHERE intent_id=$1 RETURNING generation`, x.ID).Scan(&existing.Generation); err != nil {
			return x, err
		}
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return x, err
	}
	if err := s.admitPipelineStream(ctx, tx, identity); err != nil {
		return x, err
	}
	x.FileID, x.Generation, x.State = id.NewShortId(), 1, "reserved"
	x.Object = fmt.Sprintf("library/%s/%s/%s", identity.OwnerUserID, x.FileID, identity.Name)
	payload, err := json.Marshal(identity)
	if err != nil {
		return x, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_artifact_uploads
(intent_id, owner_user_id, identity, file_id, object_key, size_bytes, generation, state)
VALUES ($1,$2,$3::jsonb,$4,$5,$6,1,'reserved')`, x.ID, identity.OwnerUserID, string(payload), x.FileID, x.Object, identity.Size); err != nil {
		return x, err
	}
	return x, tx.Commit()
}

func lockPipelineUploadOwner(ctx context.Context, tx *sql.Tx, owner string) error {
	key := sha256.Sum256([]byte("pipeline-artifact-quota:" + owner))
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(key[:8])))
	return err
}

// Ready intents still own retained bytes even if their editable Library row
// is removed or points elsewhere. Count until a confirmed retirement fence,
// deduplicating only a current file which names exactly those same bytes. Owner locking also
// covers finalization, so no reservation can disappear between these reads.
func (s *pipelinesLibraryStore) admitPipelineStream(ctx context.Context, tx *sql.Tx, identity pipelineStreamIdentity) error {
	remaining := s.userQuota()
	charge := func(size int64) error {
		if size < 0 || size > remaining {
			return errors.New("artifact reservation exceeds the owner's Library quota")
		}
		remaining -= size
		return nil
	}
	if err := charge(identity.Size); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT file_id, size_bytes, blob_url, identity->>'SHA256', state FROM pipeline_artifact_uploads WHERE owner_user_id=$1", identity.OwnerUserID)
	if err != nil {
		return err
	}
	retained := map[string]azureblob.VerifiedBlob{}
	for rows.Next() {
		var fileID string
		var state string
		var blob azureblob.VerifiedBlob
		if err := rows.Scan(&fileID, &blob.Size, &blob.URL, &blob.SHA256, &state); err != nil {
			rows.Close()
			return err
		}
		charged := blob.Size
		if state == "retired" {
			charged = 0
		}
		if err := charge(charged); err != nil {
			rows.Close()
			return err
		}
		retained[fileID] = blob
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range []string{"libraryFileSizesForOwner", "libraryFileVersionSizesForOwner", "openUploadSessionsForOwner"} {
		result, err := s.engine.Execute(memql.ContextWithFreshRead(ctx), "query "+name+"()")
		if err != nil {
			return fmt.Errorf("cannot reserve artifact capacity: %w", err)
		}
		for _, row := range memql.MaterializeRows(result) {
			size, err := pipelineQuotaSize(row["size"])
			if err != nil {
				return err
			}
			if name == "libraryFileSizesForOwner" {
				blob, exists := retained[memql.BareShortId(fmt.Sprint(row["id"]))]
				if exists && blob.URL != "" && row["blobUrl"] == blob.URL && row["sha256"] == blob.SHA256 && size == blob.Size {
					continue
				}
			}
			if err := charge(size); err != nil {
				return err
			}
		}
	}
	return nil
}

func pipelineQuotaSize(value any) (int64, error) {
	var size int64
	switch v := value.(type) {
	case int64:
		size = v
	case int:
		size = int64(v)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= math.MaxInt64 || v != math.Trunc(v) {
			return 0, errors.New("Library quota read contains an invalid size")
		}
		size = num.ClampFloat64ToInt64(v)
	case json.Number:
		var err error
		size, err = v.Int64()
		if err != nil {
			return 0, errors.New("Library quota read contains an invalid size")
		}
	default:
		return 0, errors.New("Library quota read contains no numeric size")
	}
	if size < 0 {
		return 0, errors.New("Library quota read contains a negative size")
	}
	return size, nil
}

func savePipelineVerified(ctx context.Context, db *sql.DB, intent pipelineUploadIntent, blob azureblob.VerifiedBlob) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := readPipelineIntent(ctx, tx, intent.ID)
	if err != nil {
		return err
	}
	if current.Generation != intent.Generation || (current.State != "reserved" && current.State != "blob_verified" && current.State != "ready") {
		return errPipelineUploadStale
	}
	if current.ETag != "" && (current.ETag != blob.ETag || current.URL != blob.URL) {
		return errors.New("artifact object version changed from its durable receipt")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pipeline_artifact_uploads SET blob_url=$2, etag=$3,
state=CASE WHEN state='ready' THEN state ELSE 'blob_verified' END, updated_at=clock_timestamp()
WHERE intent_id=$1`, intent.ID, blob.URL, blob.ETag); err != nil {
		return err
	}
	return tx.Commit()
}
