// Package gcs provides Google Cloud Storage upload functionality.
package gcs

import (
	"context"
	"fmt"
	"os"
	"strings"

	"cloud.google.com/go/storage"
)

// Uploader uploads files to Google Cloud Storage.
type Uploader interface {
	Upload(ctx context.Context, bucket, objectName string, data []byte, contentType string) (gcsURL string, err error)
}

// GCSUploader is the real GCS implementation using the storage client library.
type GCSUploader struct {
	client *storage.Client
}

// New creates a new GCSUploader using Application Default Credentials.
// In Cloud Run the service account has GCS access automatically.
// Locally, set GOOGLE_APPLICATION_CREDENTIALS or run `gcloud auth application-default login`.
func New(ctx context.Context) (*GCSUploader, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create gcs client: %w", err)
	}
	return &GCSUploader{client: client}, nil
}

// Close releases the underlying GCS client connection.
func (u *GCSUploader) Close() error {
	if u == nil || u.client == nil {
		return nil
	}
	return u.client.Close()
}

// Upload stores data in the specified GCS bucket under objectName.
// Returns a gs:// URI for the stored object.
func (u *GCSUploader) Upload(ctx context.Context, bucket, objectName string, data []byte, contentType string) (string, error) {
	if u == nil || u.client == nil {
		return "", fmt.Errorf("gcs uploader not initialized")
	}
	bucket = strings.TrimSpace(bucket)
	objectName = strings.TrimSpace(objectName)
	if bucket == "" {
		return "", fmt.Errorf("gcs bucket name is required")
	}
	if objectName == "" {
		return "", fmt.Errorf("gcs object name is required")
	}

	obj := u.client.Bucket(bucket).Object(objectName)
	w := obj.NewWriter(ctx)

	if strings.TrimSpace(contentType) != "" {
		w.ContentType = contentType
	}

	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return "", fmt.Errorf("write to gcs: %w", err)
	}

	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close gcs writer: %w", err)
	}

	return fmt.Sprintf("gs://%s/%s", bucket, objectName), nil
}

// BucketFromEnv reads the GCS bucket name from MEMQL_GCS_BUCKET.
func BucketFromEnv() string {
	return strings.TrimSpace(os.Getenv("MEMQL_GCS_BUCKET"))
}
