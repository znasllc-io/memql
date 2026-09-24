package packages

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

func TestExternalAssetCacheUsesAzureProtocolAndDeclaredReadBudget(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			objects[r.URL.Path] = raw
			w.Header().Set("ETag", `"asset"`)
			w.WriteHeader(http.StatusCreated)
			return
		}
		raw, ok := objects[r.URL.Path]
		if !ok {
			w.Header().Set("x-ms-error-code", "BlobNotFound")
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", "AccountName=test;AccountKey="+key+";BlobEndpoint="+server.URL+"/test;")
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "memql")
	t.Setenv("MEMQL_AZURE_BLOB_AUTOCREATE", "false")
	ctx := context.Background()
	object := "packages/assets/package/digest"
	if _, found, err := (azureAssetCache{}).Read(ctx, object, 5); err != nil || found {
		t.Fatalf("empty cache: %v %v", found, err)
	}
	if err := (azureAssetCache{}).Write(ctx, object, []byte("movie")); err != nil {
		t.Fatal(err)
	}
	// New cache instance models another replica using the same object store.
	if got, found, err := (azureAssetCache{}).Read(ctx, object, 5); err != nil || !found || string(got) != "movie" {
		t.Fatalf("durable cache: %q %v %v", got, found, err)
	}
	if _, _, err := (azureAssetCache{}).Read(ctx, object, 4); err == nil {
		t.Fatal("cache read ignored declared byte limit")
	}
}
