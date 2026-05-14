package gcs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql"
)

// StorageIntegration wraps GCSUploader as an IntegrationProvider.
type StorageIntegration struct {
	uploader Uploader
	bucket   string
}

// NewStorageIntegration creates a storage integration with the given uploader and default bucket.
func NewStorageIntegration(uploader Uploader, bucket string) *StorageIntegration {
	return &StorageIntegration{
		uploader: uploader,
		bucket:   bucket,
	}
}

// IntegrationName returns the stable identifier.
func (s *StorageIntegration) IntegrationName() string {
	return "storage"
}

// Capabilities returns DSL-callable storage operations.
func (s *StorageIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "upload",
			Description: "Upload file data to cloud storage, returns the storage URL",
			Handler:     s.handleUpload,
			ArgsSchema: map[string]string{
				"bucket":      "string",
				"objectName":  "string",
				"data":        "string",
				"contentType": "string",
			},
		},
	}
}

func (s *StorageIntegration) handleUpload(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	bucket, _ := args["bucket"].(string)
	if bucket == "" {
		bucket = s.bucket
	}
	objectName, _ := args["objectName"].(string)
	contentType, _ := args["contentType"].(string)
	dataStr, _ := args["data"].(string)

	if bucket == "" || objectName == "" {
		return nil, fmt.Errorf("storage.upload requires bucket and objectName")
	}

	url, err := s.uploader.Upload(ctx, bucket, objectName, []byte(dataStr), contentType)
	if err != nil {
		return nil, fmt.Errorf("storage upload: %w", err)
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"url":         url,
		"bucket":      bucket,
		"objectName":  objectName,
		"contentType": contentType,
		"uploadedAt":  time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("storage-upload:%s/%s", bucket, objectName),
		Concept:   "integration:storage:upload",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}
