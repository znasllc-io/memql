package pipelinesteps

import "context"

// ports.go -- what this package needs from the node it runs on, implemented
// in app/ and faked in tests (epic memql#5478). The workbench's runner and
// the agent's fleet path share them: a step's files land in the same Library
// the same way, and its clone token is minted the same way, whichever surface
// ran it.

// RunFile is one file a step produced, on its way to the owner's Library: the
// archived log, or one artifact.
type RunFile struct {
	OwnerUserID, WorkRunID, StepKey string
	Name, MimeType                  string
	Bytes                           []byte
}

// StoredFile is what the Library did with a RunFile.
type StoredFile struct {
	FileID string
	// Omitted says why nothing was stored ("" when stored): the owner is
	// over their quota, the file is over the upload cap, no storage is
	// configured. It is a note beside the step, never a failure of it.
	Omitted string
}

// LibraryStore files a step's output in its owner's Library, bound to the
// work run and step that produced it.
type LibraryStore interface {
	StoreRunFile(ctx context.Context, f RunFile) (StoredFile, error)
}

// TokenMinter mints the short-lived token a step clones its repository with.
type TokenMinter interface {
	// CloneToken is a token that can read owner/name and nothing else.
	// installationID 0 is an anonymous clone of a public repository, for
	// which no token is minted.
	CloneToken(ctx context.Context, installationID int64, owner, name string) (string, error)
}
