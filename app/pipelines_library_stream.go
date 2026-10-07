package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type pipelineVerifiedUploader interface {
	CreateVerifiedStream(context.Context, string, string, io.Reader, int64, string, string) (azureblob.VerifiedBlob, error)
}

var _ pipelinesteps.StreamLibraryStore = (*pipelinesLibraryStore)(nil)

// StoreRunFileStream is one bounded storage effect, not pipeline policy. The
// caller selects artifacts and required/optional handling in the DSL workflow.
// Intents and reservations never expire: a timeout does not prove that an old
// producer cannot still commit. An interrupted call is resumed with its exact
// identity; a different attempt gets a different intent.
func (s *pipelinesLibraryStore) StoreRunFileStream(ctx context.Context, f pipelinesteps.StreamRunFile) (pipelinesteps.StoredFile, error) {
	identity, err := s.streamIdentity(f)
	if err != nil {
		return pipelinesteps.StoredFile{}, err
	}
	uploader, ok := s.uploader.(pipelineVerifiedUploader)
	if !ok || s.engine == nil || s.uploadDB == nil {
		return pipelinesteps.StoredFile{}, errors.New("streamed Library storage requires verified object storage, an engine and a durable upload journal")
	}
	db := s.uploadDB()
	if db == nil {
		return pipelinesteps.StoredFile{}, errors.New("streamed Library storage has no durable upload journal")
	}
	ownerCtx := memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, identity.OwnerUserID))
	intent, err := s.reserveStream(ownerCtx, db, identity)
	if err != nil {
		return pipelinesteps.StoredFile{}, err
	}
	verified, err := uploader.CreateVerifiedStream(ctx, identity.Container, intent.Object, f.Body, identity.Size, identity.SHA256, identity.MimeType)
	if err != nil {
		// Even a transport error can follow a successful remote commit. Keep
		// identity and capacity; a replacement verifies the same destination.
		return pipelinesteps.StoredFile{}, fmt.Errorf("artifact upload retained for reconciliation: %w", err)
	}
	if verified.ETag == "" || verified.URL == "" || verified.Size != identity.Size || verified.SHA256 != identity.SHA256 {
		return pipelinesteps.StoredFile{}, errors.New("artifact storage returned an incomplete or disagreeing verified receipt")
	}
	// Persist the verified ETag before attempting the Library row. A failed
	// row write must not make a replacement forget the observed object version.
	if err := savePipelineVerified(ctx, db, intent, verified); err != nil {
		return pipelinesteps.StoredFile{}, err
	}
	intent.URL, intent.ETag = verified.URL, verified.ETag
	if err := s.finalizeStream(ownerCtx, db, intent); err != nil {
		return pipelinesteps.StoredFile{}, fmt.Errorf("verified artifact retained; Library finalization requires reconciliation: %w", err)
	}
	receipt := pipelineIntentReceipt(intent)
	return pipelinesteps.StoredFile{FileID: intent.FileID, Receipt: &receipt}, nil
}

// All non-stream fields are immutable. JSON is persisted as a structured
// object and compared after decoding, never by jsonb's whitespace/key order.
type pipelineStreamIdentity struct {
	OwnerUserID, WorkRunID, StepKey string
	Attempt                         int
	Path, Name, MimeType, Container string
	Size                            int64
	SHA256                          string
}

