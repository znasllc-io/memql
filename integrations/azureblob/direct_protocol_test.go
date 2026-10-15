package azureblob

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/google/uuid"
)

// This gate moves only a few bytes. It uses an explicitly configured test
// account and a uniquely named private container, removed even after failure.
// The same protocol assertions run against Azurite or a real Azure account.
func TestDirectProtocolBoundedGrantsFencesAndRanges(t *testing.T) {
	conn := os.Getenv("MEMQL_DIRECT_TEST_CONNECTION_STRING")
	if conn == "" {
		t.Skip("explicit direct-protocol test storage is not configured")
	}
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", conn)
	client, err := azblob.NewClientFromConnectionString(conn, nil)
	if err != nil {
		t.Fatal("test storage configuration is invalid")
	}
	u := &AzureBlobUploader{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	container := "backup-protocol-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err = u.EnsureContainer(ctx, container); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if _, err := client.DeleteContainer(clean, container, nil); err != nil {
			t.Errorf("remove private protocol fixture %s: %v", container, err)
		}
	})
	object := "backups/test-user/test-job/space + unicode é.zip"
	current := uuid.NewString()
	if err = u.PrepareDirect(ctx, container, object, current); err != nil {
		t.Fatal(err)
	}
	if err = u.PrepareDirect(ctx, container, object, current); err != nil {
		t.Fatal("lost prepare response", err)
	}
	blockID := base64.StdEncoding.EncodeToString([]byte("backup-v1-00000000"))
	payload := []byte("tiny-fixture")
	checksum := md5.Sum(payload)
	grant, err := u.DirectBlockGrant(ctx, container, object, current, blockID, int64(len(payload)), base64.StdEncoding.EncodeToString(checksum[:]))
	if err != nil {
		t.Fatal(err)
	}
	send := func(g DirectRequest, data []byte) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, g.Method, g.URL, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range g.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("direct request failed")
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, b
	}
	if code, _ := send(grant, []byte("different!!!")); code/100 == 2 {
		t.Fatal("altered body accepted")
	}
	if code, _ := send(grant, append(payload, 0)); code/100 == 2 {
		t.Fatal("altered length accepted")
	}
	if code, _ := send(grant, payload); code != 201 {
		t.Fatalf("valid signed block refused: %d", code)
	}
	next := uuid.NewString()
	if err = u.FenceDirect(ctx, container, object, current, next); err != nil {
		t.Fatal(err)
	}
	if err = u.FenceDirect(ctx, container, object, current, next); err != nil {
		t.Fatal("lost fence response", err)
	}
	if code, _ := send(grant, payload); code/100 == 2 {
		t.Fatal("old grant survived the fence")
	}
	current = next
	receipt, err := u.CommitDirect(ctx, container, object, current, []string{blockID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Size != int64(len(payload)) {
		t.Fatal("wrong committed size")
	}
	get, err := u.DirectDownloadGrant(ctx, container, object, receipt.ETag, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if code, data := send(get, nil); code != 206 || !bytes.Equal(data, payload[2:6]) {
		t.Fatalf("pinned range refused or incorrect: %d", code)
	}
	changed := get
	changed.Headers = map[string]string{}
	for k, v := range get.Headers {
		changed.Headers[k] = v
	}
	changed.Headers["Range"] = "bytes=0-11"
	if code, _ := send(changed, nil); code/100 == 2 {
		t.Fatal("expanded range accepted")
	}
	next = uuid.NewString()
	if err = u.FenceDirect(ctx, container, object, current, next); err != nil {
		t.Fatal(err)
	}
	if err = u.RetireDirect(ctx, container, object, next); err != nil {
		t.Fatal(err)
	}
	if code, _ := send(get, nil); code/100 == 2 {
		t.Fatal("old version still downloadable after purge")
	}
	if code, _ := send(grant, payload); code/100 == 2 {
		t.Fatal("retired destination resurrected")
	}
}
