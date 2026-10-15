package filebackups

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/fileversion"
)

func (s *Service) execute(ctx context.Context, kind, name string, args map[string]any) ([]map[string]any, error) {
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	result, err := s.Engine.Execute(memql.ContextWithFreshRead(ctx), kind+" "+call)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(result), nil
}

func (s *Service) publishSyncFile(ctx context.Context, t *Transfer) error {
	unlock, err := memql.AcquireWriteGate(ctx, s.GateDB(), "library-backup-source:"+t.Owner+":"+t.WorkerID+":"+t.Path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.checkSync(ctx, t.WorkerID, t.Path, t.Metadata.WatchID); err != nil {
		return err
	}
	ctx = memql.ContextWithFreshRead(ctx)
	current, err := s.Library.FileByUploadedFrom(ctx, t.WorkerID, t.Path)
	if err != nil {
		return err
	}
	// The generated destination is immutable and unique to this transfer. Lost
	// row-write replies adopt only the exact same object and digest.
	ref := t.Object
	if current != nil && (current.BlobUrl == t.Object || strings.HasSuffix(current.BlobUrl, "/"+t.Object)) && current.Sha256 == t.SHA256 {
		t.Metadata.FileID = current.ID
		t.Metadata.VersionNumber = current.VersionNumber
		err = s.Library.RestampFileArtifact(ctx, current.ID)
		return err
	}
	mime := t.Metadata.MimeType
	if mime == "" {
		mime = "application/octet-stream"
	}
	if current != nil {
		if current.VersionNumber != t.Metadata.ExpectedVersion || current.Archived {
			return errors.New("the cloud file changed; resolve the conflict before syncing")
		}
		fileID := current.ID
		err = s.Library.SupersedeFile(ctx, server.LibraryVersionSnapshot{
			VersionId: fileversion.DerivedVersionId(fileID, current.VersionNumber), FileId: fileID, VersionNumber: current.VersionNumber,
			Name: current.Name, MimeType: current.MimeType, Size: int64(current.Size), Sha256: current.Sha256, BlobUrl: current.BlobUrl,
			Format: current.Format, Summary: current.Summary, UploadedFromWorkerId: current.UploadedFromWorkerId,
			UploadedFromWorkerName: current.UploadedFromWorkerName, UploadedFromPath: current.UploadedFromPath, UploadedAt: current.VersionUploadedAt,
		}, server.LibraryHeadMove{FileId: fileID, VersionNumber: current.VersionNumber + 1, Name: t.Name, MimeType: mime, Size: t.Size,
			Sha256: t.SHA256, BlobUrl: ref, Format: server.LibraryFormatForMIME(mime), BackupWorkerId: t.WorkerID, BackupPath: t.Path, LinkState: "synced",
			UploadedFromWorkerId: t.WorkerID, UploadedFromWorkerName: t.Metadata.WorkerName, UploadedFromPath: t.Path, VersionAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return err
		}
		t.Metadata.FileID = fileID
		t.Metadata.VersionNumber = current.VersionNumber + 1
	} else {
		if t.Metadata.ExpectedVersion > 0 {
			return errors.New("the cloud file was removed; the sync will not replace it silently")
		}
		existing, err := s.Library.File(ctx, server.LibraryFileConceptRef(t.Metadata.FileID))
		if err != nil {
			return err
		}
		if existing != nil && existing.BlobUrl != ref {
			return errors.New("the file identity already names different data")
		}
		if existing == nil {
			err = s.Library.CreateFile(ctx, server.LibraryFileCreateParams{FileId: t.Metadata.FileID, Name: t.Name, MimeType: mime, Size: int(t.Size), Sha256: t.SHA256, BlobUrl: ref,
				Source: "uploaded", Format: server.LibraryFormatForMIME(mime), FolderId: t.Metadata.FolderID, UploadedFromWorkerId: t.WorkerID, UploadedFromWorkerName: t.Metadata.WorkerName, UploadedFromPath: t.Path})
			if err != nil {
				return err
			}
		}
		t.Metadata.VersionNumber = 1
	}
	if err = s.Library.SetFileLinkState(ctx, t.Metadata.FileID, "synced"); err != nil {
		return err
	}
	// Backup bytes are not implicitly sent to an analysis/embedding pipeline.
	if err = s.Library.SetFileStatus(ctx, server.LibraryFileStatusParams{FileId: t.Metadata.FileID, Status: "ready"}); err != nil {
		return err
	}
	err = s.Library.RestampFileArtifact(ctx, t.Metadata.FileID)
	return err
}

// SyncDelete is a conditional move to the Bin, never a storage purge. The
// worker must have a complete scan and confirm absence locally; the server
// independently checks opt-in sync mode, ownership, path and exact revision.
func (s *Service) SyncDelete(ctx context.Context, watchID, workerID, p, fileID, digest string, version int) error {
	owner, err := principal(ctx, true)
	if err != nil {
		return err
	}
	if err = s.checkSync(ctx, workerID, p, watchID); err != nil {
		return err
	}
	watches, err := s.execute(ctx, "query", "libraryWatchedFolders", map[string]any{"workerId": workerID})
	if err != nil {
		return err
	}
	allowed := false
	for _, w := range watches {
		if memql.BareShortId(fmt.Sprint(w["id"])) == watchID && w["mode"] == "sync" {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("this folder has not been set to sync deletions")
	}
	unlock, err := memql.AcquireWriteGate(ctx, s.GateDB(), "library-file:"+memql.BareShortId(fileID))
	if err != nil {
		return err
	}
	defer unlock()
	current, err := s.Library.File(memql.ContextWithFreshRead(ctx), server.LibraryFileConceptRef(fileID))
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("the synced file is no longer available")
	}
	if memql.BareShortId(current.OwnerUserId) != owner || memql.BareShortId(current.BackupWorkerId) != workerID || current.BackupPath != p || current.Sha256 != digest || current.VersionNumber != version {
		return errors.New("the cloud copy changed; it was kept")
	}
	if current.Archived {
		return s.directory(ctx, Transfer{Owner: owner, WorkerID: workerID, Path: p, Kind: "sync_file", Metadata: Metadata{WatchID: watchID, FileID: fileID}}, "bin")
	}
	// Archive the derived index first, matching the ordinary Files Bin action.
	// A direct backing-file write also makes retries independent of event delay.
	rows, err := s.execute(ctx, "query", "libraryArtifactBySourceConceptRef", map[string]any{"sourceConceptRef": server.LibraryFileConceptRef(fileID)})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, err = s.execute(ctx, "mutation", "archiveArtifact", map[string]any{"artifactId": r["id"]}); err != nil {
			return err
		}
	}
	_, err = s.execute(ctx, "mutation", "archiveLibraryFile", map[string]any{"fileId": fileID})
	if err != nil {
		return err
	}
	return s.directory(ctx, Transfer{Owner: owner, WorkerID: workerID, Path: p, Kind: "sync_file", Metadata: Metadata{WatchID: watchID, FileID: fileID}}, "bin")
}
