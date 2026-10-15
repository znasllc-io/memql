package azureblob

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"path"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// StorageIntegration wraps an Azure Blob Uploader as an IntegrationProvider,
// exposing the DSL-callable `integration.storage.upload` capability. The
// integration name stays "storage" so the DSL surface is unchanged across the
// GCS->Azure migration (memql#801).
type StorageIntegration struct {
	uploader  Uploader
	container string
}

// NewStorageIntegration creates a storage integration with the given uploader
// and default container.
func NewStorageIntegration(uploader Uploader, container string) *StorageIntegration {
	return &StorageIntegration{uploader: uploader, container: container}
}

// IntegrationName returns the stable identifier.
func (s *StorageIntegration) IntegrationName() string { return "storage" }

// Capabilities returns DSL-callable storage operations.
func (s *StorageIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "upload",
			Description: "Upload file data to Azure Blob Storage, returns the blob URL",
			Handler:     s.handleUpload,
			ArgsSchema: map[string]string{
				"container":   "string (optional -- defaults to the configured container)",
				"objectName":  "string",
				"data":        "string",
				"contentType": "string",
			},
		},
	}
}

func (s *StorageIntegration) handleUpload(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	// Accept either "container" or the legacy "bucket" arg name for callers
	// that haven't been swept since the GCS->Azure migration.
	container, _ := args["container"].(string)
	if container == "" {
		container, _ = args["bucket"].(string)
	}
	if container == "" {
		container = s.container
	}
	objectName, _ := args["objectName"].(string)
	contentType, _ := args["contentType"].(string)
	dataStr, _ := args["data"].(string)

	if container == "" || objectName == "" {
		return nil, fmt.Errorf("storage.upload requires container and objectName")
	}

	// Public storage calls cannot choose a library/backup object or charge the
	// cluster identity. Native producers use the uploader with their own authority.
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.IsAnonymous || ac.UserId == "" {
		return nil, fmt.Errorf("storage.upload requires an authenticated actor")
	}
	if !ac.Synthetic {
		subject, ok := auth.SubjectFromContext(ctx)
		if !ok || !auth.CapableFor(ctx, subject, auth.VerbCreate, auth.ResourceData) {
			return nil, fmt.Errorf("storage.upload requires permission to write files")
		}
		if container != s.container || strings.HasPrefix(objectName, "/") || path.Clean(objectName) != objectName || strings.ContainsAny(objectName, "\\\x00\r\n") {
			return nil, fmt.Errorf("invalid storage upload destination")
		}
		objectName = "objects/" + memql.BareShortId(ac.UserId) + "/" + objectName
	}
	url, err := s.uploader.Upload(ctx, container, objectName, []byte(dataStr), contentType)
	if err != nil {
		return nil, fmt.Errorf("storage upload: %w", err)
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"url":         url,
		"container":   container,
		"objectName":  objectName,
		"contentType": contentType,
		"uploadedAt":  time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("storage-upload:%s/%s", container, objectName),
		Concept:   "integration:storage:upload",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}
