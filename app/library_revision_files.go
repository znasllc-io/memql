package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/fileversion"
	"github.com/znasllc-io/memql/integrations/library"
)

// Storage adapter only: identity, quota, immutable upload and version CAS. The
// Library operation has already checked the exact approved proposal. This
// adapter never chooses what content to generate or when to request approval.
type libraryRevisionFileStore interface {
	File(context.Context, string) (*server.LibraryFileRow, error)
	FileVersion(context.Context, string, int) (*server.LibraryFileVersionRow, error)
	StorageFootprint(context.Context) (int64, int64, int64, error)
	SupersedeFile(context.Context, server.LibraryVersionSnapshot, server.LibraryHeadMove) error
	RestampFileArtifact(context.Context, string) error
}

type libraryRevisionFiles struct {
	store    libraryRevisionFileStore
	uploader server.FileUploader
	bucket   string
}

func (s *libraryRevisionFiles) SaveRevision(ctx context.Context, write library.RevisionFileWrite) error {
	ac, ok := auth.AccessFromContext(ctx)
	subject, hasSubject := auth.SubjectFromContext(ctx)
	if !ok || ac == nil || ac.UserId == "" || !hasSubject || !auth.CapableFor(ctx, subject, auth.VerbCreate, auth.ResourceData) {
		return fmt.Errorf("you do not have permission to save this revision")
	}
	if s.uploader == nil || s.bucket == "" {
		return fmt.Errorf("document storage is unavailable on this node")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	current, err := s.store.File(ctx, server.LibraryFileConceptRef(write.FileID))
	if err != nil {
		return err
	}
	if current == nil || current.Archived {
		return fmt.Errorf("the document is unavailable")
	}
	digestBytes := sha256.Sum256(write.Content)
	digest := hex.EncodeToString(digestBytes[:])
	// Both the request and bytes address the object. Repeating an uncertain
	// upload writes identical bytes to the same object, never another version.
	operationHash := sha256.Sum256([]byte(write.OperationID))
	suffix := fmt.Sprintf("/revisions/%x/%s/document.md", operationHash, digest)
	objectName := fmt.Sprintf("library/%s/%s%s", memql.BareShortId(ac.UserId), memql.BareShortId(write.FileID), suffix)
	reconciled := func() (bool, error) {
		head, err := s.store.File(ctx, server.LibraryFileConceptRef(write.FileID))
		if err != nil || head == nil {
			return false, err
		}
		if head.VersionNumber == write.Version+1 && head.Sha256 == digest && strings.HasSuffix(head.BlobUrl, suffix) {
			return true, nil
		}
		if head.VersionNumber > write.Version+1 {
			version, err := s.store.FileVersion(ctx, write.FileID, write.Version+1)
			if err != nil {
				return false, err
			}
			return version != nil && version.Sha256 == digest && strings.HasSuffix(version.BlobUrl, suffix), nil
		}
		return false, nil
	}
	if done, err := reconciled(); err != nil {
		return err
	} else if done {
		return s.store.RestampFileArtifact(ctx, write.FileID)
	}
	if current.VersionNumber != write.Version || current.BlobUrl != write.BlobURL {
		return fmt.Errorf("the document changed before these edits could be applied; review the current revision")
	}
	size := int64(len(write.Content))
	if size > server.LibraryMaxUploadBytes() {
		return fmt.Errorf("the revised document exceeds the upload limit")
	}
	files, versions, sessions, err := s.store.StorageFootprint(ctx)
	if err != nil {
		return err
	}
	if files+versions+sessions+size > server.LibraryUserQuotaBytes() {
		return fmt.Errorf("the revised document would exceed your Library storage quota")
	}
	blobURL, err := s.uploader.Upload(ctx, s.bucket, objectName, write.Content, current.MimeType)
	if err != nil {
		return fmt.Errorf("could not store the approved revision: %w", err)
	}
	if blobURL == "" {
		return fmt.Errorf("storage did not confirm the approved revision")
	}
	err = s.store.SupersedeFile(ctx, server.LibraryVersionSnapshot{
		VersionId: fileversion.DerivedVersionId(write.FileID, current.VersionNumber), FileId: write.FileID, VersionNumber: current.VersionNumber,
		Name: current.Name, MimeType: current.MimeType, Size: int64(current.Size), Sha256: current.Sha256, BlobUrl: current.BlobUrl, Format: current.Format, Summary: current.Summary,
		UploadedFromWorkerId: current.UploadedFromWorkerId, UploadedFromWorkerName: current.UploadedFromWorkerName, UploadedFromPath: current.UploadedFromPath, UploadedAt: current.VersionUploadedAt,
	}, server.LibraryHeadMove{FileId: write.FileID, VersionNumber: write.Version + 1, Name: current.Name, MimeType: current.MimeType, Size: size, Sha256: digest, BlobUrl: blobURL, Format: current.Format})
	if err != nil {
		if done, readErr := reconciled(); readErr != nil || !done {
			return fmt.Errorf("could not commit the approved revision: %w", err)
		}
	}
	return s.store.RestampFileArtifact(ctx, write.FileID)
}
