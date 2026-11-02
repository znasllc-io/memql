package library

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// urlBlobStreamer answers each stored URL with its own bytes and counts the
// streams it opened, so a test can say no bytes were read before authorization.
type urlBlobStreamer struct {
	data    map[string][]byte
	streams int
	onOpen  func()
	// answers replaces what storage does for one stored URL: a failure, or a
	// stream that fails partway.
	answers map[string]blobAnswer
}

type blobAnswer func(context.Context) (io.ReadCloser, error)

func (f *urlBlobStreamer) DownloadStreamURL(ctx context.Context, url string) (io.ReadCloser, error) {
	if answer, ok := f.answers[url]; ok {
		return answer(ctx)
	}
	data, ok := f.data[url]
	if !ok {
		return nil, fmt.Errorf("no object at %s", url)
	}
	f.streams++
	if f.onOpen != nil {
		f.onOpen()
	}
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

// openFails is storage refusing to open a file; readFails is a stream that
// breaks after its first line.
func openFails(err error) blobAnswer {
	return func(context.Context) (io.ReadCloser, error) { return nil, err }
}

func readFails(err error) blobAnswer {
	return func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(strings.NewReader("FAIL partial\n"), iotest.ErrReader(err))), nil
	}
}

// storageAnswered is the error the Azure SDK returns, as DownloadStreamURL wraps it.
func storageAnswered(status int, code string) error {
	return fmt.Errorf("open azure blob stream: %w", &azcore.ResponseError{StatusCode: status, ErrorCode: code})
}

func excerptArgs(ids ...string) map[string]any {
	files := make([]any, 0, len(ids))
	for _, id := range ids {
		files = append(files, map[string]any{"fileId": id, "label": "label of " + id})
	}
	return map[string]any{"files": files, "patterns": []any{"FAIL"}, "maxBytes": 4096}
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
		// Past the per-file floor a file's share could come back empty.
		"two files in 1 KiB":   {"files": []any{file, file}, "patterns": patterns, "maxBytes": 1024},
		"eight files in 7 KiB": {"files": nine[:8], "patterns": patterns, "maxBytes": 7 << 10},
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

// The version gate covers the row reads only: a version's bytes never change,
// and a scan that held the gate would hold a gate-pool connection for its length.
func TestReadFileExcerptsReleasesTheGateBeforeReadingBytes(t *testing.T) {
	i, _, fetcher := excerptStub("user-a", map[string][]byte{"log-1": []byte("FAIL\n")}, nil)
	held := false
	i.fileVersionGate = func(context.Context, string) (func(), error) {
		held = true
		return func() { held = false }, nil
	}
	fetcher.onOpen = func() {
		if held {
			t.Error("bytes were opened while the file's version gate was held")
		}
	}
	rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), map[string]any{
		"files": []any{map[string]any{"fileId": "log-1", "label": "a"}}, "patterns": []any{"FAIL"}, "maxBytes": 4096,
	}, 0)
	if entries := excerptEntries(t, rows, err); entries[0]["excerpt"] != "FAIL\n" || held {
		t.Fatalf("excerpt %v, gate held after the call: %v", entries[0], held)
	}
}

func TestReadFileExcerptsReportsAFileThatRanOutOfTime(t *testing.T) {
	i, _, _ := excerptStub("user-a", map[string][]byte{"log-1": []byte("FAIL one\n"), "log-2": []byte("FAIL two\n")}, nil)
	defer func(d time.Duration) { excerptScanTime = d }(excerptScanTime)
	excerptScanTime = time.Nanosecond
	rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), map[string]any{
		"files":    []any{map[string]any{"fileId": "log-1", "label": "a"}, map[string]any{"fileId": "log-2", "label": "b"}},
		"patterns": []any{"FAIL"}, "maxBytes": 4096,
	}, 0)
	for _, e := range excerptEntries(t, rows, err) {
		if e["unavailable"] != "this file could not be excerpted in time" || e["excerpt"] != nil {
			t.Fatalf("a scan past its time answered %v", e)
		}
	}
}

// The floor's other side: every file count has a budget, and every file of it
// gets an excerpt it can be told apart by.
func TestReadFileExcerptsAtTheFloorGivesEveryFileAnExcerpt(t *testing.T) {
	files := map[string][]byte{}
	ids := make([]string, 0, excerptMaxFiles)
	for k := range excerptMaxFiles {
		id := fmt.Sprintf("log-%d", k)
		ids = append(ids, id)
		files[id] = []byte(lines(4000, func(n int) string { return fmt.Sprintf("%s line %d", id, n) }))
	}
	i, _, _ := excerptStub("user-a", files, nil)
	args := excerptArgs(ids...)
	args["maxBytes"] = excerptMaxFiles * excerptMinBytes
	rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), args, 0)
	entries := excerptEntries(t, rows, err)
	total := 0
	for k, e := range entries {
		text, _ := e["excerpt"].(string)
		total += len(text)
		if !strings.HasSuffix(text, fmt.Sprintf("%s line 4000\n", ids[k])) {
			t.Errorf("%s came back without its end: %q", ids[k], text)
		}
	}
	if total > excerptMaxFiles*excerptMinBytes {
		t.Fatalf("%d excerpt bytes in a budget of %d", total, excerptMaxFiles*excerptMinBytes)
	}
}

