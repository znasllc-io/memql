package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type pipelineLegacyVerifier interface {
	VerifyStream(context.Context, string, string, int64, string) (azureblob.VerifiedBlob, error)
}

func (s *pipelinesLibraryStore) AdoptLegacyRunLog(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, fileID string) (pipelinesteps.StoredFileReceipt, error) {
	var zero pipelinesteps.StoredFileReceipt
	var err error
	if scope, err = validatePipelineReceiptScope(ctx, scope, nil); err != nil {
		return zero, err
	}
	if fileID == "" || len(fileID) > 256 || strings.ContainsAny(fileID, "/\\:") {
		return zero, errors.New("legacy log requires one bare file identity")
	}
	db, err := s.pipelineReceiptDB()
	if err != nil {
		return zero, err
	}
	// Repeated adoption resolves the same immutable receipt, including a file
	// already retired. It never recreates a deleted presentation or object.
	var existing string
	err = db.QueryRowContext(ctx, `SELECT intent_id FROM pipeline_artifact_uploads WHERE file_id=$1`, fileID).Scan(&existing)
	if err == nil {
		return s.existingLegacyLogReceipt(ctx, db, scope, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	row, err := s.pipelineLibraryRow(ctx, fileID)
	if err != nil {
		return zero, err
	}
	if row == nil {
		return zero, errors.New("legacy log is unavailable to its owner")
	}
	var fields struct {
		Name     string
		MimeType string
		Size     int64
		SHA256   string
		BlobURL  string
	}
	encoded, err := json.Marshal(row)
	if err != nil || json.Unmarshal(encoded, &fields) != nil {
		return zero, errors.New("legacy log metadata is invalid")
	}
	if !strings.HasSuffix(fields.Name, ".log") || fields.MimeType != "text/plain" || fields.Size < 0 || fields.Size > 32<<20 || server.SanitizeLibraryFileName(fields.Name) != fields.Name {
		return zero, errors.New("legacy log exceeds the supported name, type or byte bound")
	}
	x, err := s.streamIdentity(pipelinesteps.StreamRunFile{OwnerUserID: scope.OwnerUserID, WorkRunID: scope.WorkRunID, StepKey: scope.StepKey, Attempt: scope.Attempt,
		Path: ".memql-legacy-log/" + fileID + "/" + fields.Name, Name: fields.Name, MimeType: fields.MimeType, Size: fields.Size, SHA256: fields.SHA256, Body: strings.NewReader("")})
	if err != nil {
		return zero, err
	}
	intent := pipelineUploadIntent{Identity: x, FileID: fileID, ID: pipelineIntentID(x), Object: fmt.Sprintf("library/%s/%s/%s", scope.OwnerUserID, fileID, fields.Name), URL: fields.BlobURL, Generation: 1, State: "ready"}
	if err := matchingPipelineLibraryRow(row, intent); err != nil {
		return zero, err
	}
	verifier, ok := s.uploader.(pipelineLegacyVerifier)
	if !ok {
		return zero, errors.New("legacy log adoption requires bounded object verification")
	}
	// Always derive the object from owner/file/name. Never follow a row's URL.
	verified, err := verifier.VerifyStream(ctx, x.Container, intent.Object, x.Size, x.SHA256)
	if err != nil {
		return zero, err
	}
	if verified.ETag == "" || verified.Size != x.Size || verified.SHA256 != x.SHA256 || (intent.URL != verified.URL && intent.URL != intent.Object) {
		return zero, errors.New("legacy log object does not match its Library record")
	}
	intent.ETag = verified.ETag
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if err := lockPipelineUploadOwner(ctx, tx, scope.OwnerUserID); err != nil {
		return zero, err
	}
	if err := checkPipelineLibraryRetirement(ctx, tx, intent); err != nil {
		return zero, err
	}
	// The metadata lock also fences edits of pre-journal files. Recheck the
	// exact bytes and presentation after verification, before adoption commits.
	var found string
	err = tx.QueryRowContext(ctx, `SELECT intent_id FROM pipeline_artifact_uploads WHERE file_id=$1`, fileID).Scan(&found)
	if err == nil {
		current, err := readPipelineIntent(ctx, tx, found)
		if err != nil {
			return zero, err
		}
		if !pipelineIntentInScope(current, scope) {
			return zero, errors.New("legacy log already belongs to another producer scope")
		}
		return pipelineIntentReceipt(current), tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	identity, err := json.Marshal(x)
	if err != nil {
		return zero, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pipeline_artifact_uploads(intent_id,owner_user_id,identity,file_id,object_key,size_bytes,generation,state,blob_url,etag)
VALUES($1,$2,$3::jsonb,$4,$5,$6,1,'ready',$7,$8)`, intent.ID, scope.OwnerUserID, string(identity), fileID, intent.Object, x.Size, intent.URL, intent.ETag)
	if err != nil {
		return zero, err
	}
	return pipelineIntentReceipt(intent), tx.Commit()
}

func (s *pipelinesLibraryStore) existingLegacyLogReceipt(ctx context.Context, db *sql.DB, scope pipelinesteps.RunFileReceiptScope, key string) (pipelinesteps.StoredFileReceipt, error) {
	var zero pipelinesteps.StoredFileReceipt
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if err := lockPipelineUploadOwner(ctx, tx, scope.OwnerUserID); err != nil {
		return zero, err
	}
	current, err := readPipelineIntent(ctx, tx, key)
	if err != nil {
		return zero, err
	}
	if !pipelineIntentInScope(current, scope) {
		return zero, errors.New("legacy log already belongs to another producer scope")
	}
	return pipelineIntentReceipt(current), tx.Commit()
}
