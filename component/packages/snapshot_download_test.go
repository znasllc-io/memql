package packages

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/znasllc-io/memql/integrations/azureblob"
)

func servePackageBlob(t *testing.T, raw []byte) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = w.Write(raw)
	}))
	t.Cleanup(server.Close)
	// A generated test-only key lets the actual Azure SDK sign requests to the
	// local fixture. No storage service or production credential is involved.
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", "AccountName=test;AccountKey="+key+";BlobEndpoint="+server.URL+"/test;")
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "memql")
}

func TestPackageBlobReadersUseSourceBudget(t *testing.T) {
	raw := bytes.Repeat([]byte("source"), 32)
	servePackageBlob(t, raw)
	for _, limit := range []int{len(raw) - 1, len(raw), len(raw) + 1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			t.Setenv(MaxSourceBytesEnv, strconv.Itoa(limit))
			publisher := &enginePublisher{}
			reader := &blobReader{}
			for name, read := range map[string]func() ([]byte, error){
				"snapshot confirmation": func() ([]byte, error) {
					return publisher.ReadSnapshot(context.Background(), "blob://packages/snapshots/source.tar.gz")
				},
				"uploaded zip": func() ([]byte, error) { return reader.read(context.Background(), "library/source.zip") },
			} {
				got, err := read()
				if limit < len(raw) {
					if err == nil || got != nil {
						t.Fatalf("%s accepted source above package budget: %d bytes, %v", name, len(got), err)
					}
				} else if err != nil || !bytes.Equal(got, raw) {
					t.Fatalf("%s did not restore complete bytes: %d, %v", name, len(got), err)
				}
			}
		})
	}
}

func TestSnapshotRestoreCrossesAttachmentCeiling(t *testing.T) {
	// A repo source of this size was accepted during analysis but silently
	// truncated by the generic 100 MiB attachment reader on confirmation.
	raw := make([]byte, (100<<20)+1)
	raw[len(raw)-1] = 91
	servePackageBlob(t, raw)
	t.Setenv(MaxSourceBytesEnv, strconv.Itoa(len(raw)))
	publisher := &enginePublisher{}
	restored, err := publisher.ReadSnapshot(context.Background(), "blob://packages/snapshots/source.tar.gz")
	if err != nil || !bytes.Equal(raw, restored) {
		t.Fatalf("snapshot restore: %d bytes, %v", len(restored), err)
	}
	// Keep the generic attachment budget unchanged, and prove the different
	// contract rather than merely increasing a global download ceiling.
	uploader, err := azureblob.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if partial, err := uploader.Download(context.Background(), "memql", "source"); err == nil || partial != nil {
		t.Fatal("attachment ceiling no longer enforced")
	}
}
