package fileprocessor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	fp "github.com/visionarys-io/memql/component/fileprocessor"
	"github.com/visionarys-io/memql/component/memql"
)

// FilesIntegration wraps the file processor as an IntegrationProvider.
type FilesIntegration struct {
	processor fp.Processor
}

// NewFilesIntegration creates a files integration with the given processor.
func NewFilesIntegration(processor fp.Processor) *FilesIntegration {
	return &FilesIntegration{processor: processor}
}

// IntegrationName returns the stable identifier.
func (f *FilesIntegration) IntegrationName() string {
	return "files"
}

// Capabilities returns DSL-callable file operations.
func (f *FilesIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "extractText",
			Description: "Extract plain text from a file (PDF, DOCX, images, text). Returns extracted content.",
			Handler:     f.handleExtractText,
			ArgsSchema: map[string]string{
				"mimeType": "string",
				"data":     "string",
			},
		},
	}
}

func (f *FilesIntegration) handleExtractText(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	mimeType, _ := args["mimeType"].(string)
	dataStr, _ := args["data"].(string)

	if mimeType == "" || dataStr == "" {
		return nil, fmt.Errorf("files.extractText requires mimeType and data")
	}

	text, err := f.processor.Extract(ctx, mimeType, []byte(dataStr))
	if err != nil {
		return nil, fmt.Errorf("extract text: %w", err)
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"text":        text,
		"mimeType":    mimeType,
		"extractedAt": time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("files-extract:%d", time.Now().UnixNano()),
		Concept:   "integration:files:extraction",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}
