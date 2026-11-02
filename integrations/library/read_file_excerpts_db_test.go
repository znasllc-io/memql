package library

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// testReadFileExcerptsAcrossEngines writes two pipeline logs on one engine and
// reads their excerpts from the other, which shares nothing with it but the
// database: authorization comes before any byte is fetched, a revised head is
// read as revised, and a binary or archived file is reported without a fetch.
// A malformed call refuses before any byte is read, storage that may recover
// fails the call while a blob that is gone stays one entry, and every entry
// answers with the bare id however the caller named the file.
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
	file := map[string]any{"fileId": first, "label": "a"}
	for name, bad := range map[string]map[string]any{
		"no files":           {"patterns": args["patterns"], "maxBytes": 4096},
		"no fileId":          {"files": []any{map[string]any{"label": "a"}}, "patterns": args["patterns"], "maxBytes": 4096},
		"no patterns":        {"files": []any{file}, "maxBytes": 4096},
		"invalid RE2":        {"files": []any{file}, "patterns": []any{"("}, "maxBytes": 4096},
		"a budget too small": {"files": []any{file}, "patterns": args["patterns"], "maxBytes": 1023},
		"a budget too large": {"files": []any{file}, "patterns": args["patterns"], "maxBytes": 65537},
		"two files in 1 KiB": {"files": args["files"], "patterns": args["patterns"], "maxBytes": 1024},
	} {
		if rows, err := f.second.handleReadFileExcerpts(reader, bad, 0); err == nil {
			t.Fatalf("%s: a malformed call was answered: %v", name, rows)
		}
	}
	if fetcher.streams != 0 {
		t.Fatalf("a malformed call read %d files' bytes", fetcher.streams)
	}

	rows, err = f.second.handleReadFileExcerpts(reader, args, 0)
	entries := excerptEntries(t, rows, err)
	if entries[0]["label"] != "a" || entries[0]["excerpt"] != "ok\nFAIL first\n" || entries[0]["version"] != float64(1) ||
		entries[1]["label"] != "b" || entries[1]["excerpt"] != "FAIL second\n" {
		t.Fatalf("owner's logs: %v", entries)
	}

	// A caller may name a file by its canonical id. Every entry answers bare.
	named := map[string]any{"files": []any{
		map[string]any{"fileId": conceptFile + ":" + first, "label": "readable"},
		map[string]any{"fileId": conceptFile + ":" + f.owner + "-log-none", "label": "no row"},
		map[string]any{"fileId": conceptFile + ":" + second, "label": "gone"},
	}, "patterns": args["patterns"], "maxBytes": 4096}
	fetcher.answers = map[string]blobAnswer{excerptBlobURL(f.owner, second): openFails(azureblob.ErrBlobRetired)}
	rows, err = f.second.handleReadFileExcerpts(reader, named, 0)
	entries = excerptEntries(t, rows, err)
	if entries[0]["fileId"] != first || entries[1]["fileId"] != f.owner+"-log-none" || entries[2]["fileId"] != second {
		t.Fatalf("not every entry answered with its bare id: %v", entries)
	}
	if entries[0]["excerpt"] != "ok\nFAIL first\n" || entries[2]["unavailable"] != "could not read this saved version" {
		t.Fatalf("a retired log cost the other its result: %v", entries)
	}

	// Storage that may recover fails the whole call on the reading replica, in
	// words the failure path retries, and the next attempt reads the log.
	fetcher.answers = map[string]blobAnswer{excerptBlobURL(f.owner, second): readFails(storageAnswered(503, "ServerBusy"))}
	_, err = f.second.handleReadFileExcerpts(reader, args, 0)
	if err == nil || !strings.Contains(err.Error(), second) {
		t.Fatalf("a storage failure was not the call's: %v", err)
	}
	if symptom, _, ok := work.ClassifyByRules(work.Signal{ErrorMessage: err.Error()}); !ok || symptom != work.SymptomTransient {
		t.Fatalf("%q is classified %q, which does not retry", err, symptom)
	}
	fetcher.answers = nil
	rows, err = f.second.handleReadFileExcerpts(reader, args, 0)
	if entries = excerptEntries(t, rows, err); entries[1]["excerpt"] != "FAIL second\n" {
		t.Fatalf("the retry did not read the log: %v", entries[1])
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
