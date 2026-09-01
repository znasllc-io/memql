package azureblob

// blocks_azurite_test.go -- the staged-block and streaming APIs against a
// REAL Azure blob implementation (memql#4782).
//
// Gated on MEMQL_AZURITE_TEST_CONNECTION_STRING: the handler-level suites
// in component/server prove the session protocol over fakes, and this file
// proves the fakes' contract holds where it matters -- Azurite implements
// the same block-blob API the cloud does, so what passes here is evidence
// about StageBlock/CommitBlockList/GetBlockList/DownloadStream themselves,
// not about our model of them.
//
// Run locally with a throwaway emulator:
//
//	docker run -d --name azurite -p 127.0.0.1:10010:10000 \
//	  mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0
//	MEMQL_AZURITE_TEST_CONNECTION_STRING='DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10010/devstoreaccount1;' \
//	  go test ./integrations/azureblob/ -run Azurite -v
//
// Skips -- loudly, naming the variable -- when the emulator is not there.

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

const azuriteEnv = "MEMQL_AZURITE_TEST_CONNECTION_STRING"

func azuriteUploader(t *testing.T) *AzureBlobUploader {
	t.Helper()
	connStr := os.Getenv(azuriteEnv)
	if connStr == "" {
		t.Skipf("%s is not set; the staged-block API is proven against fakes only in this run", azuriteEnv)
	}
	client, err := azblob.NewClientFromConnectionString(connStr, nil)
	if err != nil {
		t.Fatalf("azurite client: %v", err)
	}
	u := &AzureBlobUploader{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := u.EnsureContainer(ctx, "blockstest"); err != nil {
		t.Fatalf("ensure container: %v", err)
	}
	return u
}

func TestAzuriteStagedBlockLifecycle(t *testing.T) {
	u := azuriteUploader(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	object := fmt.Sprintf("library/u-1/f-%d/big.bin", time.Now().UnixNano())

	chunk1 := bytes.Repeat([]byte("a"), 1024)
	chunk2 := bytes.Repeat([]byte("b"), 512)

	// Stage out of order -- the commit's ordering, not the staging's, is
	// what decides the blob.
	if err := u.StageBlock(ctx, "blockstest", object, blockIDForTest(2), chunk2); err != nil {
		t.Fatalf("stage 2: %v", err)
	}
	if err := u.StageBlock(ctx, "blockstest", object, blockIDForTest(1), chunk1); err != nil {
		t.Fatalf("stage 1: %v", err)
	}

	// The inventory sees both, with sizes -- what resume reads.
	staged, err := u.UncommittedBlocks(ctx, "blockstest", object)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if len(staged) != 2 || staged[blockIDForTest(1)] != 1024 || staged[blockIDForTest(2)] != 512 {
		t.Fatalf("inventory = %v, want blocks 1 (1024) and 2 (512)", staged)
	}

	// Re-staging replaces, not appends -- what makes chunk retry safe.
	if err := u.StageBlock(ctx, "blockstest", object, blockIDForTest(2), chunk2); err != nil {
		t.Fatalf("restage 2: %v", err)
	}
	if staged, err = u.UncommittedBlocks(ctx, "blockstest", object); err != nil || len(staged) != 2 {
		t.Fatalf("inventory after restage = %v (%v), want the same two blocks", staged, err)
	}

	// Commit in order; the blob is the concatenation in COMMIT order.
	if err := u.CommitBlockList(ctx, "blockstest", object,
		[]string{blockIDForTest(1), blockIDForTest(2)}, "application/octet-stream"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	blobURL, err := u.Upload(ctx, "blockstest", object+".probe", []byte("x"), "")
	if err != nil {
		t.Fatalf("probe upload (to learn the base url shape): %v", err)
	}
	base := blobURL[:len(blobURL)-len("/blockstest/"+object+".probe")]
	committedURL := base + "/blockstest/" + object

	data, err := u.DownloadURL(ctx, committedURL)
	if err != nil {
		t.Fatalf("download committed blob: %v", err)
	}
	if want := append(append([]byte{}, chunk1...), chunk2...); !bytes.Equal(data, want) {
		t.Fatalf("committed blob is %d bytes, want %d in commit order", len(data), len(want))
	}

	// After commit the uncommitted inventory is empty -- nothing staged.
	staged, err = u.UncommittedBlocks(ctx, "blockstest", object)
	if err != nil {
		t.Fatalf("inventory after commit: %v", err)
	}
	if len(staged) != 0 {
		t.Fatalf("uncommitted inventory after commit = %v, want empty", staged)
	}

	// Streaming + range: the same bytes, and a 206-shaped slice.
	rc, err := u.DownloadStreamURL(ctx, committedURL)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	streamed, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(streamed, data) {
		t.Fatalf("streamed read differs from buffered read (err=%v)", err)
	}
	rrc, err := u.DownloadRangeURL(ctx, committedURL, 1020, 8)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	ranged, err := io.ReadAll(rrc)
	_ = rrc.Close()
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	if want := data[1020:1028]; !bytes.Equal(ranged, want) {
		t.Fatalf("range 1020+8 = %q, want %q", ranged, want)
	}
}

func TestAzuriteInventoryOfAnUnknownObjectIsEmpty(t *testing.T) {
	u := azuriteUploader(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	staged, err := u.UncommittedBlocks(ctx, "blockstest", "library/u-1/never-staged/none.bin")
	if err != nil {
		t.Fatalf("inventory of an unknown object must be 'nothing staged', got: %v", err)
	}
	if len(staged) != 0 {
		t.Fatalf("inventory = %v, want empty", staged)
	}
}

// blockIDForTest mirrors component/server's uploadBlockID (base64 over
// fixed-width decimal) without importing it -- integrations must not import
// component/server, and one format string is cheaper than a dependency edge.
func blockIDForTest(n int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%08d", n)))
}
