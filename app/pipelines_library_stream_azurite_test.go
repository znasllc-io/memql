package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

type pipelineLostBlobReply struct{ *azureblob.AzureBlobUploader }

func (u pipelineLostBlobReply) CreateVerifiedStream(ctx context.Context, container, object string, body io.Reader, size int64, digest, mime string) (azureblob.VerifiedBlob, error) {
	if _, err := u.AzureBlobUploader.CreateVerifiedStream(ctx, container, object, body, size, digest, mime); err != nil {
		return azureblob.VerifiedBlob{}, err
	}
	return azureblob.VerifiedBlob{}, errors.New("injected lost upload reply after verified commit")
}

type pipelineZeroReader struct{}

func (pipelineZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// Real Postgres and two separate Azure clients: neither replacement has the
// first producer's memory, source bytes, stream position or engine cache.
func TestPipelineStreamAzuriteDBRecoveryAcrossClients(t *testing.T) {
	conn := os.Getenv("MEMQL_AZURITE_TEST_CONNECTION_STRING")
	if conn == "" {
		t.Skip("MEMQL_AZURITE_TEST_CONNECTION_STRING is required for real blob recovery evidence")
	}
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", conn)
	client, err := azblob.NewClientFromConnectionString(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	container := "pipeline-stream-" + strings.ToLower(id.NewShortId())
	if _, err := client.CreateContainer(ctx, container, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := client.DeleteContainer(ctx, container, nil); err != nil {
			t.Error(err)
			return
		}
		_, err := client.ServiceClient().NewContainerClient(container).GetProperties(ctx, nil)
		if !bloberror.HasCode(err, bloberror.ContainerNotFound) {
			t.Errorf("fixture container cleanup unconfirmed: %v", err)
		}
	})
	u1, err := azureblob.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := azureblob.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, _, db := pipelineStreamDBStore(t, pipelineLostBlobReply{u1})
	second, _, _ := pipelineStreamDBStore(t, u2)
	first.bucket, second.bucket = container, container
	f := pipelineStreamFile(id.NewShortId(), "")
	const size = int64(128 << 20)
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(pipelineZeroReader{}, size)); err != nil {
		t.Fatal(err)
	}
	f.Size, f.SHA256, f.Body = size, hex.EncodeToString(hash.Sum(nil)), io.LimitReader(pipelineZeroReader{}, size)
	if got, err := first.StoreRunFileStream(ctx, f); err == nil || got.FileID != "" {
		t.Fatalf("lost commit reply succeeded: %+v %v", got, err)
	}
	x, _ := first.streamIdentity(f)
	var originalID, originalObject, state string
	if err := db.QueryRowContext(ctx, "SELECT file_id, object_key, state FROM pipeline_artifact_uploads WHERE intent_id=$1", pipelineIntentID(x)).Scan(&originalID, &originalObject, &state); err != nil || state != "reserved" {
		t.Fatalf("unknown commit reservation: %s %v", state, err)
	}
	f.Body = pipelineUnreadableSource{}
	delegate := second.engine
	second.engine = pipelineStreamEngineFunc(func(ctx context.Context, q string) (any, error) {
		result, err := delegate.Execute(ctx, q)
		if strings.HasPrefix(q, "mutation recordStoredPipelineFile(") && err == nil {
			return nil, errors.New("injected committed row with lost reply")
		}
		return result, err
	})
	if got, err := second.StoreRunFileStream(ctx, f); err == nil || got.FileID != "" {
		t.Fatalf("lost Library reply succeeded: %+v %v", got, err)
	}
	second.engine = delegate
	got, err := second.StoreRunFileStream(ctx, f)
	if err != nil || got.Receipt == nil || got.FileID != originalID || got.Receipt.Object != originalObject || got.Receipt.Size != size || got.Receipt.SHA256 != f.SHA256 || got.Receipt.ETag == "" {
		t.Fatalf("replacement recovery: %+v %v", got, err)
	}
	verified, err := u1.VerifyStream(ctx, container, originalObject, size, f.SHA256)
	if err != nil || verified.ETag != got.Receipt.ETag {
		t.Fatalf("independent version read: %+v %v", verified, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT state FROM pipeline_artifact_uploads WHERE intent_id=$1", got.Receipt.IntentID).Scan(&state); err != nil || state != "ready" {
		t.Fatalf("final receipt: %s %v", state, err)
	}
	t.Logf("verified %d streamed bytes across lost upload reply, lost Library reply and independent-client recovery; file=%s etag=%s", size, got.FileID, got.Receipt.ETag)
}
