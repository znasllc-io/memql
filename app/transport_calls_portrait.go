package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/calls"
)

type callsPortraitBlobReader interface {
	DownloadURLWithLimit(context.Context, string, int64) ([]byte, error)
}
type callsPortraitBlobStore struct {
	uploader   server.FileUploader
	reader     callsPortraitBlobReader
	bucket     string
	retirement callsPortraitRetirer
}

func newCallsPortraitBlobStore(uploader server.FileUploader, bucket string) *callsPortraitBlobStore {
	reader, ok := uploader.(callsPortraitBlobReader)
	if uploader == nil || bucket == "" || !ok {
		return nil
	}
	retirement, _ := uploader.(callsPortraitRetirer)
	return &callsPortraitBlobStore{uploader: uploader, reader: reader, bucket: bucket, retirement: retirement}
}
func (b *callsPortraitBlobStore) Put(ctx context.Context, owner, object string, data []byte, contentType string) (string, error) {
	return b.uploader.Upload(auth.ContextWithUserActor(ctx, owner), b.bucket, object, data, contentType)
}
func (b *callsPortraitBlobStore) Get(ctx context.Context, owner, ref string, maxBytes int64) ([]byte, error) {
	return b.reader.DownloadURLWithLimit(auth.ContextWithUserActor(ctx, owner), ref, maxBytes)
}

// Both provider inspection and retirement address only the native object path;
// an object's returned download URL is never a deletion capability.
type callsPortraitRetirer interface {
	DownloadWithLimit(context.Context, string, string, int64) ([]byte, error)
	VerifyStream(context.Context, string, string, int64, string) (azureblob.VerifiedBlob, error)
	RetireVerifiedStream(context.Context, string, string, azureblob.VerifiedBlob, string) (azureblob.RetiredBlob, error)
}

func (b *callsPortraitBlobStore) InspectRetirement(ctx context.Context, owner, object, digest string, size int64) (calls.PortraitRetirementReceipt, error) {
	var out calls.PortraitRetirementReceipt
	if b.retirement == nil || !ownedPortraitObject(owner, object) {
		return out, fmt.Errorf("portrait retirement is unavailable")
	}
	ctx = auth.ContextWithUserActor(ctx, owner)
	if size == 0 {
		data, e := b.retirement.DownloadWithLimit(ctx, b.bucket, object, callrules.PortraitCanonicalBytes)
		if e != nil && !azureblob.IsBlobAbsent(e) {
			return out, e
		}
		size = int64(len(data))
	}
	if size < 0 || size > callrules.PortraitCanonicalBytes {
		return out, fmt.Errorf("portrait retirement size is invalid")
	}
	receipt, e := b.retirement.VerifyStream(ctx, b.bucket, object, size, digest)
	if azureblob.IsBlobAbsent(e) {
		return calls.PortraitRetirementReceipt{Size: size}, nil
	}
	if e != nil {
		return out, e
	}
	return calls.PortraitRetirementReceipt{Size: receipt.Size, ETag: receipt.ETag}, nil
}
func (b *callsPortraitBlobStore) Retire(ctx context.Context, owner, object, digest string, receipt calls.PortraitRetirementReceipt, retirementID string) error {
	if b.retirement == nil || !ownedPortraitObject(owner, object) || receipt.Size < 0 || receipt.Size > callrules.PortraitCanonicalBytes {
		return fmt.Errorf("portrait retirement is unavailable")
	}
	_, e := b.retirement.RetireVerifiedStream(auth.ContextWithUserActor(ctx, owner), b.bucket, object, azureblob.VerifiedBlob{Size: receipt.Size, SHA256: digest, ETag: receipt.ETag}, retirementID)
	return e
}
func ownedPortraitObject(owner, object string) bool {
	parts := strings.Split(object, "/")
	return len(parts) == 6 && parts[0] == "objects" && parts[1] == owner && parts[2] == "calls" && parts[4] == "portraits" && callrules.ValidWorkflowID(owner) && callrules.ValidWorkflowID(parts[3]) && callrules.ValidWorkflowID(parts[5])
}
func (a *App) wireCallPortraitMaintenance() {
	if a.engine == nil || a.callsPortraitStorage == nil || a.callsPortraitStorage.retirement == nil {
		return
	}
	if integration, ok := a.engine.IntegrationByName("calls").(*calls.Integration); ok {
		integration.ConfigurePortraitMaintenance(a.callsPortraitStorage)
	}
}
