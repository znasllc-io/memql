package azureblob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
)

// OpenReceiptStream reads exactly the recorded object version. It never
// follows receipt.URL: the configured account plus container/object are the
// only authority. The returned stream checks size and digest before EOF.
// Consumers MUST read to successful EOF before using bytes for an effect and
// MUST close on every outcome. Cancellation interrupts the provider request.
func (u *AzureBlobUploader) OpenReceiptStream(ctx context.Context, container, object string, receipt VerifiedBlob) (io.ReadCloser, error) {
	if err := verifiedStreamIdentity(receipt.Size, receipt.SHA256); err != nil {
		return nil, err
	}
	if !validVerifiedETag(receipt.ETag) {
		return nil, errors.New("receipt download requires its exact ETag")
	}
	bc, err := u.blockClient(container, object)
	if err != nil {
		return nil, err
	}
	etag := azcore.ETag(receipt.ETag)
	response, err := bc.DownloadStream(ctx, &blob.DownloadStreamOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag}}})
	if err != nil {
		return nil, err
	}
	if response.Body == nil {
		return nil, errors.New("receipt download returned no stream")
	}
	if response.ETag == nil || *response.ETag != etag || response.ContentLength == nil || *response.ContentLength != receipt.Size || hasRetirementMarker(response.Metadata) {
		response.Body.Close()
		return nil, errors.New("receipt download differs from the recorded object version")
	}
	return &receiptStream{body: response.Body, remaining: receipt.Size, digest: receipt.SHA256, hash: sha256.New()}, nil
}

type receiptStream struct {
	body      io.ReadCloser
	remaining int64
	digest    string
	hash      hash.Hash
	terminal  error
}

func (s *receiptStream) Close() error { return s.body.Close() }

func (s *receiptStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.terminal != nil {
		return 0, s.terminal
	}
	if s.remaining == 0 {
		var extra [1]byte
		n, err := s.body.Read(extra[:])
		if n != 0 {
			s.terminal = errors.New("receipt stream exceeds recorded size")
		} else if errors.Is(err, io.EOF) {
			if hex.EncodeToString(s.hash.Sum(nil)) != s.digest {
				s.terminal = errors.New("receipt stream digest differs")
			} else {
				s.terminal = io.EOF
			}
		} else if err != nil {
			s.terminal = err
		}
		return 0, s.terminal
	}
	if int64(len(p)) > s.remaining {
		p = p[:s.remaining]
	}
	n, err := s.body.Read(p)
	s.remaining -= int64(n)
	_, _ = s.hash.Write(p[:n])
	if errors.Is(err, io.EOF) {
		if s.remaining != 0 {
			s.terminal = io.ErrUnexpectedEOF
		} else if hex.EncodeToString(s.hash.Sum(nil)) != s.digest {
			s.terminal = errors.New("receipt stream digest differs")
		} else {
			s.terminal = io.EOF
		}
	} else if err != nil {
		s.terminal = err
	}
	return n, s.terminal
}
