package azureblob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func verifiedAzuriteObject(t *testing.T, u *AzureBlobUploader) string {
	t.Helper()
	object := fmt.Sprintf("verified-stream/%d/archive", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client, err := u.blockClient("blockstest", object)
		if err != nil {
			t.Error(err)
			return
		}
		// This random object belongs solely to this fixture, including any
		// deliberately mismatched content. The container is disposable too.
		if _, err := client.Delete(ctx, nil); err != nil && !isBlobNotFound(err) {
			t.Errorf("fixture object cleanup: %v", err)
		}
	})
	return object
}

func TestAzuriteVerifiedStreamLargeObjectAndRecovery(t *testing.T) {
	u := azuriteUploader(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	object := verifiedAzuriteObject(t, u)
	// Larger than the old in-memory artifact limit, generated and verified
	// without allocating a body proportional to this size.
	const size int64 = 128 << 20
	hash := sha256.New()
	if n, err := io.Copy(hash, io.LimitReader(zeroDownloadReader{}, size)); err != nil || n != size {
		t.Fatal(n, err)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	got, err := u.CreateVerifiedStream(ctx, "blockstest", object, io.LimitReader(zeroDownloadReader{}, size), size, digest, "application/octet-stream")
	if err != nil || got.ETag == "" || got.Size != size || got.SHA256 != digest {
		t.Fatalf("large upload: %+v %v", got, err)
	}
	other := &AzureBlobUploader{client: u.client}
	recovered, err := other.VerifyStream(ctx, "blockstest", object, size, digest)
	if err != nil || got != recovered {
		t.Fatalf("replacement reconciler: %+v %v", recovered, err)
	}
	if _, err := other.CreateVerifiedStream(ctx, "blockstest", object, strings.NewReader("replacement"), 11, verifiedSHA([]byte("replacement")), ""); err == nil {
		t.Fatal("different upload overwrote the committed object")
	}
	if same, err := u.VerifyStream(ctx, "blockstest", object, size, digest); err != nil || same != got {
		t.Fatalf("original object was changed: %+v %v", same, err)
	}
	t.Logf("verified %d bytes through streaming upload, readback, recovery and overwrite refusal", size)
}

func TestAzuriteVerifiedEmptyStream(t *testing.T) {
	u := azuriteUploader(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := u.CreateVerifiedStream(ctx, "blockstest", verifiedAzuriteObject(t, u), bytes.NewReader(nil), 0, verifiedSHA(nil), ""); err != nil {
		t.Fatal(err)
	}
}

func TestAzuriteVerifiedArchiveRehearsal(t *testing.T) {
	path := os.Getenv("MEMQL_VERIFIED_STREAM_REHEARSAL_FILE")
	if path == "" {
		t.Skip("MEMQL_VERIFIED_STREAM_REHEARSAL_FILE is not set; no real build archive supplied")
	}
	u := azuriteUploader(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxVerifiedStreamBytes {
		t.Fatal("fixture needs a bounded regular archive", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	got, err := u.CreateVerifiedStream(ctx, "blockstest", verifiedAzuriteObject(t, u), file, info.Size(), hex.EncodeToString(hash.Sum(nil)), "application/vnd.oci.image.layout.v1.tar")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real archive retained and readback verified: size=%d sha256=%s", got.Size, got.SHA256)
}
