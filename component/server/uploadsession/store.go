// Package uploadsession is the graph store for chunked-upload session rows
// (memql#4782, design C2) -- and, deliberately, the SMALLEST possible home
// for an internal-origin stamp.
//
// The two session mutations are @serverOnly: blobPath is a storage path a
// caller must never author (a forged one would point the complete step at
// another user's bytes), and status's 'open' stamp is what stops a
// completed session being re-opened. Satisfying @serverOnly from a handler
// means stamping auth.ContextWithInternalOrigin on a REQUEST-DERIVED
// context, which is the shape component/auth/call_origin.go warns about --
// so the stamp lives here, in a package that contains nothing else, with
// the precondition asserted by store_internal_origin_test.go and the
// package named in the root call-origin allowlist:
//
//   - the stamped context is a local, derived per call, and no method
//     returns a context -- the stamp cannot outlive one write;
//   - the rendered writes never name ownerUserId or status -- the mutation
//     stamps both from the actor already on the caller's context;
//   - the READ is not stamped at all. uploadSessionById runs under the
//     caller's own actor, and row admission is the per-chunk owner check
//     the whole design leans on.
package uploadsession

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// Executor is the one engine capability this store needs. Declared here so
// the package imports nothing from component/server (which imports it).
type Executor interface {
	Execute(ctx context.Context, query string) (any, error)
}

// CreateParams is everything init records on the session row. There is
// deliberately no OwnerUserId and no Status: the mutation stamps both.
type CreateParams struct {
	UploadId               string
	Name                   string
	Size                   int64
	MimeType               string
	FolderId               string
	Labels                 []string
	UploadedFromWorkerId   string
	UploadedFromWorkerName string
	UploadedFromPath       string
	BlobPath               string
	FileId                 string
	ChunkSize              int64
	// TargetArtifactId is set when this session uploads a NEW VERSION of an
	// existing artifact (epic memql#4806): complete then supersedes rather
	// than creates, and FileId above is that artifact's EXISTING file id.
	// Blank is the ordinary fresh upload.
	TargetArtifactId string
}

// Row is the projection the chunk / inventory / complete handlers decide on.
type Row struct {
	ID                     string
	OwnerUserId            string
	Name                   string
	Size                   int64
	MimeType               string
	FolderId               string
	Labels                 []string
	UploadedFromWorkerId   string
	UploadedFromWorkerName string
	UploadedFromPath       string
	BlobPath               string
	FileId                 string
	ChunkSize              int64
	TargetArtifactId       string
	Status                 string
}

// Store runs the session constructs against the engine.
type Store struct {
	engine Executor
}

// NewStore creates a session store over an engine executor.
func NewStore(engine Executor) *Store {
	return &Store{engine: engine}
}

// Create opens a session row. The internal-origin stamp is applied to a
// LOCAL derived context and dies with this call; the actor on ctx -- the
// authenticated caller's own -- is what the mutation stamps ownerUserId
// from.
func (s *Store) Create(ctx context.Context, p CreateParams) error {
	args := map[string]any{
		"uploadId":  p.UploadId,
		"name":      p.Name,
		"size":      p.Size,
		"blobPath":  p.BlobPath,
		"fileId":    p.FileId,
		"chunkSize": p.ChunkSize,
	}
	if v := strings.TrimSpace(p.MimeType); v != "" {
		args["mimeType"] = v
	}
	if v := strings.TrimSpace(p.FolderId); v != "" {
		args["folderId"] = v
	}
	if len(p.Labels) > 0 {
		args["labels"] = p.Labels
	}
	if v := strings.TrimSpace(p.UploadedFromWorkerId); v != "" {
		args["uploadedFromWorkerId"] = v
	}
	if v := strings.TrimSpace(p.UploadedFromWorkerName); v != "" {
		args["uploadedFromWorkerName"] = v
	}
	if v := strings.TrimSpace(p.UploadedFromPath); v != "" {
		args["uploadedFromPath"] = v
	}
	if v := strings.TrimSpace(p.TargetArtifactId); v != "" {
		args["targetArtifactId"] = v
	}
	return s.executeServerOnly(ctx, "createUploadSession", args)
}

// Complete marks the session terminal. Same stamping shape as Create.
func (s *Store) Complete(ctx context.Context, uploadId string) error {
	uploadId = strings.TrimSpace(uploadId)
	if uploadId == "" {
		return fmt.Errorf("uploadId is required")
	}
	return s.executeServerOnly(ctx, "completeUploadSession", map[string]any{"uploadId": uploadId})
}

// ByID resolves a session UNDER THE CALLER'S ACTOR -- no stamp, on purpose:
// this read is the per-chunk authorization, and a session that is not the
// caller's must come back nil exactly as a missing one does.
func (s *Store) ByID(ctx context.Context, uploadId string) (*Row, error) {
	uploadId = strings.TrimSpace(uploadId)
	if uploadId == "" {
		return nil, fmt.Errorf("uploadId is required")
	}
	q, err := renderCall("uploadSessionById", map[string]any{"uploadId": uploadId})
	if err != nil {
		return nil, err
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("execute uploadSessionById: %w", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return &Row{
		ID:                     fieldString(r, "id"),
		OwnerUserId:            fieldString(r, "ownerUserId"),
		Name:                   fieldString(r, "name"),
		Size:                   fieldInt64(r, "size"),
		MimeType:               fieldString(r, "mimeType"),
		FolderId:               fieldString(r, "folderId"),
		Labels:                 fieldStrings(r, "labels"),
		UploadedFromWorkerId:   fieldString(r, "uploadedFromWorkerId"),
		UploadedFromWorkerName: fieldString(r, "uploadedFromWorkerName"),
		UploadedFromPath:       fieldString(r, "uploadedFromPath"),
		BlobPath:               fieldString(r, "blobPath"),
		FileId:                 fieldString(r, "fileId"),
		ChunkSize:              fieldInt64(r, "chunkSize"),
		TargetArtifactId:       fieldString(r, "targetArtifactId"),
		Status:                 fieldString(r, "status"),
	}, nil
}

// executeServerOnly renders and runs one @serverOnly write under a derived,
// call-local internal-origin context. The derived context is never stored
// and never returned; see the package comment for why that is load-bearing.
func (s *Store) executeServerOnly(ctx context.Context, fn string, args map[string]any) error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("engine not configured")
	}
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
// property component/server's dslCall guarantees, restated here because the
// two packages deliberately do not import each other.
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

func fieldString(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func fieldInt64(row map[string]any, key string) int64 {
	switch v := row[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	}
	return 0
}

func fieldStrings(row map[string]any, key string) []string {
	raw, ok := row[key].([]any)
	if !ok {
		if direct, ok := row[key].([]string); ok {
			return direct
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
