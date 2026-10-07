package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// pipelines_library_store.go -- the Library half of a pipeline step (epic
// memql#5478, issue memql#5495): a step's archived log and each of its
// artifacts, filed in the pipeline OWNER's Library as source "pipeline" and
// bound to the work run and the step that produced them.
//
// ===========================================================================
// ONE STORE, TWO RUNNERS, AND NO BUILD TAG
// ===========================================================================
// The workbench node's runner files a cluster step's files (a Job's log and
// artifacts); the agent node's fleet path files a step run on one of the
// owner's machines. Both are handed THIS store, so a person's Library cannot
// tell which surface ran a step -- and it is untagged because the two runners
// live in two binaries. integrations/pipelinesteps declares the port and
// cannot implement it: filing bytes takes the storage client the node built
// and a createLibraryFile row through the engine, and component/server is not
// an integration's to import. It is app/appsession_content_store.go's shape,
// for the same reason.
//
// ===========================================================================
// THE OWNER'S ACTOR, AND NO INTERNAL-ORIGIN STAMP
// ===========================================================================
// createLibraryFile, setLibraryFileStatus and the three quota reads are owner
// constructs: a file is created for the person the call runs as, and the row
// is theirs to read. So every call below borrows the owner's actor and stamps
// nothing -- the owner's authority is exactly the authority these constructs
// want. A file with no owner is REFUSED, never written: ContextWithUserActor
// leaves a blank owner's context as it was, and on the workbench that context
// is the engine's own forward.
//
// ===========================================================================
// THE UPLOAD ROUTE'S RULES, NOT A SECOND SET
// ===========================================================================
// One file is bounded by the route's MEMQL_LIBRARY_MAX_UPLOAD_BYTES and the
// owner's whole Library by its MEMQL_LIBRARY_USER_QUOTA_BYTES, measured the
// way the route measures it: stored files, every earlier version and open
// upload sessions, plus this file. The bytes land at the route's object path,
// library/{owner}/{fileId}/{name}, so one bucket has one layout. The name and
// the content type go through the route's OWN functions, never a copy of
// them: server.SanitizeLibraryFileName (the last path segment, quotes and
// control characters dropped, the dots at either end trimmed) and
// server.ResolveLibraryMIME (parameters stripped, lowercased, sniffed from the
// bytes only when none was given).
//
// What the Library will not take -- a file over the limit, an owner over the
// quota, a node with no object storage -- is an OMISSION, which the runner
// reports as a note beside the step and never as its failure. What breaks on
// the way -- the footprint unreadable, the storage or the engine refusing --
// is an ERROR, which the runner logs and notes the same way.
//
// ===========================================================================
// NOT CONTENT-ADDRESSED, unlike the app session's store
// ===========================================================================
// Every file belongs to the run and the step that made it. Two runs whose
// test logs happen to match are two files: a dedup would bind the second
// run's log to the first run.

// pipelineFileSource is the v1:library:file source a pipeline's files carry.
const pipelineFileSource = "pipeline"

// pipelinesLibraryStore implements pipelinesteps.LibraryStore.
type pipelinesLibraryStore struct {
	// engine runs the file row's create, under the owner's actor.
	engine server.MemQLExecutor
	// library is the Library's own store over the same engine: the owner's
	// storage footprint the quota is measured against, and the ready
	// transition.
	library  *server.EngineLibraryStore
	uploader server.FileUploader
	bucket   string
	logger   *slog.Logger
	// maxBytes bounds one file and quotaBytes the owner's whole Library.
	// Zero takes the Library's own, read at each call:
	// server.LibraryMaxUploadBytes() and server.LibraryUserQuotaBytes().
	maxBytes, quotaBytes int64
	// Short transactions fence streaming uploads across replicas. No session
	// advisory lock survives outside a transaction, so PgBouncer is supported.
	uploadDB func() *sql.DB
}

var _ pipelinesteps.LibraryStore = (*pipelinesLibraryStore)(nil)

// newPipelinesLibraryStore files over engine and the node's object storage.
// A nil uploader or a blank bucket is a node with no object storage: every
// file is then an omission that says so.
func newPipelinesLibraryStore(engine server.MemQLExecutor, uploader server.FileUploader, bucket string, logger *slog.Logger) *pipelinesLibraryStore {
	return &pipelinesLibraryStore{
		engine:   engine,
		library:  server.NewEngineLibraryStore(engine),
		uploader: uploader,
		bucket:   bucket,
		logger:   logger,
	}
}

// pipelinesLibraryStoreFor is the Library store this node's pipeline steps
// file their log and artifacts with, over the node's engine and the blob store
// resolveBlobStore built. The workbench's runner and the agent's fleet path
// are both handed one of these, and nothing else
// (TestEveryPipelineRunnerFilesItsStepsInTheLibrary).
func (a *App) pipelinesLibraryStoreFor(uploader server.FileUploader, bucket string) pipelinesteps.LibraryStore {
	// A NIL INTERFACE when there is no engine, never an adapter around a nil
	// one: the store refuses a nil engine, and would call through an adapter.
	var engine server.MemQLExecutor
	if a.engine != nil {
		engine = &AttachmentEngineAdapter{Engine: a.engine}
	}
	store := newPipelinesLibraryStore(engine, uploader, bucket, a.Logger)
	store.uploadDB = func() *sql.DB {
		if db := a.BunDB(); db != nil {
			return db.DB
		}
		return nil
	}
	return store
}

