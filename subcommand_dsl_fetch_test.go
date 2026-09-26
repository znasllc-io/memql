package main

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDslFetchUsesTheSeededStorageSecret(t *testing.T) {
	t.Chdir(t.TempDir()) // no developer .env layer
	var requests atomic.Int32
	var denied atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if denied.Load() {
			w.Header().Set("x-ms-error-code", "AuthorizationFailure")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("x-ms-error-code", "BlobNotFound")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	t.Setenv("MEMQL_DSL_PATH", t.TempDir())
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "memql")
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", "")
	t.Setenv("AZURE_BLOB_CONNECTION_STRING", "AccountName=test;AccountKey="+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))+";BlobEndpoint="+server.URL+"/test;")
	if code := runDslFetchSubcommand(nil); code != 0 || requests.Load() == 0 {
		t.Fatalf("dsl-fetch did not reach storage through the seeded secret: code=%d requests=%d", code, requests.Load())
	}
	// An unreadable pointer is not an empty installation: boot must refuse
	// rather than silently dropping all deployed package definitions.
	denied.Store(true)
	if code := runDslFetchSubcommand(nil); code != 1 {
		t.Fatalf("storage authorization failure allowed boot: exit %d", code)
	}
}