// A failure storage may get over says nothing about the file, so it is not
// reported as a fact about the file: the recipe would read every log as gone
// and end the diagnosis for good. The call fails in words the failure path
// retries, and the cause goes to the log with the file id. The id holds digits
// the symptom table also reads, and the cause's own text never reaches it: a
// 404 in a blob's path or a request id must not decide what the run does next.
func TestReadFileExcerptsFailsTheCallWhenStorageFails(t *testing.T) {
	const failing = "log-404-403"
	for name, answer := range map[string]blobAnswer{
		"the open answers 500":                openFails(storageAnswered(500, "InternalError")),
		"the open answers 503":                openFails(storageAnswered(503, "ServerBusy")),
		"storage refuses this node":           openFails(storageAnswered(403, "AuthorizationFailure")),
		"the whole container is missing":      openFails(storageAnswered(404, "ContainerNotFound")),
		"a 404 with no service code":          openFails(storageAnswered(404, "")),
		"the connection drops":                openFails(errors.New("read tcp 10.0.0.1:443: connection reset by peer")),
		"a 404 only in the text of a 500":     openFails(errors.New("GET https://acct.blob.core.windows.net/memql-library/library/404/x: RESPONSE 500")),
		"the stream breaks partway":           readFails(io.ErrUnexpectedEOF),
		"storage times out with time in hand": readFails(context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			i, _, fetcher := excerptStub("user-a", map[string][]byte{failing: []byte("FAIL\n"), "log-2": []byte("FAIL two\n")}, nil)
			fetcher.answers = map[string]blobAnswer{excerptBlobURL("user-a", failing): answer}
			var logged bytes.Buffer
			i.SetLogger(slog.New(slog.NewTextHandler(&logged, nil)))
			rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), excerptArgs(failing, "log-2"), 0)
			if err == nil {
				t.Fatalf("a storage failure was answered as a result: %v", rows)
			}
			if code, terminal := work.TerminalFailureCode(err.Error()); terminal {
				t.Fatalf("%q ends the run as %s", err, code)
			}
			symptom, _, ok := work.ClassifyByRules(work.Signal{ErrorMessage: err.Error()})
			if !ok || symptom != work.SymptomTransient || work.ActFor(symptom, 0, work.DefaultMaxRetries) != work.ActRetry {
				t.Fatalf("%q is classified %q (ok=%v), which does not retry", err, symptom, ok)
			}
			if !strings.Contains(err.Error(), failing) || strings.Contains(err.Error(), "blob.core.windows.net") {
				t.Fatalf("%q should name the file and leave the storage error to the log", err)
			}
			if !strings.Contains(logged.String(), "fileId="+failing) {
				t.Fatalf("the failure was not logged with its file id: %q", logged.String())
			}
		})
	}
}

// A blob that is really gone is a fact about the file, and no attempt changes
// it: it stays one entry, and the other logs still reach the diagnosis.
func TestReadFileExcerptsReportsAnAbsentBlobInItsOwnEntry(t *testing.T) {
	for name, absent := range map[string]error{
		"retired":   azureblob.ErrBlobRetired,
		"not found": storageAnswered(404, "BlobNotFound"),
	} {
		t.Run(name, func(t *testing.T) {
			i, _, fetcher := excerptStub("user-a", map[string][]byte{"gone": []byte("FAIL\n"), "log-2": []byte("FAIL two\n")}, nil)
			fetcher.answers = map[string]blobAnswer{excerptBlobURL("user-a", "gone"): openFails(absent)}
			var logged bytes.Buffer
			i.SetLogger(slog.New(slog.NewTextHandler(&logged, nil)))
			rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), excerptArgs("gone", "log-2"), 0)
			entries := excerptEntries(t, rows, err)
			if entries[0]["unavailable"] != "could not read this saved version" || entries[0]["excerpt"] != nil || entries[0]["label"] != "label of gone" {
				t.Fatalf("absent blob: %v", entries[0])
			}
			if entries[1]["excerpt"] != "FAIL two\n" {
				t.Fatalf("the other file was lost: %v", entries[1])
			}
			if !strings.Contains(logged.String(), "fileId=gone") {
				t.Fatalf("the absence was not logged with its file id: %q", logged.String())
			}
		})
	}
}

