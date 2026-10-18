package library

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func testReadFileFromDeliveryReceiptAcrossEngines(t *testing.T, f *revisionDB) {
	fileID := f.owner + "-saved"
	url := "https://store.example/memql-library/library/" + f.owner + "/" + fileID + "/list.md"
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "createLibraryFile", map[string]any{
		"fileId": fileID, "name": "list.md", "mimeType": "text/markdown", "size": 20,
		"blobUrl": url, "source": "agent_generated", "format": "markdown",
	})
	// No artifact index or shared in-memory state on the reading replica.
	fetcher := &fakeBlobStreamer{data: []byte("1. California quail\n")}
	f.second.SetBlobFetcher(fetcher)
	args := map[string]any{"fileId": fileID}
	for _, ctx := range []context.Context{context.Background(), revisionActor(f.owner+"-outsider", auth.RoleWriter)} {
		if _, err := f.second.handleReadFile(ctx, args, 0); err == nil || fetcher.streams != 0 {
			t.Fatal("file bytes read before authorization")
		}
	}
	reader := revisionActor(f.owner, auth.RoleReader)
	rows, err := f.second.handleReadFile(reader, args, 0)
	result := historyResult(t, rows, err)
	if result["content"] != string(fetcher.data) || result["fileId"] != fileID || result["version"] != float64(1) {
		t.Fatalf("receipt did not resolve saved bytes: %v", result)
	}
	update := map[string]any{"fileId": fileID, "versionNumber": 2, "name": "list.md", "mimeType": "text/markdown", "size": 21, "blobUrl": url, "format": "markdown"}
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "supersedeLibraryFileHead", update)
	fetcher.data = []byte("1. Acorn woodpecker\n")
	rows, err = f.second.handleReadFile(reader, args, 0)
	result = historyResult(t, rows, err)
	if result["content"] != string(fetcher.data) || result["version"] != float64(2) || result["revision"] != "file:2" {
		t.Fatalf("read replica kept the old version: %v", result)
	}
	for _, content := range [][]byte{{0xff, 0xfe}, []byte(strings.Repeat("x", 2*1024*1024+1))} {
		fetcher.data = content
		if _, err := f.second.handleReadFile(reader, args, 0); err == nil {
			t.Fatal("invalid UTF-8 or oversized file accepted")
		}
	}
	update["mimeType"] = "image/png"
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "supersedeLibraryFileHead", update)
	before := fetcher.streams
	if _, err := f.second.handleReadFile(reader, args, 0); err == nil || fetcher.streams != before {
		t.Fatal("binary file was fetched as text")
	}
	f.query(f.engine, f.ctx, "mutation", "archiveLibraryFile", map[string]any{"fileId": fileID})
	if _, err := f.second.handleReadFile(reader, args, 0); err == nil || fetcher.streams != before {
		t.Fatal("archived file was read")
	}
}
