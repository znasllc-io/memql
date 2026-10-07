package azureblob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/lease"
)

const retiredStreamMetadata = "memql_retired_stream"

var ErrBlobRetired = errors.New("stored object has been retired")
var ErrBlobRetirementUncertain = errors.New("blob retirement outcome is uncertain")

// RetiredBlob is evidence of a zero-byte tombstone with an infinite write
// lease and no uncommitted blocks. It does not attest removal of provider
// snapshots, soft-deleted versions, or account-level retained history.
type RetiredBlob struct{ ETag string }

// RetireVerifiedStream permanently fences this destination against uploaders
// that have no lease ID. The caller must first durably retire producers and
// exclude live references. It must persist a random canonical UUID leaseID
// before calling, keep it private, and reuse it with the same identity after
// an uncertain result. This adapter does not decide retention or authority.
//
// A verified receipt pins the expected ETag. Before an upload has a receipt,
// an empty ETag permits retirement of either absence or matching stored bytes.
// Never delete the tombstone or break/release its lease: an old create-only
// writer would otherwise be able to resurrect the destination.
//
// An empty block-list commit discards uncommitted blocks, and an active lease
// rejects Put Block without its ID. The second empty commit, UNDER the lease,
// clears staging
// that could race between tombstone creation and lease acquisition.
func (u *AzureBlobUploader) RetireVerifiedStream(ctx context.Context, container, object string, receipt VerifiedBlob, leaseID string) (RetiredBlob, error) {
	if err := verifiedStreamIdentity(receipt.Size, receipt.SHA256); err != nil {
		return RetiredBlob{}, err
	}
	if !validRetirementLeaseID(leaseID) {
		return RetiredBlob{}, errors.New("retirement requires a persisted canonical UUID lease identity")
	}
	if receipt.ETag != "" && !validVerifiedETag(receipt.ETag) {
		return RetiredBlob{}, errors.New("retirement requires the exact quoted ETag from its receipt")
	}
	bc, err := u.blockClient(container, object)
	if err != nil {
		return RetiredBlob{}, err
	}
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%s\n%d\n%s\n%s", leaseID, receipt.Size, receipt.SHA256, receipt.ETag)))
	marker := hex.EncodeToString(fingerprint[:])
	props, err := bc.GetProperties(ctx, nil)
	var condition *blob.ModifiedAccessConditions
	switch {
	case bloberror.HasCode(err, bloberror.BlobNotFound):
		star := azcore.ETag("*")
		condition = &blob.ModifiedAccessConditions{IfNoneMatch: &star}
	case err != nil:
		return RetiredBlob{}, err
	case hasRetirementMarker(props.Metadata):
		if !matchingTombstone(props, marker) {
			return RetiredBlob{}, errors.New("object carries a different retirement identity")
		}
	default:
		current, verifyErr := verifyStream(ctx, bc, receipt.Size, receipt.SHA256)
		if verifyErr != nil {
			return RetiredBlob{}, verifyErr
		}
		if receipt.ETag != "" && receipt.ETag != current.ETag {
			return RetiredBlob{}, errors.New("stored version differs from the retirement receipt")
		}
		etag := azcore.ETag(current.ETag)
		condition = &blob.ModifiedAccessConditions{IfMatch: &etag}
	}
	if condition != nil {
		// A staged-only destination may refuse Put Blob's If-None-Match even
		// though GET/HEAD report absence. A create-only empty block-list commit
		// handles that state without ever replacing committed content.
		_, writeErr := bc.CommitBlockList(ctx, []string{}, &blockblob.CommitBlockListOptions{
			Metadata:         map[string]*string{retiredStreamMetadata: &marker},
			AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: condition},
		})
		props, err = bc.GetProperties(ctx, nil)
		if err != nil || !matchingTombstone(props, marker) {
			return RetiredBlob{}, retirementUncertain(errors.New("initial tombstone readback disagreed"), writeErr, err)
		}
	}
	lc, err := lease.NewBlobClient(bc, &lease.BlobClientOptions{LeaseID: &leaseID})
	if err != nil {
		return RetiredBlob{}, retirementUncertain(err)
	}
	// Azure permits re-acquiring an active lease with the same proposed ID,
	// including recovery after its successful response was lost.
	if _, err := lc.AcquireLease(ctx, -1, &lease.BlobAcquireOptions{ModifiedAccessConditions: &lease.ModifiedAccessConditions{IfMatch: props.ETag}}); err != nil {
		// A matching concurrent retirement or staged-block operation may have
		// advanced the provider version. Reconcile exactly once, preserving the
		// marker/zero-length/ETag guard; never lease an unrelated replacement.
		props, readErr := bc.GetProperties(ctx, nil)
		if readErr != nil || !matchingTombstone(props, marker) {
			return RetiredBlob{}, retirementUncertain(err, readErr)
		}
		if _, retryErr := lc.AcquireLease(ctx, -1, &lease.BlobAcquireOptions{ModifiedAccessConditions: &lease.ModifiedAccessConditions{IfMatch: props.ETag}}); retryErr != nil {
			return RetiredBlob{}, retirementUncertain(err, retryErr)
		}
	}
	access := &blob.AccessConditions{LeaseAccessConditions: &blob.LeaseAccessConditions{LeaseID: &leaseID}}
	_, writeErr := bc.CommitBlockList(ctx, []string{}, &blockblob.CommitBlockListOptions{
		Metadata: map[string]*string{retiredStreamMetadata: &marker}, AccessConditions: access,
	})
	props, err = bc.GetProperties(ctx, &blob.GetPropertiesOptions{AccessConditions: access})
	if err != nil || !matchingTombstone(props, marker) || props.LeaseStatus == nil || *props.LeaseStatus != lease.StatusTypeLocked ||
		props.LeaseDuration == nil || *props.LeaseDuration != lease.DurationTypeInfinite {
		return RetiredBlob{}, retirementUncertain(errors.New("final tombstone or infinite lease readback disagreed"), writeErr, err)
	}
	blocks, err := bc.GetBlockList(ctx, blockblob.BlockListTypeUncommitted, &blockblob.GetBlockListOptions{AccessConditions: access})
	if err != nil || len(blocks.UncommittedBlocks) != 0 {
		return RetiredBlob{}, retirementUncertain(fmt.Errorf("uncommitted block inventory has %d blocks", len(blocks.UncommittedBlocks)), writeErr, err)
	}
	return RetiredBlob{ETag: string(*props.ETag)}, nil
}

func retirementUncertain(errs ...error) error {
	return errors.Join(append([]error{ErrBlobRetirementUncertain, errors.New("the leased empty tombstone was not confirmed")}, errs...)...)
}

func matchingTombstone(p blob.GetPropertiesResponse, marker string) bool {
	var value string
	for key, v := range p.Metadata {
		if strings.EqualFold(key, retiredStreamMetadata) && v != nil {
			value = *v
		}
	}
	return value == marker && p.ContentLength != nil && *p.ContentLength == 0 && p.ETag != nil && validVerifiedETag(string(*p.ETag))
}

func hasRetirementMarker(metadata map[string]*string) bool {
	for key := range metadata {
		if strings.EqualFold(key, retiredStreamMetadata) {
			return true
		}
	}
	return false
}

func validVerifiedETag(value string) bool {
	return len(value) >= 3 && len(value) <= 256 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && !strings.ContainsAny(value, "\r\n")
}

func validRetirementLeaseID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == compact && compact != strings.Repeat("0", 32)
}
