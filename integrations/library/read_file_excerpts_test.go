package library

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// urlBlobStreamer answers each stored URL with its own bytes and counts the
// streams it opened, so a test can say no bytes were read before authorization.
type urlBlobStreamer struct {
	data    map[string][]byte
	streams int
}

func (f *urlBlobStreamer) DownloadStreamURL(_ context.Context, url string) (io.ReadCloser, error) {
	data, ok := f.data[url]
	if !ok {
		return nil, fmt.Errorf("no object at %s", url)
	}
	f.streams++
	return io.NopCloser(bytes.NewReader(data)), nil
}

func excerptBlobURL(owner, fileID string) string {
	return "https://store.example/memql-library/library/" + owner + "/" + fileID + "/step.log"
}

// excerptStub seeds owner's text files on the library stub. A size in sizes
// replaces the stored size of that file; the bytes stay what files holds.
func excerptStub(owner string, files map[string][]byte, sizes map[string]int) (*Integration, *libStub, *urlBlobStreamer) {
	stub := newLibStub()
	fetcher := &urlBlobStreamer{data: map[string][]byte{}}
	for id, data := range files {
		stub.seedFile(id, owner, "step.log", "text/plain", "text")
		size := len(data)
		if s, ok := sizes[id]; ok {
			size = s
		}
		stub.files[id]["blobUrl"] = excerptBlobURL(owner, id)
		stub.files[id]["size"] = size
		stub.files[id]["versionNumber"] = 1
		fetcher.data[excerptBlobURL(owner, id)] = data
	}
	i := NewIntegration(stub)
	i.fileVersionGate = func(context.Context, string) (func(), error) { return func() {}, nil }
	i.SetBlobFetcher(fetcher)
	return i, stub, fetcher
}

func excerptEntries(t *testing.T, rows []memorynodes.MemoryNode, err error) []map[string]any {
	t.Helper()
	result := historyResult(t, rows, err)
	list, _ := result["files"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		entry, _ := item.(map[string]any)
		out = append(out, entry)
	}
	return out
}

func TestReadFileExcerptsRefusesMalformedInputBeforeAnyRead(t *testing.T) {
	i, stub, fetcher := excerptStub("user-a", map[string][]byte{"log-1": []byte("FAIL\n")}, nil)
	file := map[string]any{"fileId": "log-1", "label": "a"}
	nine := make([]any, 9)
	for k := range nine {
		nine[k] = file
	}
	many := make([]any, 33)
	for k := range many {
		many[k] = fmt.Sprintf("p%d", k)
	}
	patterns := []any{"FAIL"}
	for name, args := range map[string]map[string]any{
		"no files":     {"patterns": patterns, "maxBytes": 4096},
		"empty files":  {"files": []any{}, "patterns": patterns, "maxBytes": 4096},
		"nine files":   {"files": nine, "patterns": patterns, "maxBytes": 4096},
		"no fileId":    {"files": []any{map[string]any{"label": "a"}}, "patterns": patterns, "maxBytes": 4096},
		"no patterns":  {"files": []any{file}, "maxBytes": 4096},
		"33 patterns":  {"files": []any{file}, "patterns": many, "maxBytes": 4096},
		"long pattern": {"files": []any{file}, "patterns": []any{strings.Repeat("a", 257)}, "maxBytes": 4096},
		"invalid RE2":  {"files": []any{file}, "patterns": []any{"("}, "maxBytes": 4096},
		"1023 bytes":   {"files": []any{file}, "patterns": patterns, "maxBytes": 1023},
		"65537 bytes":  {"files": []any{file}, "patterns": patterns, "maxBytes": 65537},
		"no budget":    {"files": []any{file}, "patterns": patterns},
	} {
		if _, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), args, 0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	valid := map[string]any{"files": []any{file}, "patterns": patterns, "maxBytes": 4096}
	if _, err := i.handleReadFileExcerpts(context.Background(), valid, 0); err == nil {
		t.Error("an unsigned caller was answered")
	}
	if fetcher.streams != 0 || len(stub.calls) != 0 {
		t.Fatalf("read before the input was accepted: %d streams, calls %v", fetcher.streams, stub.calls)
	}
}

func TestReadFileExcerptsSharesOneBudgetAndKeepsInputOrder(t *testing.T) {
	long := func(tag string) []byte {
		return []byte(lines(3500, func(n int) string { return fmt.Sprintf("%s line %04d of a long log", tag, n) }))
	}
	small := []byte(lines(10, func(n int) string { return fmt.Sprintf("small %d", n) }))
	// big-a's stored size is wrong: it says 0 for 100 KiB of bytes.
	i, _, fetcher := excerptStub("user-a", map[string][]byte{"big-a": long("a"), "small": small, "big-b": long("b")}, map[string]int{"big-a": 0})
	rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), map[string]any{
		"files": []any{
			map[string]any{"fileId": "big-a", "label": "a"},
			map[string]any{"fileId": "small", "label": "s"},
			map[string]any{"fileId": "big-b", "label": "b"},
		},
		"patterns": []any{"FAIL"},
		"maxBytes": 32768,
	}, 0)
	entries := excerptEntries(t, rows, err)
	if len(entries) != 3 || entries[0]["label"] != "a" || entries[1]["label"] != "s" || entries[2]["label"] != "b" {
		t.Fatalf("results not in input order: %v", entries)
	}
	if entries[1]["complete"] != true || entries[1]["excerpt"] != string(small) {
		t.Fatalf("the small file did not come back whole: %v", entries[1])
	}
	total := 0
	for _, e := range entries {
		total += len(e["excerpt"].(string))
	}
	a, b := len(entries[0]["excerpt"].(string)), len(entries[2]["excerpt"].(string))
	if total > 32768 || a < 10000 || b < 10000 || fetcher.streams != 3 {
		t.Fatalf("budget not shared: total %d, a %d, b %d, streams %d", total, a, b, fetcher.streams)
	}
}

func TestReadFileExcerptsReportsAnUnavailableFileWithoutFailingTheCall(t *testing.T) {
	i, stub, fetcher := excerptStub("user-a", map[string][]byte{"mine": []byte("FAIL here\n")}, nil)
	stub.seedFile("theirs", "user-b", "step.log", "text/plain", "text")
	rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), map[string]any{
		"files":    []any{map[string]any{"fileId": "mine", "label": "m"}, map[string]any{"fileId": "theirs", "label": "t"}},
		"patterns": []any{"FAIL"},
		"maxBytes": 4096,
	}, 0)
	entries := excerptEntries(t, rows, err)
	if len(entries) != 2 || entries[0]["excerpt"] != "FAIL here\n" || entries[0]["version"] != float64(1) {
		t.Fatalf("readable file: %v", entries)
	}
	if entries[1]["unavailable"] != "file is unavailable" || entries[1]["excerpt"] != nil || entries[1]["label"] != "t" {
		t.Fatalf("another person's file: %v", entries[1])
	}
	if fetcher.streams != 1 {
		t.Fatalf("%d streams opened; only the readable file's bytes may be read", fetcher.streams)
	}
}