// A chunked upload's row stores a bare key, which the URL reader can never
// open, so a retry cannot change this answer either. The real adapter is what
// says so: nothing here reaches its storage client.
func TestReadFileExcerptsReportsABareKeyInItsOwnEntry(t *testing.T) {
	i, stub, _ := excerptStub("user-a", map[string][]byte{"chunked": []byte("FAIL\n")}, nil)
	stub.files["chunked"]["blobUrl"] = "library/user-a/chunked/step.log"
	i.SetBlobFetcher(&azureblob.AzureBlobUploader{})
	rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), excerptArgs("chunked"), 0)
	if entries := excerptEntries(t, rows, err); entries[0]["unavailable"] != "could not read this saved version" {
		t.Fatalf("bare key: %v", entries[0])
	}
}

// openFileBytes refuses a stored key outside its owner's Library before storage
// is asked. That is a fact about the row, so it stays one entry too.
func TestReadFileExcerptsReportsARefusedKeyInItsOwnEntry(t *testing.T) {
	i, stub, fetcher := excerptStub("user-a", map[string][]byte{"foreign": []byte("FAIL\n"), "log-2": []byte("FAIL two\n")}, nil)
	stub.files["foreign"]["blobUrl"] = excerptBlobURL("user-b", "foreign")
	var logged bytes.Buffer
	i.SetLogger(slog.New(slog.NewTextHandler(&logged, nil)))
	rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), excerptArgs("foreign", "log-2"), 0)
	entries := excerptEntries(t, rows, err)
	if entries[0]["unavailable"] != "could not read this saved version" || entries[1]["excerpt"] != "FAIL two\n" || fetcher.streams != 1 {
		t.Fatalf("refused key: %v, %d streams", entries, fetcher.streams)
	}
	if !strings.Contains(logged.String(), "fileId=foreign") {
		t.Fatalf("the refusal was not logged with its file id: %q", logged.String())
	}
}

// The scan's own deadline stays a per-file answer wherever it lands: another
// attempt gets the same two minutes. Only the deadline having really passed
// counts, which is what tells it from storage's own timeout above.
func TestReadFileExcerptsReportsADeadlineThatStopsStorage(t *testing.T) {
	defer func(d time.Duration) { excerptScanTime = d }(excerptScanTime)
	excerptScanTime = 20 * time.Millisecond
	for name, answer := range map[string]blobAnswer{
		"while opening": func(ctx context.Context) (io.ReadCloser, error) {
			<-ctx.Done()
			return nil, fmt.Errorf("open azure blob stream: %w", ctx.Err())
		},
		"while reading": func(ctx context.Context) (io.ReadCloser, error) {
			return io.NopCloser(readerFunc(func([]byte) (int, error) {
				<-ctx.Done()
				return 0, errors.New("stream closed by its context")
			})), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			i, _, fetcher := excerptStub("user-a", map[string][]byte{"log-1": []byte("FAIL\n")}, nil)
			fetcher.answers = map[string]blobAnswer{excerptBlobURL("user-a", "log-1"): answer}
			var logged bytes.Buffer
			i.SetLogger(slog.New(slog.NewTextHandler(&logged, nil)))
			rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), excerptArgs("log-1"), 0)
			if entries := excerptEntries(t, rows, err); entries[0]["unavailable"] != "this file could not be excerpted in time" {
				t.Fatalf("a scan past its time answered %v", entries[0])
			}
			if !strings.Contains(logged.String(), "fileId=log-1") {
				t.Fatalf("storage stopping the scan was not logged with the file id: %q", logged.String())
			}
		})
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// A caller who left gets no answer at all, however storage worded it.
func TestReadFileExcerptsStopsWhenItsCallerLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(revisionActor("user-a", auth.RoleReader))
	defer cancel()
	i, _, fetcher := excerptStub("user-a", map[string][]byte{"log-1": []byte("FAIL\n")}, nil)
	fetcher.answers = map[string]blobAnswer{excerptBlobURL("user-a", "log-1"): func(context.Context) (io.ReadCloser, error) {
		cancel()
		return nil, azureblob.ErrBlobRetired
	}}
	if rows, err := i.handleReadFileExcerpts(ctx, excerptArgs("log-1"), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller who left was answered: %v, %v", rows, err)
	}
}

// Identifiers leave as bare ids at every wire seam, whichever answer the file
// got (docs/public/concepts/identifiers.md).
func TestReadFileExcerptsAnswersAnUnavailableFileWithItsBareId(t *testing.T) {
	i, _, _ := excerptStub("user-a", nil, nil)
	rows, err := i.handleReadFileExcerpts(revisionActor("user-a", auth.RoleReader), excerptArgs(conceptFile+":gone"), 0)
	if entries := excerptEntries(t, rows, err); entries[0]["fileId"] != "gone" || entries[0]["unavailable"] != "file is unavailable" {
		t.Fatalf("unavailable entry: %v", entries[0])
	}
}
