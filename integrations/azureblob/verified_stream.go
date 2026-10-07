package azureblob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
)

const (
	MaxVerifiedStreamBytes int64 = 2 << 30
	verifiedBlockBytes           = 4 << 20
)

// ErrBlobCommitUncertain means a commit was attempted but its stored bytes
// could not be confirmed. Reconcile the same object, size and digest; never
// select a fresh destination merely because the previous reply was lost.
var ErrBlobCommitUncertain = errors.New("blob commit outcome is uncertain")

// ErrBlobDeleteUncertain requires a later read of the same owned object. An
// accepted DELETE without confirmed absence is not a cleanup receipt.
var ErrBlobDeleteUncertain = errors.New("blob deletion outcome is uncertain")

// VerifiedBlob names the exact version whose bytes were read and hashed.
// CreateVerifiedStream never overwrites a committed object. This is not a
// storage-account WORM policy: consumers must retain this ETag and digest and
// verify them when using the object. Authorization and retention are callers'.
type VerifiedBlob struct {
	URL, ETag, SHA256 string
	Size              int64
}

// CreateVerifiedStream stages at most 2 GiB in 4 MiB blocks and commits only
// after the stream's exact length and SHA-256 match. Attempt-specific block
// names prevent simultaneous writers from mixing their staged bytes. A
// conditional commit preserves an existing object; adoption always hashes the
// actual stored bytes under an ETag guard, never trusts digest metadata.
//
// Caller owns source and its cancellation. Account credentials stay in this
// adapter, not in a build container. Uncommitted blocks from a failed attempt
// expire under the provider's retention; committed-object cleanup is explicit.
// This bounded primitive also supports exports and backup archives.
func (u *AzureBlobUploader) CreateVerifiedStream(ctx context.Context, container, object string, source io.Reader, size int64, digest, contentType string) (VerifiedBlob, error) {
	if source == nil {
		return VerifiedBlob{}, errors.New("verified upload requires a source")
	}
	if err := verifiedStreamIdentity(size, digest); err != nil {
		return VerifiedBlob{}, err
	}
	bc, err := u.blockClient(container, object)
	if err != nil {
		return VerifiedBlob{}, err
	}
	// Recovery does not restage or replace an existing committed object.
	if stored, err := verifyStream(ctx, bc, size, digest); err == nil {
		return stored, nil
	} else if !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return VerifiedBlob{}, err
	}

	prefix := rand.Text()
	blocks := make([]string, 0, size/verifiedBlockBytes+1)
	buffer := make([]byte, verifiedBlockBytes)
	hash := sha256.New()
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return VerifiedBlob{}, err
		}
		chunk := buffer[:min(int64(len(buffer)), remaining)]
		if _, err := io.ReadFull(source, chunk); err != nil {
			return VerifiedBlob{}, fmt.Errorf("read verified upload: %w", err)
		}
		_, _ = hash.Write(chunk)
		block := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s-%08d", prefix, len(blocks))))
		if err := u.StageBlock(ctx, container, object, block, chunk); err != nil {
			return VerifiedBlob{}, err
		}
		blocks = append(blocks, block)
		remaining -= int64(len(chunk))
	}
	// A declared size is a bound, not permission to silently truncate input.
	var extra [1]byte
	if n, err := io.ReadFull(source, extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return VerifiedBlob{}, errors.New("verified upload stream exceeds its size or did not end cleanly")
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return VerifiedBlob{}, errors.New("verified upload SHA-256 mismatch; no object was committed")
	}
	if err := ctx.Err(); err != nil {
		return VerifiedBlob{}, err
	}
	star := azcore.ETag("*")
	opts := &blockblob.CommitBlockListOptions{
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &star}},
	}
	if contentType = strings.TrimSpace(contentType); contentType != "" {
		opts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: &contentType}
	}
	_, commitErr := bc.CommitBlockList(ctx, blocks, opts)
	// A successful response, a conflict and a lost response all need the same
	// evidence. Never replay the commit without its create-only condition.
	stored, verifyErr := verifyStream(ctx, bc, size, digest)
	if verifyErr == nil {
		return stored, nil
	}
	return VerifiedBlob{}, fmt.Errorf("%w: %w", ErrBlobCommitUncertain, errors.Join(commitErr, verifyErr))
}

