package server

import "context"

// The byte-transport seams the HTTP handlers share.
//
// They were declared beside the space-attachment handler, which was the first
// route to move bytes. That handler went with the space concept (epic
// memql#4988) and these came here, because the Library's artifact routes --
// now the only upload path any client in this repo uses -- were already the
// heavier consumer of both.

// FileUploader persists bytes and returns a storage URI.
type FileUploader interface {
	Upload(ctx context.Context, bucket, objectName string, data []byte, contentType string) (blobUrl string, err error)
}

// FileDownloader reads bytes back from a storage URI.
type FileDownloader interface {
	DownloadURL(ctx context.Context, blobURL string) ([]byte, error)
}
