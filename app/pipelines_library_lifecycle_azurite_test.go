package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type pipelineLostRetirementReply struct{ *azureblob.AzureBlobUploader }

func (u pipelineLostRetirementReply) RetireVerifiedStream(ctx context.Context, container, object string, receipt azureblob.VerifiedBlob, token string) (azureblob.RetiredBlob, error) {
	if _, err := u.AzureBlobUploader.RetireVerifiedStream(ctx, container, object, receipt, token); err != nil {
		return azureblob.RetiredBlob{}, err
	}
	return azureblob.RetiredBlob{}, errors.New("lost successful retirement response")
}

func TestPipelineLifecycleAzuriteDBRetirementRecoveryAcrossClients(t *testing.T) {
	conn := os.Getenv("MEMQL_AZURITE_TEST_CONNECTION_STRING")
	if conn == "" {
		t.Skip("MEMQL_AZURITE_TEST_CONNECTION_STRING is required for real retirement evidence")
	}
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", conn)
	client, err := azblob.NewClientFromConnectionString(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	container := "pipeline-retire-" + strings.ToLower(id.NewShortId())
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
		if _, err := client.ServiceClient().NewContainerClient(container).GetProperties(ctx, nil); !bloberror.HasCode(err, bloberror.ContainerNotFound) {
			t.Errorf("fixture cleanup unconfirmed: %v", err)
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
	one, _, db := pipelineStreamDBStore(t, pipelineLostRetirementReply{u1})
	two, _, _ := pipelineStreamDBStore(t, u2)
	one.bucket, two.bucket = container, container
	f := pipelineStreamFile(id.NewShortId(), "data")
	stored, err := one.StoreRunFileStream(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := pipelineLifecycleScope(f)
	ref := pipelinesteps.RunFileReference{Scope: scope, ReferenceID: "candidate", IntentIDs: []string{stored.Receipt.IntentID}}
	if _, err := two.PinRunFileReceipts(owner, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := one.RetireRunFile(owner, scope, stored.Receipt.IntentID); err == nil {
		t.Fatal("retired pinned real blob")
	}
	if err := two.ReleaseRunFileReference(owner, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := one.RetireRunFile(owner, scope, stored.Receipt.IntentID); err == nil {
		t.Fatal("lost retirement reply reported success")
	}
	var state string
	if err := db.QueryRow("SELECT state FROM pipeline_artifact_uploads WHERE intent_id=$1", stored.Receipt.IntentID).Scan(&state); err != nil || state != "retiring" {
		t.Fatal(state, err)
	}
	if _, err := two.RetireRunFile(owner, scope, stored.Receipt.IntentID); err != nil {
		t.Fatal("replacement retirement", err)
	}
	if _, err := u1.DownloadURL(owner, stored.Receipt.URL); !errors.Is(err, azureblob.ErrBlobRetired) {
		t.Fatal("retired object downloaded", err)
	}
	f.Body = strings.NewReader("data")
	if _, err := one.StoreRunFileStream(ctx, f); err == nil {
		t.Fatal("retired destination admitted retry")
	}
	t.Log("real leased tombstone and durable retirement recovered across independent clients")
}