// VerifyStream reconciles a previous create using the pinned expected identity.
// It neither writes nor repairs mismatching content.
func (u *AzureBlobUploader) VerifyStream(ctx context.Context, container, object string, size int64, digest string) (VerifiedBlob, error) {
	if err := verifiedStreamIdentity(size, digest); err != nil {
		return VerifiedBlob{}, err
	}
	bc, err := u.blockClient(container, object)
	if err != nil {
		return VerifiedBlob{}, err
	}
	return verifyStream(ctx, bc, size, digest)
}

// DeleteVerifiedStream removes only the exact stored version named by receipt.
// The caller must authorize ownership and retire all producers before cleanup;
// this primitive cannot fence later writes. A changed version is never deleted,
// even if its bytes match. Absence is verified after deletion, including when
// the reply was lost. The receipt URL is not followed or used for authorization.
func (u *AzureBlobUploader) DeleteVerifiedStream(ctx context.Context, container, object string, receipt VerifiedBlob) error {
	if err := verifiedStreamIdentity(receipt.Size, receipt.SHA256); err != nil {
		return err
	}
	if !validVerifiedETag(receipt.ETag) {
		return errors.New("verified deletion requires the exact quoted ETag from its receipt")
	}
	bc, err := u.blockClient(container, object)
	if err != nil {
		return err
	}
	current, err := verifyStream(ctx, bc, receipt.Size, receipt.SHA256)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.ETag != receipt.ETag {
		return errors.New("stored object version differs from the cleanup receipt; nothing was deleted")
	}
	etag := azcore.ETag(receipt.ETag)
	_, deleteErr := bc.Delete(ctx, &blob.DeleteOptions{
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag}},
	})
	_, verifyErr := bc.GetProperties(ctx, nil)
	if bloberror.HasCode(verifyErr, bloberror.BlobNotFound) {
		return nil
	}
	if verifyErr == nil {
		verifyErr = errors.New("an object remains at the cleanup destination")
	}
	return fmt.Errorf("%w: %w", ErrBlobDeleteUncertain, errors.Join(deleteErr, verifyErr))
}

func verifiedStreamIdentity(size int64, digest string) error {
	if size < 0 || size > MaxVerifiedStreamBytes {
		return errors.New("verified stream size is outside the 0 through 2 GiB bound")
	}
	if len(digest) != sha256.Size*2 {
		return errors.New("verified stream needs a lowercase SHA-256 digest")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != digest {
		return errors.New("verified stream needs a lowercase SHA-256 digest")
	}
	return nil
}

func verifyStream(ctx context.Context, bc *blockblob.Client, size int64, digest string) (VerifiedBlob, error) {
	props, err := bc.GetProperties(ctx, nil)
	if err != nil {
		return VerifiedBlob{}, err
	}
	if hasRetirementMarker(props.Metadata) {
		return VerifiedBlob{}, ErrBlobRetired
	}
	if props.ETag == nil || *props.ETag == "" || props.ContentLength == nil || *props.ContentLength != size {
		return VerifiedBlob{}, errors.New("stored object has no version identity or differs from the expected size")
	}
	response, err := bc.DownloadStream(ctx, &blob.DownloadStreamOptions{
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: props.ETag}},
	})
	if err != nil {
		return VerifiedBlob{}, err
	}
	defer response.Body.Close()
	if hasRetirementMarker(response.Metadata) {
		return VerifiedBlob{}, ErrBlobRetired
	}
	if response.ETag == nil || *response.ETag != *props.ETag || response.ContentLength == nil || *response.ContentLength != size {
		return VerifiedBlob{}, errors.New("stored object changed during verification")
	}
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, io.LimitReader(response.Body, size+1), make([]byte, 64<<10))
	if err != nil {
		return VerifiedBlob{}, fmt.Errorf("verify stored stream: %w", err)
	}
	if n != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return VerifiedBlob{}, errors.New("stored object bytes differ from their expected identity")
	}
	// A configured client may use SAS authorization. A durable artifact
	// reference must not carry that credential or its expiration with it.
	reference, err := url.Parse(bc.URL())
	if err != nil {
		return VerifiedBlob{}, errors.New("stored stream has no valid reference URL")
	}
	reference.RawQuery, reference.Fragment, reference.User, reference.ForceQuery = "", "", nil, false
	return VerifiedBlob{URL: reference.String(), ETag: string(*props.ETag), Size: size, SHA256: digest}, nil
}
