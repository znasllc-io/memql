package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// MemQLExecutor runs raw MemQL queries.
type MemQLExecutor interface {
	Execute(ctx context.Context, query string) (any, error)
}

// EngineAttachmentStore writes attachment rows by calling the DSL mutation
// `mutationCreateAttachment`. The mutation owns the concept choice; this
// store is concept-agnostic and simply plumbs the upload metadata
// through. Product DSL (mutations/v1/copresent/, etc.) defines the
// mutation and the concept it targets.
type EngineAttachmentStore struct {
	engine MemQLExecutor
}

// NewEngineAttachmentStore creates an AttachmentStore backed by a MemQL engine.
func NewEngineAttachmentStore(engine MemQLExecutor) *EngineAttachmentStore {
	return &EngineAttachmentStore{engine: engine}
}

// CreateAttachment invokes the DSL mutation `mutationCreateAttachment` with
// the upload metadata and returns the mutation's shaped result.
func (s *EngineAttachmentStore) CreateAttachment(ctx context.Context, params AttachmentCreateParams) (json.RawMessage, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}

	nodeId := strings.TrimSpace(params.PartitionId) + ":" + id.NewShortId()

	args := map[string]any{
		"attachmentId": nodeId,
		"partitionId":  params.PartitionId,
		"fileName":     params.FileName,
		"mimeType":     params.MimeType,
		"fileSize":     params.FileSize,
		"blobUrl":      params.BlobUrl,
		"status":       params.Status,
		"uploadedBy":   params.UploadedBy,
	}
	if t := strings.TrimSpace(params.Transcription); t != "" {
		args["transcription"] = t
	}
	if sm := strings.TrimSpace(params.Summary); sm != "" {
		args["summary"] = sm
	}
	q, err := dslCall("mutationCreateAttachment", args)
	if err != nil {
		return nil, err
	}

	result, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("execute mutationCreateAttachment: %w", err)
	}

	if result == nil {
		return json.Marshal(map[string]any{"id": nodeId})
	}

	b, err := json.Marshal(result)
	if err != nil {
		return json.Marshal(map[string]any{"id": nodeId})
	}
	return b, nil
}

// CallerOwnsSpace runs queryOwnedSpaceById against the engine. The DSL
// filter pins payload.ownerUserId==actor.userId; the engine envelope's
// actor is whoever the caller authenticated as (resolved by the gRPC
// stream interceptor or HTTP auth middleware that wrapped this
// request). If the result set is non-empty, the caller owns the space.
func (s *EngineAttachmentStore) CallerOwnsSpace(ctx context.Context, partitionId string) (bool, error) {
	if s == nil || s.engine == nil {
		return false, fmt.Errorf("engine not configured")
	}
	partitionId = strings.TrimSpace(partitionId)
	if partitionId == "" {
		return false, fmt.Errorf("partitionId is required")
	}

	q, err := dslCall("queryOwnedSpaceById", map[string]any{"partitionId": partitionId})
	if err != nil {
		return false, err
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return false, fmt.Errorf("execute queryOwnedSpaceById: %w", err)
	}
	return queryResultHasRow(res), nil
}

// GetAttachment reads one v1:common:attachment row by id within a space via
// the attachmentById DSL query. The query's filter pins
// payload.partitionId==args.partitionId, so an attachment id from a different space
// returns no row. Returns nil (no error) when not found. (memql#804)
func (s *EngineAttachmentStore) GetAttachment(ctx context.Context, attachmentId, partitionId string) (*AttachmentRow, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	attachmentId = strings.TrimSpace(attachmentId)
	partitionId = strings.TrimSpace(partitionId)
	if attachmentId == "" || partitionId == "" {
		return nil, fmt.Errorf("attachmentId and partitionId are required")
	}

	q, err := dslCall("attachmentById", map[string]any{
		"attachmentId": attachmentId,
		"partitionId":  partitionId,
	})
	if err != nil {
		return nil, err
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("execute attachmentById: %w", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	getStr := func(k string) string {
		if v, ok := r[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	return &AttachmentRow{
		ID:          getStr("id"),
		FileName:    getStr("fileName"),
		MimeType:    getStr("mimeType"),
		BlobUrl:     getStr("blobUrl"),
		PartitionId: getStr("partitionId"),
		Status:      getStr("status"),
	}, nil
}

// queryResultHasRow returns true if the engine result contains at
// least one row. The result type is opaque to this package (different
// engine versions return Bundle, []*MemoryNode, or shape-wrapped
// []*structpb.Value depending on the call path); use reflection to
// check non-emptiness without taking on a typed import.
func queryResultHasRow(v any) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		return rv.Len() > 0
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return false
		}
		return queryResultHasRow(rv.Elem().Interface())
	case reflect.Struct:
		// Try the common "Bundle has Nodes slice" shape via reflection.
		if f := rv.FieldByName("Bundle"); f.IsValid() && f.Kind() == reflect.Ptr && !f.IsNil() {
			if nodes := f.Elem().FieldByName("Nodes"); nodes.IsValid() && (nodes.Kind() == reflect.Slice || nodes.Kind() == reflect.Array) {
				return nodes.Len() > 0
			}
		}
		// Try Data slice shape (shape-wrapped result).
		if f := rv.FieldByName("Data"); f.IsValid() && (f.Kind() == reflect.Slice || f.Kind() == reflect.Array) {
			return f.Len() > 0
		}
		// Fallback: a non-zero struct counts as "something". Conservative.
		return !rv.IsZero()
	}
	return false
}

// dslCall renders a MemQL function call `fn(k: v, ...)` in the named-args
// invocation form (Story 9 / #2335: NOT the legacy object-literal wrapper
// `fn({...})`, which the parser now rejects). Each value is JSON-encoded so a
// value containing a double quote can never break out of its enclosing literal
// (CodeQL go/unsafe-quoting); keys sorted for a deterministic call string.
func dslCall(fn string, args map[string]any) (string, error) {
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
