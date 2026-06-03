// Package azureblob provides Azure Blob Storage upload functionality.
// It replaces the retired GCS storage backend (memql#801): memQL is on
// Azure, so attachment + workbench/computer-use deliverable bytes live in
// an Azure Blob container.
package azureblob

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
)

// Uploader uploads files to blob storage. The signature is backend-agnostic
// (the same contract the GCS backend exposed) so the attachment handler,
// workbench, and worker integrations inject it without knowing the provider.
// `container` is the Azure Blob container name (the old "bucket" arg).
type Uploader interface {
	Upload(ctx context.Context, container, objectName string, data []byte, contentType string) (url string, err error)
}

// AzureBlobUploader is the real Azure implementation over the azblob SDK.
type AzureBlobUploader struct {
	client *azblob.Client
}

// New creates an AzureBlobUploader from the connection string in
// MEMQL_AZURE_STORAGE_CONNECTION_STRING. Returns an error when the
// connection string is absent or invalid; callers treat that as "storage
// not configured" and degrade to local/pointer behaviour.
func New(_ context.Context) (*AzureBlobUploader, error) {
	connStr := ConnectionStringFromEnv()
	if connStr == "" {
		return nil, fmt.Errorf("azure blob: MEMQL_AZURE_STORAGE_CONNECTION_STRING is not set")
	}
	client, err := azblob.NewClientFromConnectionString(connStr, nil)
	if err != nil {
		return nil, fmt.Errorf("create azure blob client: %w", err)
	}
	return &AzureBlobUploader{client: client}, nil
}

// Upload stores data in the given container under objectName and returns the
// blob's https URL (https://<account>.blob.core.windows.net/<container>/<object>).
func (u *AzureBlobUploader) Upload(ctx context.Context, container, objectName string, data []byte, contentType string) (string, error) {
	if u == nil || u.client == nil {
		return "", fmt.Errorf("azure blob uploader not initialized")
	}
	container = strings.TrimSpace(container)
	objectName = strings.TrimSpace(objectName)
	if container == "" {
		return "", fmt.Errorf("azure blob container name is required")
	}
	if objectName == "" {
		return "", fmt.Errorf("azure blob object name is required")
	}

	opts := &azblob.UploadBufferOptions{}
	if ct := strings.TrimSpace(contentType); ct != "" {
		opts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: &ct}
	}
	if _, err := u.client.UploadBuffer(ctx, container, objectName, data, opts); err != nil {
		return "", fmt.Errorf("upload to azure blob: %w", err)
	}

	// client.URL() is the account service URL (trailing slash); compose the
	// blob URL from it so we don't have to re-parse the connection string.
	base := strings.TrimSuffix(u.client.URL(), "/")
	return fmt.Sprintf("%s/%s/%s", base, container, objectName), nil
}

// ContainerFromEnv reads the blob container name from MEMQL_AZURE_BLOB_CONTAINER.
func ContainerFromEnv() string {
	return strings.TrimSpace(os.Getenv("MEMQL_AZURE_BLOB_CONTAINER"))
}

// ConnectionStringFromEnv reads the Azure Storage connection string from
// MEMQL_AZURE_STORAGE_CONNECTION_STRING.
func ConnectionStringFromEnv() string {
	return strings.TrimSpace(os.Getenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING"))
}
