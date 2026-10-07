package app

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

const maxPipelineReceiptIDs = 1024

type pipelineReceiptQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

var _ pipelinesteps.LibraryReceiptReader = (*pipelinesLibraryStore)(nil)

// ReadRunFileReceipts is a server-only native journal read. Run authorization
// happens at the caller, which supplies the verified scope and owner actor.
// The SQL repeats every scope predicate; an ID alone is never authority.
func (s *pipelinesLibraryStore) ReadRunFileReceipts(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, ids []string) ([]pipelinesteps.StoredFileReceipt, error) {
	var err error
	if scope, err = validatePipelineReceiptScope(ctx, scope, ids); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []pipelinesteps.StoredFileReceipt{}, nil
	}
	db, err := s.pipelineReceiptDB()
	if err != nil {
		return nil, err
	}
	return readPipelineReceipts(ctx, db, scope, ids)
}

func (s *pipelinesLibraryStore) pipelineReceiptDB() (*sql.DB, error) {
	if s.uploadDB != nil {
		if db := s.uploadDB(); db != nil {
			return db, nil
		}
	}
	return nil, errors.New("artifact receipt journal is unavailable")
}

func validatePipelineReceiptScope(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, ids []string) (pipelinesteps.RunFileReceiptScope, error) {
	scope.OwnerUserID = memql.BareShortId(strings.TrimSpace(scope.OwnerUserID))
	scope.WorkRunID = memql.BareShortId(strings.TrimSpace(scope.WorkRunID))
	access, _ := auth.AccessFromContext(ctx)
	if auth.OriginFromContext(ctx) != auth.OriginInternal || access == nil || scope.OwnerUserID == "" || memql.BareShortId(access.UserId) != scope.OwnerUserID {
		return scope, errors.New("artifact receipts require a trusted server call under the scoped owner's actor")
	}
	for _, value := range []string{scope.OwnerUserID, scope.WorkRunID, scope.StepKey} {
		if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return scope, errors.New("artifact receipt scope is invalid")
		}
	}
	if scope.Attempt < 1 || len(ids) > maxPipelineReceiptIDs {
		return scope, errors.New("artifact receipt read requires a positive attempt and at most 1024 IDs")
	}
	seen := make(map[string]bool, len(ids))
	for _, key := range ids {
		if len(key) != 64 {
			return scope, errors.New("artifact receipt IDs must be 64 characters")
		}
		decoded, err := hex.DecodeString(key)
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != key || seen[key] {
			return scope, errors.New("artifact receipt IDs must be unique lowercase SHA-256 identities")
		}
		seen[key] = true
	}
	return scope, nil
}

func readPipelineReceipts(ctx context.Context, db pipelineReceiptQuerier, scope pipelinesteps.RunFileReceiptScope, ids []string) ([]pipelinesteps.StoredFileReceipt, error) {
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
SELECT upload.intent_id, upload.identity, upload.file_id, upload.object_key, upload.blob_url, upload.etag
FROM jsonb_array_elements_text($1::jsonb) WITH ORDINALITY AS requested(intent_id, ordinal)
JOIN pipeline_artifact_uploads AS upload ON upload.intent_id=requested.intent_id
WHERE upload.state='ready' AND upload.owner_user_id=$2
  AND upload.identity->>'OwnerUserID'=$2 AND upload.identity->>'WorkRunID'=$3
  AND upload.identity->>'StepKey'=$4 AND upload.identity->>'Attempt'=$5
ORDER BY requested.ordinal`, string(encoded), scope.OwnerUserID, scope.WorkRunID, scope.StepKey, strconv.Itoa(scope.Attempt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]pipelinesteps.StoredFileReceipt, 0, len(ids))
	for rows.Next() {
		var intent pipelineUploadIntent
		var identity []byte
		if err := rows.Scan(&intent.ID, &identity, &intent.FileID, &intent.Object, &intent.URL, &intent.ETag); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(identity, &intent.Identity); err != nil {
			return nil, err
		}
		if intent.ID != pipelineIntentID(intent.Identity) || intent.FileID == "" || intent.Object == "" || intent.URL == "" || intent.ETag == "" {
			return nil, errors.New("artifact journal has an incomplete or inconsistent ready receipt")
		}
		result = append(result, pipelineIntentReceipt(intent))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != len(ids) {
		return nil, errors.New("artifact receipts are unavailable for this owner, run, step and attempt")
	}
	return result, nil
}

func pipelineIntentReceipt(intent pipelineUploadIntent) pipelinesteps.StoredFileReceipt {
	x := intent.Identity
	return pipelinesteps.StoredFileReceipt{
		IntentID: intent.ID, FileID: intent.FileID, OwnerUserID: x.OwnerUserID,
		WorkRunID: x.WorkRunID, StepKey: x.StepKey, Attempt: x.Attempt, Path: x.Path,
		Container: x.Container, Object: intent.Object, URL: intent.URL, ETag: intent.ETag,
		Size: x.Size, SHA256: x.SHA256,
	}
}
