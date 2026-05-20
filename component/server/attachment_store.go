package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

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

	nodeId := strings.TrimSpace(params.SpaceId) + ":" + id.NewShortId()

	var sb strings.Builder
	sb.WriteString(`mutationCreateAttachment({"attachmentId": `)
	sb.WriteString(jsonString(nodeId))
	sb.WriteString(`, "spaceId": `)
	sb.WriteString(jsonString(params.SpaceId))
	sb.WriteString(`, "fileName": `)
	sb.WriteString(jsonString(params.FileName))
	sb.WriteString(`, "mimeType": `)
	sb.WriteString(jsonString(params.MimeType))
	sb.WriteString(fmt.Sprintf(`, "fileSize": %d`, params.FileSize))
	sb.WriteString(`, "gcsURL": `)
	sb.WriteString(jsonString(params.GCSUrl))
	sb.WriteString(`, "status": `)
	sb.WriteString(jsonString(params.Status))
	sb.WriteString(`, "uploadedBy": `)
	sb.WriteString(jsonString(params.UploadedBy))
	if t := strings.TrimSpace(params.Transcription); t != "" {
		sb.WriteString(`, "transcription": `)
		sb.WriteString(jsonString(t))
	}
	if sm := strings.TrimSpace(params.Summary); sm != "" {
		sb.WriteString(`, "summary": `)
		sb.WriteString(jsonString(sm))
	}
	sb.WriteString(`})`)

	result, err := s.engine.Execute(ctx, sb.String())
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
func (s *EngineAttachmentStore) CallerOwnsSpace(ctx context.Context, spaceId string) (bool, error) {
	if s == nil || s.engine == nil {
		return false, fmt.Errorf("engine not configured")
	}
	spaceId = strings.TrimSpace(spaceId)
	if spaceId == "" {
		return false, fmt.Errorf("spaceId is required")
	}

	q := fmt.Sprintf(`queryOwnedSpaceById({"spaceId": %s})`, jsonString(spaceId))
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return false, fmt.Errorf("execute queryOwnedSpaceById: %w", err)
	}
	return queryResultHasRow(res), nil
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

// jsonString returns a JSON-encoded string literal (including surrounding quotes).
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Sprintf(`"%s"`, strings.ReplaceAll(s, `"`, `\"`))
	}
	return string(b)
}
