package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
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

	nodeId := strings.TrimSpace(params.SpaceId) + ":" + uuid.New().String()

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

// jsonString returns a JSON-encoded string literal (including surrounding quotes).
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Sprintf(`"%s"`, strings.ReplaceAll(s, `"`, `\"`))
	}
	return string(b)
}