// StoreRunFile files one of a step's files in its owner's Library.
func (s *pipelinesLibraryStore) StoreRunFile(ctx context.Context, f pipelinesteps.RunFile) (pipelinesteps.StoredFile, error) {
	owner := strings.TrimSpace(f.OwnerUserID)
	if owner == "" {
		return pipelinesteps.StoredFile{}, errors.New("a pipeline's file is filed in its owner's Library, and this one names no owner; nothing was stored")
	}
	size := int64(len(f.Bytes))
	if limit := s.fileLimit(); size > limit {
		return pipelinesteps.StoredFile{Omitted: fmt.Sprintf(
			"%d bytes is above the Library's limit of %d bytes for one file (%s)", size, limit, server.LibraryMaxUploadBytesEnv)}, nil
	}
	if s.uploader == nil || strings.TrimSpace(s.bucket) == "" {
		return pipelinesteps.StoredFile{Omitted: "this node has no object storage configured " +
			"(MEMQL_AZURE_BLOB_CONTAINER and MEMQL_AZURE_STORAGE_CONNECTION_STRING)"}, nil
	}
	if s.engine == nil {
		return pipelinesteps.StoredFile{}, errors.New("this node has no engine to record the file with; nothing was stored")
	}

	// The OWNER's actor for every engine call below, read and write alike.
	ownerCtx := auth.ContextWithUserActor(ctx, owner)

	// THE QUOTA BEFORE A BYTE MOVES, like every other refusal on the upload
	// route. A footprint that cannot be read refuses: a quota read that
	// failed is not a quota that admits.
	fileBytes, versionBytes, sessionBytes, err := s.library.StorageFootprint(ownerCtx)
	if err != nil {
		return pipelinesteps.StoredFile{}, fmt.Errorf("the owner's Library could not be measured against its quota, so nothing was stored: %w", err)
	}
	if total, quota := fileBytes+versionBytes+sessionBytes+size, s.userQuota(); total > quota {
		return pipelinesteps.StoredFile{Omitted: fmt.Sprintf(
			"it would take the owner's Library to %d bytes, over the quota of %d bytes (%s)", total, quota, server.LibraryUserQuotaBytesEnv)}, nil
	}

	// THE BYTES, THEN THE ROW. Never the other way round: a row written first
	// would claim a blobUrl nothing answers.
	name := server.SanitizeLibraryFileName(f.Name)
	// The runner's content type, believed as the route believes a client's
	// (ruling R32b) and normalized as the route normalizes one: the Library
	// serves every byte as an attachment, so a step's .html or .svg is never
	// rendered in the OS's origin, and a stamped type would make a person's
	// own artifact lie about what it is.
	mimeType := server.ResolveLibraryMIME(f.MimeType, f.Bytes)
	fileID := id.NewShortId()
	object := fmt.Sprintf("library/%s/%s/%s", owner, fileID, name)
	blobURL, err := s.uploader.Upload(ctx, s.bucket, object, f.Bytes, mimeType)
	if err != nil {
		return pipelinesteps.StoredFile{}, fmt.Errorf("object storage refused the file, so nothing was stored: %w", err)
	}
	if strings.TrimSpace(blobURL) == "" {
		blobURL = object
	}

	sum := sha256.Sum256(f.Bytes)
	args := map[string]any{
		"fileId":   fileID,
		"name":     name,
		"mimeType": mimeType,
		"size":     len(f.Bytes),
		"sha256":   hex.EncodeToString(sum[:]),
		"blobUrl":  blobURL,
		"source":   pipelineFileSource,
		"format":   server.LibraryFormatForMIME(mimeType),
	}
	// Bound to the work run and the step that produced it. A blank is not a
	// binding, so an unnamed one is left absent rather than written empty.
	if run := strings.TrimSpace(f.WorkRunID); run != "" {
		args["producedByRunId"] = run
	}
	if step := strings.TrimSpace(f.StepKey); step != "" {
		args["producedByStepKey"] = step
	}
	call, err := langparser.RenderCall("createLibraryFile", args)
	if err != nil {
		return pipelinesteps.StoredFile{}, fmt.Errorf("the file's row could not be composed, so it was not recorded: %w", err)
	}
	if _, err := s.engine.Execute(ownerCtx, "mutation "+call); err != nil {
		// THE BYTES STAY BEHIND, AND THIS SAYS SO. No row points at them, so
		// nothing lists, serves or counts them against the owner's quota (the
		// footprint is read from rows), but they occupy the bucket. Taking them
		// back needs a delete the node's uploader does not have:
		// server.FileUploader is Upload alone, and integrations/azureblob
		// implements no delete. app/appsession_content_store.go leaves its
		// bytes the same way when its row write fails after an upload.
		return pipelinesteps.StoredFile{}, fmt.Errorf("the file's bytes are stored but its Library row could not be written: %w", err)
	}

	// READY AT ONCE, as a materialized file is (integrations/compose): nothing
	// analyzes a pipeline's file, so left `stored` it would wait forever on a
	// pass that never comes. The file is in the Library either way -- its
	// bytes and its row exist -- so a failed transition is logged, and its id
	// is still the answer.
	if err := s.library.SetFileStatus(ownerCtx, server.LibraryFileStatusParams{FileId: fileID, Status: "ready"}); err != nil {
		s.log().Warn("pipelines: a step's file is stored, but could not be marked ready; it stays `stored`",
			"component", "pipelinesteps", "fileId", fileID, "workRunId", f.WorkRunID, "stepKey", f.StepKey, "error", err)
	}
	return pipelinesteps.StoredFile{FileID: fileID}, nil
}

func (s *pipelinesLibraryStore) fileLimit() int64 {
	if s.maxBytes > 0 {
		return s.maxBytes
	}
	return server.LibraryMaxUploadBytes()
}

func (s *pipelinesLibraryStore) userQuota() int64 {
	if s.quotaBytes > 0 {
		return s.quotaBytes
	}
	return server.LibraryUserQuotaBytes()
}

func (s *pipelinesLibraryStore) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}
