package azureblob

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

func TestStoredBlobURLDecodesSDKPathExactlyOnce(t *testing.T) {
	for _, base := range []string{"https://blob.test", "http://127.0.0.1:10000/account", "https://blob.test/?sig=private"} {
		client, err := azblob.NewClientWithNoCredential(base, nil)
		if err != nil {
			t.Fatal(err)
		}
		u := &AzureBlobUploader{client: client}
		for _, object := range []string{"nested/archive.tar", "nested/space name%2F#?.tar", "literal%252F.txt"} {
			bc, err := u.blockClient("container", object)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := storedBlobURL(bc.URL())
			if err != nil {
				t.Fatal(err)
			}
			for _, stored := range []string{bc.URL(), canonical} {
				container, decoded, ok := u.splitStoredURL(stored)
				if !ok || container != "container" || decoded != object {
					t.Fatalf("roundtrip %q: %q %q %t", stored, container, decoded, ok)
				}
			}
			parsed, _ := url.Parse(canonical)
			if parsed.RawQuery != "" || parsed.User != nil || parsed.Fragment != "" {
				t.Fatal("stored reference retained connection credentials or fragment")
			}
		}
	}
}

func TestAzuriteStoredBlobURLsRoundTripNestedEscapedPaths(t *testing.T) {
	conn := os.Getenv(azuriteEnv)
	if conn == "" {
		t.Skip("MEMQL_AZURITE_TEST_CONNECTION_STRING is required for stored URL roundtrip evidence")
	}
	client, err := azblob.NewClientFromConnectionString(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := &AzureBlobUploader{client: client}
	container := "roundtrip-" + strings.ToLower(rand.Text())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := u.EnsureContainer(ctx, container); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := client.DeleteContainer(ctx, container, nil); err != nil {
			t.Error("fixture container cleanup", err)
		}
	})
	body := []byte("read these exact bytes")
	for _, streamed := range []bool{false, true} {
		object := "nested/space name%2F#?.tar"
		if streamed {
			object = "stream/" + object
		}
		var reference string
		var err error
		if streamed {
			var receipt VerifiedBlob
			receipt, err = u.CreateVerifiedStream(ctx, container, object, bytes.NewReader(body), int64(len(body)), verifiedSHA(body), "")
			reference = receipt.URL
		} else {
			reference, err = u.Upload(ctx, container, object, body, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := u.DownloadURL(ctx, reference)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("roundtrip streamed=%t: %q %v", streamed, got, err)
		}
	}
}
