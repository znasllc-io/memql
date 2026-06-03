// Package azureblob provides Azure Blob Storage upload functionality.
// It replaces the retired GCS storage backend (memql#801): memQL is on
// Azure, so attachment + workbench/computer-use deliverable bytes live in
// an Azure Blob container.
package azureblob

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
)

// maxDownloadBytes caps a single blob read so an oversized object can't
// exhaust memory when streamed back through the attachment endpoint.
const maxDownloadBytes = 100 << 20 // 100 MiB

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

// Downloader reads blob bytes back from a stored blob URL. The attachment
// download handler injects this (it stays provider-agnostic by passing the
// stored URL string); AzureBlobUploader implements it (memql#804).
type Downloader interface {
	DownloadURL(ctx context.Context, blobURL string) ([]byte, error)
}

// DownloadURL parses (container, object) out of a stored Azure blob URL and
// fetches the bytes. Returns an error for a URL with no Azure object (the
// local:// dev placeholder / legacy gs://) so the caller can 404 it.
func (u *AzureBlobUploader) DownloadURL(ctx context.Context, blobURL string) ([]byte, error) {
	container, objectName, ok := ParseBlobObject(blobURL)
	if !ok {
		return nil, fmt.Errorf("not an azure blob URL (no downloadable object): %q", blobURL)
	}
	return u.Download(ctx, container, objectName)
}

// Download fetches the blob's bytes (capped at maxDownloadBytes). Used by the
// attachment download endpoint to stream a stored file back to the caller.
func (u *AzureBlobUploader) Download(ctx context.Context, container, objectName string) ([]byte, error) {
	if u == nil || u.client == nil {
		return nil, fmt.Errorf("azure blob uploader not initialized")
	}
	container = strings.TrimSpace(container)
	objectName = strings.TrimSpace(objectName)
	if container == "" || objectName == "" {
		return nil, fmt.Errorf("azure blob container and object name are required")
	}
	resp, err := u.client.DownloadStream(ctx, container, objectName, nil)
	if err != nil {
		return nil, fmt.Errorf("download from azure blob: %w", err)
	}
	body := resp.Body
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, maxDownloadBytes))
	if err != nil {
		return nil, fmt.Errorf("read azure blob stream: %w", err)
	}
	return data, nil
}

// ParseBlobObject extracts the (container, objectName) pair from a stored blob
// URL of the form https://<account>.blob.core.windows.net/<container>/<object...>.
// Returns ok=false for non-https URLs -- e.g. the `local://` dev placeholder or
// a legacy `gs://` value -- which have no Azure object to fetch.
func ParseBlobObject(blobURL string) (container, objectName string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(blobURL))
	if err != nil || u.Scheme != "https" {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/")
	idx := strings.Index(p, "/")
	if idx <= 0 || idx == len(p)-1 {
		return "", "", false
	}
	return p[:idx], p[idx+1:], true
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
