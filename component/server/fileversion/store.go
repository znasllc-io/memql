// Package fileversion is the graph writer for a Library file's version chain
// (epic memql#4806, design D10) -- and, deliberately, the SMALLEST possible
// home for a second internal-origin stamp.
//
// The two version mutations are @serverOnly, and blobUrl is why: it is a
// storage path, composed server-side from the verified actor and the
// engine-minted fileId, and a forged one would name another user's object
// which GET /artifacts/{id}/content?version={n} would then stream back.
// Satisfying @serverOnly from an HTTP handler means stamping
// auth.ContextWithInternalOrigin on a REQUEST-DERIVED context, which is the
// shape component/auth/call_origin.go warns about -- so the stamp lives here,
// in a package that contains nothing else, with the preconditions asserted by
// store_internal_origin_test.go and the package named in the root
// call-origin allowlist. Same arrangement, same three properties, as its
// sibling component/server/uploadsession:
//
//   - the stamped context is a local, derived per call, and no method
//     returns a context -- the stamp cannot outlive one write;
//   - the rendered writes never name ownerUserId -- the mutation stamps it
//     from the actor already on the caller's context, which is the caller's
//     own;
//   - there are NO READS here at all. Every version read (the history list,
//     the ?version={n} resolve, the quota sum) runs unstamped through
//     component/server's own store, under the caller's actor, so row
//     admission is the whole authorization.
//
// SUPERSEDE IS TWO WRITES AND THE ORDER IS THE CONTRACT (design D3), which
// is why Supersede takes both halves and the handler cannot call them
// separately: the snapshot lands first, then the head moves.
package fileversion

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// Executor is the one engine capability this store needs. Declared here so
// the package imports nothing from component/server (which imports it).
type Executor interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Snapshot freezes the OUTGOING head as an immutable version row. There is
// deliberately no OwnerUserId: the mutation stamps it from the actor on the
// caller's context, and the target file was already resolved under that same
// actor before a byte moved.
type Snapshot struct {
	// VersionId is the derived id '<fileId>-v<n>' -- see DerivedVersionId.
	VersionId string
	FileId    string
	// VersionNumber is the OUTGOING head's number, the one this row freezes.
	VersionNumber int
	Name          string
	MimeType      string
	Size          int64
	// Sha256 may be blank: a chunked upload's head can be superseded before
	// the analysis pass has streamed the blob. Absent means "not measured",
	// never "no hash exists".
	Sha256                 string
	BlobUrl                string
	Format                 string
	Summary                string
	UploadedFromWorkerId   string
	UploadedFromWorkerName string
	UploadedFromPath       string
	// UploadedAt is when THESE bytes arrived, copied from the head's
	// versionUploadedAt -- not the moment of this write, which is the
	// supersede moment and is stamped separately.
	UploadedAt string
}

// Head is the new bytes the file row moves onto.
//
// Every provenance field is sent even when blank, and that is the point: a
// version pushed from a laptop names the laptop, and the browser upload that
// replaces it names nothing. The mutation writes the blanks explicitly, so a
// read-merge cannot leave the laptop's name on bytes that came from a browser.
type Head struct {
	FileId                 string
	VersionNumber          int
	Name                   string
	MimeType               string
	Size                   int64
	Sha256                 string
	BlobUrl                string
	Format                 string
	UploadedFromWorkerId   string
	UploadedFromWorkerName string
	UploadedFromPath       string
}

// Store runs the version constructs against the engine.
type Store struct {
	engine Executor
}

// NewStore creates a version store over an engine executor.
func NewStore(engine Executor) *Store {
	return &Store{engine: engine}
}

// DerivedVersionId is the id a snapshot of version n of a file lands at.
//
// Derived rather than minted so a RETRIED supersede re-versions the same row
// instead of appending a duplicate to somebody's history (design D2/D3). It
// is derived in Go rather than in the DSL because the key is a pair with an
// int in it and MemQL's concat takes strings -- and because createLibraryFile
// already establishes the precedent of an engine-minted id passed as an
// argument.
func DerivedVersionId(fileId string, versionNumber int) string {
	return fmt.Sprintf("%s-v%d", strings.TrimSpace(fileId), versionNumber)
}

