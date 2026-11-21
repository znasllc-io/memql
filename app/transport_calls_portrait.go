package app

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/server"
)

type callsPortraitBlobReader interface {
	DownloadURLWithLimit(context.Context, string, int64) ([]byte, error)
}
type callsPortraitBlobStore struct {
	uploader server.FileUploader
	reader   callsPortraitBlobReader
	bucket   string
}

func newCallsPortraitBlobStore(uploader server.FileUploader, bucket string) *callsPortraitBlobStore {
	reader, ok := uploader.(callsPortraitBlobReader)
	if uploader == nil || bucket == "" || !ok {
		return nil
	}
	return &callsPortraitBlobStore{uploader: uploader, reader: reader, bucket: bucket}
}
func (b *callsPortraitBlobStore) Put(ctx context.Context, owner, object string, data []byte, contentType string) (string, error) {
	return b.uploader.Upload(auth.ContextWithUserActor(ctx, owner), b.bucket, object, data, contentType)
}
func (b *callsPortraitBlobStore) Get(ctx context.Context, owner, ref string, maxBytes int64) ([]byte, error) {
	return b.reader.DownloadURLWithLimit(auth.ContextWithUserActor(ctx, owner), ref, maxBytes)
}
