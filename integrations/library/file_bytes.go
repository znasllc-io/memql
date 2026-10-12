package library

import (
	"context"
	"fmt"
	"io"

	"github.com/znasllc-io/memql/core/librarykey"
)

// openFileBytes opens a Library file's stored bytes, after holding the row's
// blobUrl to its owner's own library/{owner}/ prefix.
//
// Every document read here -- a saved version's preview, a passage check, a
// revision's source, a review attachment -- resolves its row under the
// caller's actor first, so row admission has already said the caller may see
// the row. It says nothing about which object the node's storage credential
// opens next: a row naming somebody else's key (one a client wrote before
// createLibraryFile was @serverOnly, or a version frozen from it) would hand
// over their bytes. So the key is judged before storage is asked. owner is the
// FILE's owner; a version of it is the same person's.
func (i *Integration) openFileBytes(ctx context.Context, owner, blobURL string) (io.ReadCloser, error) {
	if err := librarykey.Check(blobURL, owner, ""); err != nil {
		i.log().Error("library: a file row names a blob key outside its owner's library; refusing to read it",
			"error", err, "ownerUserId", owner)
		return nil, fmt.Errorf("the stored file is not in its owner's Library")
	}
	return i.blobFetcher.DownloadStreamURL(ctx, blobURL)
}