// Supersede freezes the outgoing head and then moves it, in that order.
//
// ONE METHOD, because the order is a design decision rather than a caller's
// choice (design D3). A crash between the two writes leaves a version row
// whose facts equal the head's -- the reader's fold keys on versionNumber
// and the head wins, so one version is shown, not two -- and the retry is
// idempotent, because the snapshot's id derives from (fileId, versionNumber).
// The other order loses a version's ROW while its bytes stay in storage:
// history skips a number and nothing anywhere says so.
//
// The head move is NOT attempted when the snapshot fails, for the same
// reason: a moved head with no snapshot behind it is the failure this order
// exists to prevent.
func (s *Store) Supersede(ctx context.Context, snap Snapshot, head Head) error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	if strings.TrimSpace(snap.FileId) == "" || strings.TrimSpace(head.FileId) == "" {
		return fmt.Errorf("fileId is required on both halves of a supersede")
	}
	if snap.FileId != head.FileId {
		return fmt.Errorf("a supersede's snapshot and head must name the same file (%q vs %q)",
			snap.FileId, head.FileId)
	}
	if head.VersionNumber <= snap.VersionNumber {
		return fmt.Errorf("a supersede must advance the version number (snapshot v%d, head v%d)",
			snap.VersionNumber, head.VersionNumber)
	}
	if err := s.executeServerOnly(ctx, "createLibraryFileVersion", snapshotArgs(snap)); err != nil {
		return err
	}
	return s.executeServerOnly(ctx, "supersedeLibraryFileHead", headArgs(head))
}

func snapshotArgs(s Snapshot) map[string]any {
	args := map[string]any{
		"versionId":     s.VersionId,
		"fileId":        s.FileId,
		"versionNumber": s.VersionNumber,
		"name":          s.Name,
		"mimeType":      s.MimeType,
		"size":          s.Size,
		"blobUrl":       s.BlobUrl,
		"uploadedAt":    s.UploadedAt,
	}
	// The optional half is omitted when blank rather than sent empty: this
	// is an INSERT of a frozen row, so an absent field and a blank one mean
	// the same thing, and an empty PRESENT enum argument is refused by the
	// mutation's own validation.
	for k, v := range map[string]string{
		"sha256":                 s.Sha256,
		"format":                 s.Format,
		"summary":                s.Summary,
		"uploadedFromWorkerId":   s.UploadedFromWorkerId,
		"uploadedFromWorkerName": s.UploadedFromWorkerName,
		"uploadedFromPath":       s.UploadedFromPath,
	} {
		if strings.TrimSpace(v) != "" {
			args[k] = v
		}
	}
	return args
}

func headArgs(h Head) map[string]any {
	args := map[string]any{
		"fileId":        h.FileId,
		"versionNumber": h.VersionNumber,
		"name":          h.Name,
		"mimeType":      h.MimeType,
		"size":          h.Size,
		"blobUrl":       h.BlobUrl,
	}
	// sha256 and the three provenance fields ARE sent when blank, unlike the
	// snapshot's. This is an UPDATE, and update{} is a read-merge: an omitted
	// argument leaves the PREVIOUS version's value in place, which for sha256
	// is a hash describing bytes that are gone and for uploadedFrom* is a
	// machine that did not send these bytes. The mutation coalesces each to
	// "" so the blank is written rather than inherited.
	args["sha256"] = strings.TrimSpace(h.Sha256)
	args["uploadedFromWorkerId"] = strings.TrimSpace(h.UploadedFromWorkerId)
	args["uploadedFromWorkerName"] = strings.TrimSpace(h.UploadedFromWorkerName)
	args["uploadedFromPath"] = strings.TrimSpace(h.UploadedFromPath)
	if v := strings.TrimSpace(h.Format); v != "" {
		args["format"] = v
	}
	return args
}

// executeServerOnly renders and runs one @serverOnly write under a derived,
// call-local internal-origin context. The derived context is never stored
// and never returned; see the package comment for why that is load-bearing.
func (s *Store) executeServerOnly(ctx context.Context, fn string, args map[string]any) error {
	q, err := renderCall(fn, args)
	if err != nil {
		return err
	}
	stamped := auth.ContextWithInternalOrigin(ctx)
	if _, err := s.engine.Execute(stamped, q); err != nil {
		return fmt.Errorf("execute %s: %w", fn, err)
	}
	return nil
}

// renderCall renders `fn(k: v, ...)` with every value JSON-encoded, so a
// value containing a quote can never break out of its literal -- the same
// property component/server's dslCall guarantees, restated here because
// these packages deliberately do not import each other.
func renderCall(fn string, args map[string]any) (string, error) {
	if len(args) == 0 {
		return fn + "()", nil
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(fn)
	b.WriteByte('(')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		v, err := json.Marshal(args[k])
		if err != nil {
			return "", fmt.Errorf("marshal %s arg %q: %w", fn, k, err)
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.Write(v)
	}
	b.WriteByte(')')
	return b.String(), nil
}
