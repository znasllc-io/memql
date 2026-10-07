package pipelinesteps

import (
	"context"
	"io"
)

// ports.go -- what the runner needs from the rest of the engine, as
// interfaces: implemented in app/ on the workbench node (Task 8 of epic
// memql#5478), faked in tests. The runner imports neither the Library's
// concepts nor the GitHub App's credentials.

// RunFile is one file a step produced, for the owner's Library: the step's
// archived log, or one of its artifacts.
type RunFile struct {
	// OwnerUserID is the pipeline's owner: the file is theirs, and is
	// written under their authority.
	OwnerUserID string
	// WorkRunID and StepKey bind the file to the v1:work:run and the step
	// that produced it (producedByRunId, producedByStepKey).
	WorkRunID, StepKey string
	// Name is the file's name in the Library; MimeType its content type.
	Name, MimeType string
	Bytes          []byte
}

// StoredFile is the Library's answer for one RunFile.
type StoredFile struct {
	// FileID is the stored file's id; empty when nothing was stored.
	FileID string
	// Omitted says why nothing was stored -- the owner's quota, a file over
	// the Library's size limit, no storage on this cluster -- and is empty
	// when the file was stored. The runner reports it as a note beside the
	// step, never as the step's failure.
	Omitted string
	// Receipt pins the verified object behind a streamed artifact. FileID is
	// a mutable Library entry, not immutable publication evidence.
	Receipt *StoredFileReceipt
}

// StreamRunFile identifies one artifact of one attempt before any bytes move.
// Path is its canonical relative path in the producer's export. The caller
// owns Body (including closing it); recovery may not need to consume it.
type StreamRunFile struct {
	OwnerUserID, WorkRunID, StepKey string
	Attempt                         int
	Path, Name, MimeType            string
	Size                            int64
	SHA256                          string
	Body                            io.Reader
}

// StoredFileReceipt is persisted independently of the editable Library row.
// A consumer must verify the named object version and digest before using it.
type StoredFileReceipt struct {
	IntentID, FileID                string
	OwnerUserID, WorkRunID, StepKey string
	Attempt                         int
	Path                            string
	Container, Object, URL, ETag    string
	Size                            int64
	SHA256                          string
}

// StreamLibraryStore is deliberately separate from the small-file port: a
// runner must not silently buffer a large artifact for a legacy implementation.
type StreamLibraryStore interface {
	StoreRunFileStream(context.Context, StreamRunFile) (StoredFile, error)
}

// LibraryStore stores a step's files in its owner's Library.
type LibraryStore interface {
	StoreRunFile(ctx context.Context, f RunFile) (StoredFile, error)
}

// TokenMinter mints the clone token a step's clone container fetches with.
type TokenMinter interface {
	// CloneToken is a short-lived token that can read owner/name, minted
	// under the GitHub App installation installationID. Installation 0 is
	// an anonymous clone of a public repository, for which the answer is ""
	// and no error.
	CloneToken(ctx context.Context, installationID int64, owner, name string) (string, error)
}
