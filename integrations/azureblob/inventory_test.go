package azureblob

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

func TestInventoryBoundsParallelMetadataReadsAndIncludesStagedBytes(t *testing.T) {
	var active, peak atomic.Int32
	var once sync.Once
	barrier := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case r.URL.Query().Get("comp") == "blocklist":
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			if n == 16 {
				once.Do(func() { close(barrier) })
			}
			select {
			case <-barrier:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `<BlockList><CommittedBlocks/><UncommittedBlocks><Block><Name>YQ==</Name><Size>2</Size></Block></UncommittedBlocks></BlockList>`)
		case r.URL.Query().Get("restype") == "container":
			fmt.Fprint(w, `<EnumerationResults><Blobs>`)
			for n := 0; n < 33; n++ {
				fmt.Fprintf(w, `<Blob><Name>object-%d</Name><Properties><Content-Length>3</Content-Length><BlobType>BlockBlob</BlobType></Properties></Blob>`, n)
			}
			fmt.Fprint(w, `</Blobs><NextMarker/></EnumerationResults>`)
		default:
			fmt.Fprint(w, `<EnumerationResults><Containers><Container><Name>fixture</Name></Container></Containers><NextMarker/></EnumerationResults>`)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := azblob.NewClientWithNoCredential(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := &AzureBlobUploader{client: client}
	var count int
	var bytes int64
	err = u.inventory(ctx, func(_, _ string, size int64) error { count++; bytes += size; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if count != 33 || bytes != 33*5 || peak.Load() != 16 {
		t.Fatalf("inventory: objects=%d bytes=%d peak=%d", count, bytes, peak.Load())
	}
}
