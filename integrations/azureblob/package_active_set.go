package azureblob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
)

const packageActiveSetObject = "packages/active.json"
const MaxPackageActiveSetBytes = 1 << 20

// PackageActiveSetSnapshot binds a bounded body to the exact provider version
// returned by the same read. Only BlobNotFound proves absence. The package
// runtime validates the map and preserves unrelated domains; this adapter does
// not interpret product or workflow policy.
type PackageActiveSetSnapshot struct {
	Exists bool
	ETag   string
	Data   []byte
}

// ErrPackageActiveSetChanged is a confirmed conditional-write refusal. No
// fallback write is issued; the caller must observe and reconsider its intent.
var ErrPackageActiveSetChanged = errors.New("package active set version changed")

// ReadPackageActiveSet reads only the fixed native active-set object. It never
// converts authentication, container absence, malformed data or transport
// failures into an empty active set.
func (u *AzureBlobUploader) ReadPackageActiveSet(ctx context.Context, container string) (PackageActiveSetSnapshot, error) {
	return u.readPackageActiveSet(ctx, container, "")
}

func (u *AzureBlobUploader) readPackageActiveSet(ctx context.Context, container, etag string) (PackageActiveSetSnapshot, error) {
	bc, err := u.blockClient(container, packageActiveSetObject)
	if err != nil {
		return PackageActiveSetSnapshot{}, err
	}
	var opts *blob.DownloadStreamOptions
	if etag != "" {
		version := azcore.ETag(etag)
		opts = &blob.DownloadStreamOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &version}}}
	}
	response, err := bc.DownloadStream(ctx, opts)
	if err != nil {
		if etag == "" && bloberror.HasCode(err, bloberror.BlobNotFound) {
			return PackageActiveSetSnapshot{}, nil
		}
		return PackageActiveSetSnapshot{}, err
	}
	if response.Body == nil {
		return PackageActiveSetSnapshot{}, errors.New("package active set has no response body")
	}
	defer response.Body.Close()
	if hasRetirementMarker(response.Metadata) {
		return PackageActiveSetSnapshot{}, ErrBlobRetired
	}
	if response.ETag == nil || !validVerifiedETag(string(*response.ETag)) || (etag != "" && string(*response.ETag) != etag) {
		return PackageActiveSetSnapshot{}, errors.New("package active set has no matching version identity")
	}
	if response.ContentLength == nil || *response.ContentLength <= 0 || *response.ContentLength > MaxPackageActiveSetBytes {
		return PackageActiveSetSnapshot{}, errors.New("package active set size is outside the 1 byte through 1 MiB bound")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxPackageActiveSetBytes+1))
	if err != nil {
		return PackageActiveSetSnapshot{}, fmt.Errorf("read package active set: %w", err)
	}
	if int64(len(data)) != *response.ContentLength || len(data) > MaxPackageActiveSetBytes {
		return PackageActiveSetSnapshot{}, errors.New("package active set body differs from its declared size")
	}
	return PackageActiveSetSnapshot{Exists: true, ETag: string(*response.ETag), Data: data}, nil
}

// CompareAndSwapPackageActiveSet issues one version-conditional Put Blob at the
// fixed native shared destination. It never replaces an unknown version. A
// successful result includes exact read-back bytes under the returned ETag.
// ErrBlobCommitUncertain requires read-only reconciliation of this same object;
// it is not permission to replay the write. No receipt retires a prior writer.
func (u *AzureBlobUploader) CompareAndSwapPackageActiveSet(ctx context.Context, container string, expected PackageActiveSetSnapshot, data []byte) (PackageActiveSetSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return PackageActiveSetSnapshot{}, err
	}
	if len(data) == 0 || len(data) > MaxPackageActiveSetBytes {
		return PackageActiveSetSnapshot{}, errors.New("package active set size is outside the 1 byte through 1 MiB bound")
	}
	if (expected.Exists && !validVerifiedETag(expected.ETag)) || (!expected.Exists && (expected.ETag != "" || len(expected.Data) != 0)) {
		return PackageActiveSetSnapshot{}, errors.New("package active set requires an exact existing version or observed absence")
	}
	bc, err := u.blockClient(container, packageActiveSetObject)
	if err != nil {
		return PackageActiveSetSnapshot{}, err
	}
	capacity, err := u.beginAssetCapacity(ctx, container, packageActiveSetObject, true)
	if err != nil {
		return PackageActiveSetSnapshot{}, err
	}
	if capacity != nil {
		defer capacity.Close()
		if err = capacity.Put(ctx, int64(len(data))); err != nil {
			return PackageActiveSetSnapshot{}, err
		}
	}
	conditions := &blob.ModifiedAccessConditions{}
	if expected.Exists {
		version := azcore.ETag(expected.ETag)
		conditions.IfMatch = &version
	} else {
		star := azcore.ETag("*")
		conditions.IfNoneMatch = &star
	}
	contentType := "application/json"
	response, err := bc.Upload(ctx, streaming.NopCloser(bytes.NewReader(data)), &blockblob.UploadOptions{
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: conditions},
		HTTPHeaders:      &blob.HTTPHeaders{BlobContentType: &contentType},
	})
	if err != nil {
		// A create-only Put Blob reports BlobAlreadyExists (409) on Azure/
		// Azurite, while an obsolete If-Match reports ConditionNotMet (412).
		// Only the expected-absence request can interpret AlreadyExists as its
		// definitive refusal. Production SDK retries remain disabled, so this
		// cannot be the rejection of an automatic replay after a lost success.
		if bloberror.HasCode(err, bloberror.ConditionNotMet) || (!expected.Exists && bloberror.HasCode(err, bloberror.BlobAlreadyExists)) {
			return PackageActiveSetSnapshot{}, fmt.Errorf("%w: %w", ErrPackageActiveSetChanged, err)
		}
		return PackageActiveSetSnapshot{}, fmt.Errorf("%w: %w", ErrBlobCommitUncertain, err)
	}
	if response.ETag == nil || !validVerifiedETag(string(*response.ETag)) {
		return PackageActiveSetSnapshot{}, fmt.Errorf("%w: package active set write has no version receipt", ErrBlobCommitUncertain)
	}
	if err = u.checkRetention(ctx, response.VersionID); err != nil {
		return PackageActiveSetSnapshot{}, fmt.Errorf("%w: %w", ErrBlobCommitUncertain, err)
	}
	stored, err := u.readPackageActiveSet(ctx, container, string(*response.ETag))
	if err != nil || !bytes.Equal(stored.Data, data) {
		return PackageActiveSetSnapshot{}, fmt.Errorf("%w: %w", ErrBlobCommitUncertain, errors.Join(err, errors.New("package active set write was not verified")))
	}
	if capacity != nil {
		if err = capacity.Settled(ctx, int64(len(data)), true); err != nil {
			return PackageActiveSetSnapshot{}, fmt.Errorf("%w: %w", ErrBlobCommitUncertain, err)
		}
	}
	return stored, nil
}
