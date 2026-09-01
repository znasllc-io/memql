package azureblob

// blocks.go -- staged-block upload and streaming download (memql#4782,
// design C3/C5/C6).
//
// The chunked upload path stages 16 MiB blocks against the target blob and
// commits the list once every block is present. Two properties of Azure
// block blobs are load-bearing for the whole design and worth naming here:
//
//   - STAGED BLOCKS LIVE WITH THE BLOB, not with any client or server
//     process. Any bff replica can stage block n, inventory the staged set,
//     or commit -- which is what makes the session replica-agnostic by
//     construction rather than by careful state sharing.
//   - UNCOMMITTED BLOCKS GARBAGE-COLLECT SERVER-SIDE on Azure's ~7-day
//     clock. An abandoned upload needs no sweeper anywhere in MemQL; the
//     staging simply expires.
//
// Azurite supports all of it locally.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
)

// blockClient resolves the block-blob client for one object.
func (u *AzureBlobUploader) blockClient(container, objectName string) (*blockblob.Client, error) {
	if u == nil || u.client == nil {
		return nil, fmt.Errorf("azure blob uploader not initialized")
	}
	container = strings.TrimSpace(container)
	objectName = strings.TrimSpace(objectName)
	if container == "" || objectName == "" {
		return nil, fmt.Errorf("azure blob container and object name are required")
	}
	return u.client.ServiceClient().NewContainerClient(container).NewBlockBlobClient(objectName), nil
}

// StageBlock uploads one block against the object under the given base64
// block id. Restageable: staging the same id again REPLACES the staged
// block, which is what makes a chunk retry safe with no coordination. An
// empty chunk is refused here as well as at the route -- a zero-byte block
// has no meaning in a size-verified commit.
func (u *AzureBlobUploader) StageBlock(ctx context.Context, container, objectName, blockID string, chunk []byte) error {
	bc, err := u.blockClient(container, objectName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(blockID) == "" {
		return fmt.Errorf("azure blob block id is required")
	}
	if len(chunk) == 0 {
		return fmt.Errorf("azure blob block body is empty")
	}
	if _, err := bc.StageBlock(ctx, blockID, streaming.NopCloser(bytes.NewReader(chunk)), nil); err != nil {
		return fmt.Errorf("stage azure blob block: %w", err)
	}
	return nil
}

// CommitBlockList commits the given base64 block ids, in the given order,
// as the object's content. Until this call the object either does not
// exist or keeps its previous content; after it, the staged set is the
// blob. The committed order is the caller's -- for the upload sessions
// that is ascending chunk index, verified before commit.
func (u *AzureBlobUploader) CommitBlockList(ctx context.Context, container, objectName string, blockIDs []string, contentType string) error {
	bc, err := u.blockClient(container, objectName)
	if err != nil {
		return err
	}
	if len(blockIDs) == 0 {
		return fmt.Errorf("azure blob commit needs at least one block id")
	}
	opts := &blockblob.CommitBlockListOptions{}
	if ct := strings.TrimSpace(contentType); ct != "" {
		opts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: &ct}
	}
	if _, err := bc.CommitBlockList(ctx, blockIDs, opts); err != nil {
		return fmt.Errorf("commit azure blob block list: %w", err)
	}
	return nil
}

// UncommittedBlocks inventories the staged-but-uncommitted blocks for the
// object: base64 block id -> size in bytes. THE RESUME READ (design C1):
// a client that died mid-upload asks this, uploads only what is missing,
// and completes. An object with no staged blocks answers an empty map --
// including a brand-new object Azure has never heard of, because "nothing
// is staged" is the honest answer for that too.
func (u *AzureBlobUploader) UncommittedBlocks(ctx context.Context, container, objectName string) (map[string]int64, error) {
	bc, err := u.blockClient(container, objectName)
	if err != nil {
		return nil, err
	}
	resp, err := bc.GetBlockList(ctx, blockblob.BlockListTypeUncommitted, nil)
	if err != nil {
		if isBlobNotFound(err) {
			return map[string]int64{}, nil
		}
		return nil, fmt.Errorf("list azure blob uncommitted blocks: %w", err)
	}
	out := make(map[string]int64, len(resp.UncommittedBlocks))
	for _, b := range resp.UncommittedBlocks {
		if b == nil || b.Name == nil {
			continue
		}
		var size int64
		if b.Size != nil {
			size = *b.Size
		}
		out[*b.Name] = size
	}
	return out, nil
}

// isBlobNotFound reports the "no such blob / container" family, which the
// inventory treats as "nothing staged" rather than an error.
func isBlobNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "BlobNotFound") || strings.Contains(msg, "ContainerNotFound") ||
		strings.Contains(msg, "404")
}

// DownloadStreamURL opens the blob behind a stored URL as a stream. The
// caller owns the ReadCloser. Unlike DownloadURL there is NO size cap: the
// export route streams through io.Copy with constant memory, and the
// Content-Length it advertises comes from the file row, not from buffering.
func (u *AzureBlobUploader) DownloadStreamURL(ctx context.Context, blobURL string) (io.ReadCloser, error) {
	container, objectName, ok := u.splitStoredURL(blobURL)
	if !ok {
		return nil, fmt.Errorf("not an azure blob URL (no downloadable object): %q", blobURL)
	}
	resp, err := u.client.DownloadStream(ctx, container, objectName, nil)
	if err != nil {
		return nil, fmt.Errorf("open azure blob stream: %w", err)
	}
	return resp.Body, nil
}

// DownloadRangeURL opens a single byte range of the blob behind a stored
// URL: `count` bytes starting at `offset`. Backs the content route's
// single-range Range support (design C5, 206/Content-Range).
func (u *AzureBlobUploader) DownloadRangeURL(ctx context.Context, blobURL string, offset, count int64) (io.ReadCloser, error) {
	container, objectName, ok := u.splitStoredURL(blobURL)
	if !ok {
		return nil, fmt.Errorf("not an azure blob URL (no downloadable object): %q", blobURL)
	}
	if offset < 0 || count <= 0 {
		return nil, fmt.Errorf("invalid azure blob range: offset=%d count=%d", offset, count)
	}
	resp, err := u.client.DownloadStream(ctx, container, objectName, &blob.DownloadStreamOptions{
		Range: blob.HTTPRange{Offset: offset, Count: count},
	})
	if err != nil {
		return nil, fmt.Errorf("open azure blob range stream: %w", err)
	}
	return resp.Body, nil
}
