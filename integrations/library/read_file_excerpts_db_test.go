package library

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// testReadFileExcerptsAcrossEngines writes two pipeline logs on one engine and
// reads their excerpts from the other, which shares nothing with it but the
// database: authorization comes before any byte is fetched, a revised head is
// read as revised, and a binary or archived file is reported without a fetch.
func testReadFileExcerptsAcrossEngines(t *testing.T, f *revisionDB) {
	first, second := f.owner+"-log-a", f.owner+"-log-b"
	for _, id := range []string{first, second} {
		f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "createLibraryFile", map[string]any{
			"fileId": id, "name": "step.log", "mimeType": "text/plain", "size": 16,
			"blobUrl": excerptBlobURL(f.owner, id), "source": "pipeline", "format": "text",
		})
	}
	fetcher := &urlBlobStreamer{data: map[string][]byte{
		excerptBlobURL(f.owner, first):  []byte("ok\nFAIL first\n"),
		excerptBlobURL(f.owner, second): []byte("FAIL second\n"),
	}}
	f.second.SetBlobFetcher(fetcher)
	args := map[string]any{
		"files":    []any{map[string]any{"fileId": first, "label": "a"}, map[string]any{"fileId": second, "label": "b"}},
		"patterns": []any{"^FAIL"},
		"maxBytes": 4096,
	}
	if _, err := f.second.handleReadFileExcerpts(context.Background(), args, 0); err == nil || fetcher.streams != 0 {
		t.Fatal("an unsigned caller was answered")
	}
	outsider := revisionActor(f.owner+"-outsider", auth.RoleWriter)
	rows, err := f.second.handleReadFileExcerpts(outsider, args, 0)
	for _, e := range excerptEntries(t, rows, err) {
		if e["unavailable"] != "file is unavailable" || e["excerpt"] != nil {
			t.Fatalf("another person's log was read: %v", e)
		}
	}
	if fetcher.streams != 0 {
		t.Fatal("bytes were fetched for a caller who may not read the files")
	}

	reader := revisionActor(f.owner, auth.RoleReader)
	rows, err = f.second.handleReadFileExcerpts(reader, args, 0)
	entries := excerptEntries(t, rows, err)
	if entries[0]["label"] != "a" || entries[0]["excerpt"] != "ok\nFAIL first\n" || entries[0]["version"] != float64(1) ||
		entries[1]["label"] != "b" || entries[1]["excerpt"] != "FAIL second\n" {
		t.Fatalf("owner's logs: %v", entries)
	}

	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "supersedeLibraryFileHead", map[string]any{
		"fileId": first, "versionNumber": 2, "name": "step.log", "mimeType": "text/plain", "size": 13,
		"blobUrl": excerptBlobURL(f.owner, first), "format": "text",
	})
	fetcher.data[excerptBlobURL(f.owner, first)] = []byte("FAIL revised\n")
	rows, err = f.second.handleReadFileExcerpts(reader, args, 0)
	if entries = excerptEntries(t, rows, err); entries[0]["version"] != float64(2) || entries[0]["excerpt"] != "FAIL revised\n" {
		t.Fatalf("the reading replica kept the old head: %v", entries[0])
	}

	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "supersedeLibraryFileHead", map[string]any{
		"fileId": second, "versionNumber": 2, "name": "step.png", "mimeType": "image/png", "size": 13,
		"blobUrl": excerptBlobURL(f.owner, second), "format": "image",
	})
	before := fetcher.streams
	rows, err = f.second.handleReadFileExcerpts(reader, args, 0)
	entries = excerptEntries(t, rows, err)
	if entries[1]["unavailable"] != "this reader supports saved text files; use a format-specific reader for binary files" || fetcher.streams != before+1 {
		t.Fatalf("a binary file was fetched as text: %v, %d new streams", entries[1], fetcher.streams-before)
	}

	f.query(f.engine, f.ctx, "mutation", "archiveLibraryFile", map[string]any{"fileId": first})
	before = fetcher.streams
	rows, err = f.second.handleReadFileExcerpts(reader, args, 0)
	if entries = excerptEntries(t, rows, err); entries[0]["unavailable"] != "file is unavailable" || fetcher.streams != before {
		t.Fatalf("an archived file was read: %v", entries[0])
	}
}
