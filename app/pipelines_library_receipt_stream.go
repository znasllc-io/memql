package app

import (
	"context"
	"errors"
	"io"

	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type pipelineReceiptStreamOpener interface {
	OpenReceiptStream(context.Context, string, string, azureblob.VerifiedBlob) (io.ReadCloser, error)
}

var _ pipelinesteps.LibraryReceiptOpener = (*pipelinesLibraryStore)(nil)

// Lookup repeats the trusted scope gate and native journal verification. No
// URL, ETag or object key from a client can reach the storage adapter.
func (s *pipelinesLibraryStore) OpenRunFileReceipt(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, intentID string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	rows, err := s.ReadRunFileReceipts(ctx, scope, []string{intentID})
	if err != nil {
		return pipelinesteps.StoredFileReceipt{}, nil, err
	}
	opener, ok := s.uploader.(pipelineReceiptStreamOpener)
	if !ok {
		return pipelinesteps.StoredFileReceipt{}, nil, errors.New("version-bound artifact downloads are unavailable")
	}
	receipt := rows[0]
	body, err := opener.OpenReceiptStream(ctx, receipt.Container, receipt.Object, azureblob.VerifiedBlob{ETag: receipt.ETag, Size: receipt.Size, SHA256: receipt.SHA256})
	if err != nil {
		return pipelinesteps.StoredFileReceipt{}, nil, err
	}
	return receipt, body, nil
}
