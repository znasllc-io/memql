package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

var _ pipelinesteps.LibraryArtifactLifecycle = (*pipelinesLibraryStore)(nil)

type pipelineRetirementUploader interface {
	RetireVerifiedStream(context.Context, string, string, azureblob.VerifiedBlob, string) (azureblob.RetiredBlob, error)
}

// PinRunFileReceipts makes one authorized consumer's references durable before
// it verifies and uses the bytes. Every referenced intent and retirement share
// an owner/intent lock order. A replacement process can repeat the same call.
func (s *pipelinesLibraryStore) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	return s.changeRunFileReference(ctx, ref, false)
}

// ReleaseRunFileReference permanently closes this consumer identity. Recording
// release even before a delayed pin arrives prevents that pin resurrecting it.
// The caller must first durably retire the consumer; this is not retention policy.
func (s *pipelinesLibraryStore) ReleaseRunFileReference(ctx context.Context, ref pipelinesteps.RunFileReference) error {
	_, err := s.changeRunFileReference(ctx, ref, true)
	return err
}

func (s *pipelinesLibraryStore) changeRunFileReference(ctx context.Context, ref pipelinesteps.RunFileReference, release bool) ([]pipelinesteps.StoredFileReceipt, error) {
	var err error
	if ref.Scope, err = validatePipelineReceiptScope(ctx, ref.Scope, ref.IntentIDs); err != nil {
		return nil, err
	}
	if len(ref.IntentIDs) == 0 || len(ref.ReferenceID) == 0 || len(ref.ReferenceID) > 512 || !utf8.ValidString(ref.ReferenceID) || strings.IndexFunc(ref.ReferenceID, unicode.IsControl) >= 0 {
		return nil, errors.New("artifact reference requires a bounded consumer identity and a nonempty artifact set")
	}
	db, err := s.pipelineReceiptDB()
	if err != nil {
		return nil, err
	}
	ids := slices.Clone(ref.IntentIDs)
	slices.Sort(ids)
	scopeJSON, _ := json.Marshal(ref.Scope)
	idsJSON, _ := json.Marshal(ids)
	keyJSON, _ := json.Marshal([]any{ref.Scope, ref.ReferenceID})
	hash := sha256.Sum256(keyJSON)
	key := hex.EncodeToString(hash[:])
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockPipelineUploadOwner(ctx, tx, ref.Scope.OwnerUserID); err != nil {
		return nil, err
	}
	var state string
	var oldIDs, oldScope []byte
	err = tx.QueryRowContext(ctx, `SELECT state,intent_ids,scope FROM pipeline_artifact_references
WHERE reference_key=$1 AND owner_user_id=$2 FOR UPDATE`, key, ref.Scope.OwnerUserID).Scan(&state, &oldIDs, &oldScope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		var storedIDs []string
		var storedScope pipelinesteps.RunFileReceiptScope
		if json.Unmarshal(oldIDs, &storedIDs) != nil || json.Unmarshal(oldScope, &storedScope) != nil || storedScope != ref.Scope || !slices.Equal(storedIDs, ids) {
			return nil, errors.New("artifact reference already names a different immutable scope or artifact set")
		}
		if state == "released" && !release {
			return nil, errors.New("artifact reference was permanently released")
		}
	}
	var receipts []pipelinesteps.StoredFileReceipt
	if !release {
		for _, intentID := range ids {
			intent, err := readPipelineIntent(ctx, tx, intentID)
			if err != nil {
				return nil, err
			}
			if !pipelineIntentInScope(intent, ref.Scope) || intent.State != "ready" {
				return nil, errors.New("artifact is unavailable for pinning in this scope")
			}
		}
		receipts, err = readPipelineReceipts(ctx, tx, ref.Scope, ref.IntentIDs)
		if err != nil {
			return nil, err
		}
	}
	newState := "active"
	if release {
		newState = "released"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pipeline_artifact_references
(reference_key,owner_user_id,reference_id,scope,intent_ids,state) VALUES ($1,$2,$3,$4::jsonb,$5::jsonb,$6)
ON CONFLICT (reference_key) DO UPDATE SET state=EXCLUDED.state,updated_at=clock_timestamp()`,
		key, ref.Scope.OwnerUserID, ref.ReferenceID, string(scopeJSON), string(idsJSON), newState)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return receipts, nil
}

// RetireRunFile fences one unreferenced intent before contacting storage. A
// lost reply keeps its capacity reserved. Only a confirmed permanent provider
// fence permits the journal to release that capacity. No elapsed-time rule is
// hidden here: the authorized workflow chooses which artifact to retire.
func (s *pipelinesLibraryStore) RetireRunFile(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, intentID string) (pipelinesteps.RetiredFileReceipt, error) {
	var zero pipelinesteps.RetiredFileReceipt
	var err error
	if scope, err = validatePipelineReceiptScope(ctx, scope, []string{intentID}); err != nil {
		return zero, err
	}
	uploader, ok := s.uploader.(pipelineRetirementUploader)
	if !ok {
		return zero, errors.New("artifact retirement requires a durable journal and a permanent provider fence")
	}
	db, err := s.pipelineReceiptDB()
	if err != nil {
		return zero, err
	}
	intent, token, retiredETag, err := beginPipelineRetirement(ctx, db, scope, intentID)
	if err != nil {
		return zero, err
	}
	if intent.State == "retired" {
		return pipelinesteps.RetiredFileReceipt{IntentID: intent.ID, FileID: intent.FileID, TombstoneETag: retiredETag}, nil
	}
	x := intent.Identity
	retired, err := uploader.RetireVerifiedStream(ctx, x.Container, intent.Object, azureblob.VerifiedBlob{URL: intent.URL, ETag: intent.ETag, Size: x.Size, SHA256: x.SHA256}, token)
	if err != nil {
		return zero, fmt.Errorf("artifact retirement retained for reconciliation: %w", err)
	}
	if retired.ETag == "" {
		return zero, errors.New("artifact retirement returned no tombstone receipt")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if err := lockPipelineUploadOwner(ctx, tx, scope.OwnerUserID); err != nil {
		return zero, err
	}
	current, err := readPipelineIntent(ctx, tx, intentID)
	if err != nil {
		return zero, err
	}
	if current.Generation != intent.Generation || (current.State != "retiring" && current.State != "retired") {
		return zero, errPipelineUploadStale
	}
	updated, err := tx.ExecContext(ctx, `UPDATE pipeline_artifact_uploads SET state='retired',retired_etag=$2,updated_at=clock_timestamp()
WHERE intent_id=$1 AND retirement_lease_id=$3`, intentID, retired.ETag, token)
	if err != nil {
		return zero, err
	}
	if count, err := updated.RowsAffected(); err != nil || count != 1 {
		return zero, errors.New("artifact retirement journal did not retain its fence receipt")
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return pipelinesteps.RetiredFileReceipt{IntentID: intent.ID, FileID: intent.FileID, TombstoneETag: retired.ETag}, nil
}

func beginPipelineRetirement(ctx context.Context, db *sql.DB, scope pipelinesteps.RunFileReceiptScope, key string) (pipelineUploadIntent, string, string, error) {
	var intent pipelineUploadIntent
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return intent, "", "", err
	}
	defer tx.Rollback()
	if err := lockPipelineUploadOwner(ctx, tx, scope.OwnerUserID); err != nil {
		return intent, "", "", err
	}
	intent, err = readPipelineIntent(ctx, tx, key)
	if err != nil {
		return intent, "", "", err
	}
	if !pipelineIntentInScope(intent, scope) {
		return intent, "", "", errors.New("artifact is unavailable in the retirement scope")
	}
	var pinned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pipeline_artifact_references
WHERE owner_user_id=$1 AND state='active' AND intent_ids ? $2)`, scope.OwnerUserID, key).Scan(&pinned); err != nil {
		return intent, "", "", err
	}
	if pinned {
		return intent, "", "", errors.New("artifact is retained by an active durable reference")
	}
	var token, retiredETag string
	if err := tx.QueryRowContext(ctx, "SELECT retirement_lease_id,retired_etag FROM pipeline_artifact_uploads WHERE intent_id=$1", key).Scan(&token, &retiredETag); err != nil {
		return intent, "", "", err
	}
	switch intent.State {
	case "retiring", "retired":
		if token == "" || (intent.State == "retired" && retiredETag == "") {
			return intent, "", "", errors.New("artifact retirement journal is incomplete")
		}
	case "reserved", "blob_verified", "ready":
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return intent, "", "", err
		}
		random[6] = (random[6] & 0x0f) | 0x40
		random[8] = (random[8] & 0x3f) | 0x80
		token = fmt.Sprintf("%x-%x-%x-%x-%x", random[:4], random[4:6], random[6:8], random[8:10], random[10:])
		if err := tx.QueryRowContext(ctx, `UPDATE pipeline_artifact_uploads SET state='retiring',generation=generation+1,
retirement_lease_id=$2,updated_at=clock_timestamp() WHERE intent_id=$1 RETURNING generation`, key, token).Scan(&intent.Generation); err != nil {
			return intent, "", "", err
		}
		intent.State = "retiring"
	default:
		return intent, "", "", errors.New("artifact journal has an unknown state")
	}
	if err := tx.Commit(); err != nil {
		return intent, "", "", err
	}
	return intent, token, retiredETag, nil
}

func pipelineIntentInScope(intent pipelineUploadIntent, scope pipelinesteps.RunFileReceiptScope) bool {
	x := intent.Identity
	return intent.ID == pipelineIntentID(x) && x.OwnerUserID == scope.OwnerUserID && x.WorkRunID == scope.WorkRunID && x.StepKey == scope.StepKey && x.Attempt == scope.Attempt
}