func (s *pipelinesLibraryStore) streamIdentity(f pipelinesteps.StreamRunFile) (pipelineStreamIdentity, error) {
	x := pipelineStreamIdentity{
		OwnerUserID: memql.BareShortId(strings.TrimSpace(f.OwnerUserID)),
		WorkRunID:   memql.BareShortId(strings.TrimSpace(f.WorkRunID)), StepKey: f.StepKey,
		Attempt: f.Attempt, Path: f.Path, Name: server.SanitizeLibraryFileName(f.Name),
		MimeType: server.ResolveLibraryMIME(f.MimeType, nil), Container: strings.TrimSpace(s.bucket),
		Size: f.Size, SHA256: f.SHA256,
	}
	for _, field := range []string{x.OwnerUserID, x.WorkRunID, x.StepKey, x.Path, x.Name, x.MimeType, x.Container} {
		if field == "" || len(field) > 4096 || !utf8.ValidString(field) || strings.IndexFunc(field, unicode.IsControl) >= 0 {
			return x, errors.New("streamed artifact requires bounded nonempty owner, run, step, path, name, MIME and container")
		}
	}
	if strings.ContainsAny(x.OwnerUserID, "/\\") || x.Attempt < 1 || f.Body == nil {
		return x, errors.New("streamed artifact requires an owner, a positive attempt and a source")
	}
	if path.IsAbs(x.Path) || path.Clean(x.Path) != x.Path || x.Path == "." || x.Path == ".." || strings.HasPrefix(x.Path, "../") || strings.Contains(x.Path, "\\") {
		return x, errors.New("artifact path must be canonical and relative to the export")
	}
	if len(x.SHA256) != 64 {
		return x, errors.New("streamed artifact requires a lowercase SHA-256 digest")
	}
	digest, err := hex.DecodeString(x.SHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != x.SHA256 {
		return x, errors.New("streamed artifact requires a lowercase SHA-256 digest")
	}
	if x.Size < 0 || x.Size > min(s.fileLimit(), azureblob.MaxVerifiedStreamBytes) {
		return x, errors.New("streamed artifact exceeds the Library or verified-stream size limit")
	}
	return x, nil
}

func (s *pipelinesLibraryStore) finalizeStream(ctx context.Context, db *sql.DB, intent pipelineUploadIntent) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockPipelineUploadOwner(ctx, tx, intent.Identity.OwnerUserID); err != nil {
		return err
	}
	current, err := readPipelineIntent(ctx, tx, intent.ID)
	if err != nil {
		return err
	}
	if current.Generation != intent.Generation || (current.State != "blob_verified" && current.State != "ready") {
		return errPipelineUploadStale
	}
	if current.ETag != intent.ETag || current.URL != intent.URL || current.State == "reserved" {
		return errors.New("artifact journal has no matching verified object")
	}
	// Hold the journal row lock while the independent engine writes. A new
	// generation cannot pass this finalizer. If the SQL response is lost, the
	// next generation reads the row and adopts only the exact matching file.
	row, err := s.pipelineLibraryRow(ctx, intent.FileID)
	if err != nil {
		return err
	}
	if row == nil {
		x := intent.Identity
		call, err := langparser.RenderCall("recordStoredPipelineFile", map[string]any{
			"fileId": intent.FileID, "name": x.Name, "mimeType": x.MimeType,
			"size": x.Size, "sha256": x.SHA256, "blobUrl": intent.URL,
			"format": server.LibraryFormatForMIME(x.MimeType), "producedByRunId": x.WorkRunID, "producedByStepKey": x.StepKey,
		})
		if err != nil {
			return err
		}
		if _, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
			return err
		}
		row, err = s.pipelineLibraryRow(ctx, intent.FileID)
		if err != nil {
			return err
		}
	}
	if err := matchingPipelineLibraryRow(row, intent); err != nil {
		return err
	}
	if row["status"] == "stored" {
		if err := s.library.SetFileStatus(ctx, server.LibraryFileStatusParams{FileId: intent.FileID, Status: "ready"}); err != nil {
			return err
		}
		row, err = s.pipelineLibraryRow(ctx, intent.FileID)
		if err != nil {
			return err
		}
	}
	if err := matchingPipelineLibraryRow(row, intent); err != nil {
		return err
	}
	if row["status"] != "ready" {
		return errors.New("pipeline Library file is not ready")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE pipeline_artifact_uploads SET state='ready', updated_at=clock_timestamp() WHERE intent_id=$1 AND generation=$2", intent.ID, intent.Generation); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *pipelinesLibraryStore) pipelineLibraryRow(ctx context.Context, fileID string) (map[string]any, error) {
	call, err := langparser.RenderCall("libraryFileById", map[string]any{"fileId": fileID})
	if err != nil {
		return nil, err
	}
	result, err := s.engine.Execute(memql.ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) != 1 {
		return nil, errors.New("artifact file identity resolved to multiple Library rows")
	}
	return rows[0], nil
}

func matchingPipelineLibraryRow(row map[string]any, intent pipelineUploadIntent) error {
	if row == nil {
		return errors.New("artifact Library row is absent")
	}
	x := intent.Identity
	for key, want := range map[string]string{
		"name": x.Name, "mimeType": x.MimeType, "sha256": x.SHA256, "blobUrl": intent.URL,
		"source": pipelineFileSource, "producedByStepKey": x.StepKey,
	} {
		if fmt.Sprint(row[key]) != want {
			return fmt.Errorf("artifact Library row disagrees with its durable %s", key)
		}
	}
	// The executor's JSON projection may carry a float64. Compare numerically
	// in the verified stream's exact 0..2 GiB domain, never via fmt's exponent
	// notation or a truncating conversion that would accept a fractional size.
	if !pipelineSizeMatches(row["size"], x.Size) {
		return errors.New("artifact Library row disagrees with its durable size")
	}
	for key, want := range map[string]string{"id": intent.FileID, "ownerUserId": x.OwnerUserID, "producedByRunId": x.WorkRunID} {
		if memql.BareShortId(fmt.Sprint(row[key])) != want {
			return fmt.Errorf("artifact Library row disagrees with its durable %s", key)
		}
	}
	return nil
}

func pipelineSizeMatches(value any, expected int64) bool {
	switch v := value.(type) {
	case float64:
		return v == float64(expected)
	case int64:
		return v == expected
	case int:
		return int64(v) == expected
	case json.Number:
		n, err := v.Int64()
		return err == nil && n == expected
	default:
		return false
	}
}

func pipelineIntentID(x pipelineStreamIdentity) string {
	key, _ := json.Marshal([]any{x.OwnerUserID, x.WorkRunID, x.StepKey, x.Attempt, x.Path})
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}
