package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/calls"
)

type portraitRetirerFixture struct {
	calls                                       int
	data                                        []byte
	absent                                      bool
	verifyErr                                   error
	owner, bucket, object, digest, retirementID string
	size, max                                   int64
	receipt                                     azureblob.VerifiedBlob
}

func (f *portraitRetirerFixture) remember(ctx context.Context, bucket, object string) {
	f.calls++
	f.bucket = bucket
	f.object = object
	ac, _ := auth.AccessFromContext(ctx)
	v, _ := auth.ActorEnvelopeValue(ac, "userId")
	f.owner, _ = v.(string)
}
func (f *portraitRetirerFixture) DownloadWithLimit(ctx context.Context, bucket, object string, max int64) ([]byte, error) {
	f.remember(ctx, bucket, object)
	f.max = max
	if f.absent {
		return nil, &azcore.ResponseError{StatusCode: 404, ErrorCode: "BlobNotFound"}
	}
	return f.data, nil
}
func (f *portraitRetirerFixture) VerifyStream(ctx context.Context, bucket, object string, size int64, digest string) (azureblob.VerifiedBlob, error) {
	f.remember(ctx, bucket, object)
	f.digest = digest
	f.size = size
	if f.absent {
		return azureblob.VerifiedBlob{}, &azcore.ResponseError{StatusCode: 404, ErrorCode: "BlobNotFound"}
	}
	return azureblob.VerifiedBlob{Size: size, SHA256: digest, ETag: `"owned-etag"`}, f.verifyErr
}
func (f *portraitRetirerFixture) RetireVerifiedStream(ctx context.Context, bucket, object string, receipt azureblob.VerifiedBlob, identity string) (azureblob.RetiredBlob, error) {
	f.remember(ctx, bucket, object)
	f.receipt = receipt
	f.retirementID = identity
	return azureblob.RetiredBlob{ETag: `"retired"`}, nil
}
func TestPortraitStorageRetirementOnlyAcceptsNativeOwnedObjectPath(t *testing.T) {
	f := &portraitRetirerFixture{}
	b := &callsPortraitBlobStore{bucket: "private", retirement: f}
	ctx := context.Background()
	digest := strings.Repeat("a", 64)
	for _, path := range []string{"https://example.test/private/portrait", "objects/other/calls/site/portraits/image", "objects/owner/calls/site/portraits/../image", "objects/owner/calls/site/files/image", "objects/owner/calls/site/portraits/image/extra"} {
		_, e := b.InspectRetirement(ctx, "owner", path, digest, 1)
		require.Error(t, e)
		require.Error(t, b.Retire(ctx, "owner", path, digest, calls.PortraitRetirementReceipt{Size: 1}, "11111111-1111-4111-8111-111111111111"))
	}
	require.Zero(t, f.calls)
}
func TestPortraitStorageRetirementPinsOwnerBytesETagAndPrivateLease(t *testing.T) {
	f := &portraitRetirerFixture{data: []byte("canonical image fixture")}
	b := &callsPortraitBlobStore{bucket: "private", retirement: f}
	ctx := context.Background()
	digest := callrules.PortraitContentDigest(f.data)
	const object = "objects/owner/calls/site/portraits/image"
	receipt, e := b.InspectRetirement(ctx, "owner", object, digest, 0)
	require.NoError(t, e)
	require.EqualValues(t, len(f.data), receipt.Size)
	require.Equal(t, `"owned-etag"`, receipt.ETag)
	require.EqualValues(t, callrules.PortraitCanonicalBytes, f.max)
	require.NoError(t, b.Retire(ctx, "owner", object, digest, receipt, "11111111-1111-4111-8111-111111111111"))
	require.Equal(t, object, f.object)
	require.Equal(t, "private", f.bucket)
	require.Equal(t, "owner", f.owner)
	require.Equal(t, digest, f.receipt.SHA256)
	require.Equal(t, receipt.ETag, f.receipt.ETag)
	require.Equal(t, receipt.Size, f.receipt.Size)
	require.Equal(t, "11111111-1111-4111-8111-111111111111", f.retirementID)
}
func TestPortraitStorageMissingPendingBlobAndChangedVersion(t *testing.T) {
	f := &portraitRetirerFixture{absent: true}
	b := &callsPortraitBlobStore{bucket: "private", retirement: f}
	ctx := context.Background()
	digest := strings.Repeat("a", 64)
	receipt, e := b.InspectRetirement(ctx, "owner", "objects/owner/calls/site/portraits/image", digest, 123)
	require.NoError(t, e)
	require.EqualValues(t, 123, receipt.Size)
	require.Empty(t, receipt.ETag)
	f.absent = false
	f.verifyErr = errors.New("exact bytes or ETag changed")
	_, e = b.InspectRetirement(ctx, "owner", "objects/owner/calls/site/portraits/image", digest, 123)
	require.Error(t, e)
	require.Empty(t, f.retirementID)
}
