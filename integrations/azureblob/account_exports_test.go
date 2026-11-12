package azureblob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

// memoryBlobService is the block-blob protocol the export store speaks, held in
// memory: staged blocks per object, a committed body, ranged reads, deletes and
// a flat listing. It answers the SDK the way the service does, error codes
// included, so what passes here passes over the SDK's own request building.
type memoryBlobService struct {
	mu        sync.Mutex
	committed map[string][]byte
	staged    map[string]map[string][]byte
	maxBlock  int
	stages    int
	// retired objects carry the retirement marker, as a tombstone does.
	retired map[string]bool
	// listExtra is listed whatever prefix is asked for: a listing that
	// answers more than it should, which the reader must not pass on.
	listExtra []string
}

func newMemoryBlobService() *memoryBlobService {
	return &memoryBlobService{committed: map[string][]byte{}, staged: map[string]map[string][]byte{}, retired: map[string]bool{}}
}

func (m *memoryBlobService) put(container, object string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.committed[container+"/"+object] = body
}

func (m *memoryBlobService) has(container, object string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.committed[container+"/"+object]
	return ok
}

func (m *memoryBlobService) stagedCount(container, object string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.staged[container+"/"+object])
}

func (m *memoryBlobService) do(r *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	respond := func(status int, headers http.Header, body []byte) (*http.Response, error) {
		if headers == nil {
			headers = http.Header{}
		}
		headers.Set("Content-Length", strconv.Itoa(len(body)))
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: headers,
			Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
	}
	missing := func(code string) (*http.Response, error) {
		return respond(http.StatusNotFound, http.Header{"X-Ms-Error-Code": {code}}, nil)
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	q := r.URL.Query()
	if q.Get("restype") == "container" && q.Get("comp") == "list" {
		prefix := path + "/" + q.Get("prefix")
		var names []string
		for key := range m.committed {
			if strings.HasPrefix(key, prefix) {
				names = append(names, key)
			}
		}
		sort.Strings(names)
		for _, name := range m.listExtra {
			names = append(names, path+"/"+name)
		}
		var out strings.Builder
		out.WriteString(`<?xml version="1.0" encoding="utf-8"?><EnumerationResults><Blobs>`)
		for _, key := range names {
			name := strings.TrimPrefix(key, path+"/")
			meta := ""
			if m.retired[key] && strings.Contains(q.Get("include"), "metadata") {
				meta = `<Metadata><memql_retired_stream>marker</memql_retired_stream></Metadata>`
			}
			fmt.Fprintf(&out, `<Blob><Name>%s</Name><Properties><Content-Length>%d</Content-Length><BlobType>BlockBlob</BlobType></Properties>%s</Blob>`, name, len(m.committed[key]), meta)
		}
		out.WriteString(`</Blobs><NextMarker/></EnumerationResults>`)
		return respond(http.StatusOK, http.Header{"Content-Type": {"application/xml"}}, []byte(out.String()))
	}
	key := path
	switch {
	case r.Method == http.MethodPut && q.Get("comp") == "block":
		chunk, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if len(chunk) > m.maxBlock {
			m.maxBlock = len(chunk)
		}
		id := q.Get("blockid")
		if _, err := base64.StdEncoding.DecodeString(id); err != nil {
			return respond(http.StatusBadRequest, http.Header{"X-Ms-Error-Code": {"InvalidQueryParameterValue"}}, nil)
		}
		for other := range m.staged[key] {
			if len(other) != len(id) {
				return respond(http.StatusBadRequest, http.Header{"X-Ms-Error-Code": {"InvalidBlobOrBlock"}}, nil)
			}
		}
		if m.staged[key] == nil {
			m.staged[key] = map[string][]byte{}
		}
		m.staged[key][id] = chunk
		m.stages++
		return respond(http.StatusCreated, nil, nil)
	case r.Method == http.MethodPut && q.Get("comp") == "blocklist":
		var list struct{ Latest []string }
		if err := xml.NewDecoder(r.Body).Decode(&list); err != nil {
			return nil, err
		}
		var body []byte
		for _, id := range list.Latest {
			chunk, ok := m.staged[key][id]
			if !ok {
				return respond(http.StatusBadRequest, http.Header{"X-Ms-Error-Code": {"InvalidBlockList"}}, nil)
			}
			body = append(body, chunk...)
		}
		m.committed[key] = body
		delete(m.staged, key)
		return respond(http.StatusCreated, http.Header{"Etag": {`"v1"`}}, nil)
	case r.Method == http.MethodHead:
		body, ok := m.committed[key]
		if !ok {
			return missing("BlobNotFound")
		}
		resp, err := respond(http.StatusOK, http.Header{"Etag": {`"v1"`}, "X-Ms-Blob-Type": {"BlockBlob"}}, nil)
		if m.retired[key] {
			resp.Header.Set("X-Ms-Meta-Memql_retired_stream", "marker")
		}
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return resp, err
	case r.Method == http.MethodGet:
		body, ok := m.committed[key]
		if !ok {
			return missing("BlobNotFound")
		}
		// The SDK sets x-ms-range without canonicalizing the header name.
		var rng string
		for name, values := range r.Header {
			if (strings.EqualFold(name, "x-ms-range") || strings.EqualFold(name, "Range")) && len(values) > 0 {
				rng = values[0]
			}
		}
		if rng == "" {
			headers := http.Header{"Etag": {`"v1"`}}
			if m.retired[key] {
				headers.Set("X-Ms-Meta-Memql_retired_stream", "marker")
			}
			return respond(http.StatusOK, headers, bytes.Clone(body))
		}
		var start, end int
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
			return nil, fmt.Errorf("unexpected range %q", rng)
		}
		end = min(end, len(body)-1)
		return respond(http.StatusPartialContent, http.Header{"Etag": {`"v1"`}}, bytes.Clone(body[start:end+1]))
	case r.Method == http.MethodDelete:
		if _, ok := m.committed[key]; !ok {
			return missing("BlobNotFound")
		}
		delete(m.committed, key)
		delete(m.staged, key)
		return respond(http.StatusAccepted, nil, nil)
	}
	return nil, fmt.Errorf("unexpected blob request: %s %s", r.Method, r.URL)
}

const exportTestContainer = "memql"

func exportTestClient(t *testing.T, m *memoryBlobService) *AzureBlobUploader {
	t.Helper()
	c, err := azblob.NewClientWithNoCredential("https://blob.test", &azblob.ClientOptions{ClientOptions: policy.ClientOptions{
		Transport: downloadTransport(m.do), Retry: policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &AzureBlobUploader{client: c, libraryContainer: exportTestContainer}
}

// patterned is deterministic, non-repeating-at-block-size content, so a block
// committed out of order or twice changes the checksum.
func patterned(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7 + i/8191)
	}
	return out
}

func TestAccountExportStreamCommitsSizeAndChecksum(t *testing.T) {
	ctx := context.Background()
	m := newMemoryBlobService()
	store := exportTestClient(t, m).AccountExports()
	const object = "exports/accounts/exp-1/account.memqlx"
	body := patterned(20<<20 + 5)

	w, err := store.Create(ctx, object)
	if err != nil {
		t.Fatal(err)
	}
	// Odd write sizes, so block boundaries fall inside a Write.
	for rest := body; len(rest) > 0; {
		n := min(len(rest), 3<<20+17)
		if got, err := w.Write(rest[:n]); err != nil || got != n {
			t.Fatalf("write: %d %v", got, err)
		}
		rest = rest[n:]
	}
	size, digest, err := w.Commit(ctx)
	sum := sha256.Sum256(body)
	if err != nil || size != int64(len(body)) || digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("commit: size=%d digest=%s err=%v", size, digest, err)
	}
	if m.maxBlock > exportBlockBytes || m.stages != 3 {
		t.Fatalf("blocks: largest=%d staged=%d, want at most %d bytes in 3 blocks", m.maxBlock, m.stages, exportBlockBytes)
	}
	r, err := store.Open(ctx, object)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read back: %d bytes, %v", len(got), err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("a committed stream accepted another write")
	}
}

func TestAccountExportAbortLeavesNoObject(t *testing.T) {
	ctx := context.Background()
	m := newMemoryBlobService()
	store := exportTestClient(t, m).AccountExports()
	const object = "exports/accounts/exp-2/account.memqlx"
	w, err := store.Create(ctx, object)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(patterned(9 << 20)); err != nil {
		t.Fatal(err)
	}
	if m.stagedCount(exportTestContainer, object) == 0 {
		t.Fatal("no block was staged before the abort")
	}
	if err := w.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if m.has(exportTestContainer, object) || m.stagedCount(exportTestContainer, object) != 0 {
		t.Fatal("abort left an object or staged blocks")
	}
	if _, _, err := w.Commit(ctx); err == nil {
		t.Fatal("an aborted stream committed")
	}
	if _, err := store.Open(ctx, object); !IsBlobAbsent(err) {
		t.Fatalf("open after abort: %v", err)
	}
}

func TestAccountExportRefusesKeysOutsideItsPrefix(t *testing.T) {
	ctx := context.Background()
	m := newMemoryBlobService()
	m.put(exportTestContainer, "library/u1/f1/report.pdf", []byte("private"))
	store := exportTestClient(t, m).AccountExports()
	for _, object := range []string{
		"", "library/u1/f1/report.pdf", "exports/accounts/", "exports/accounts", "exports/other/x",
		"exports/accounts/../../library/u1/f1/report.pdf", "exports/accounts/a/../../x", "/exports/accounts/x",
		" exports/accounts/x", "exports/accounts/x ", "exports/accounts//x", "exports/accounts/x\\y",
		"exports/accounts/x\x00", "https://blob.test/memql/exports/accounts/x", "exports/accounts/./x",
	} {
		if _, err := store.Create(ctx, object); !errors.Is(err, ErrNotAccountExport) {
			t.Errorf("Create(%q): %v", object, err)
		}
		if _, err := store.Open(ctx, object); !errors.Is(err, ErrNotAccountExport) {
			t.Errorf("Open(%q): %v", object, err)
		}
		if _, err := store.OpenRange(ctx, object, 0, 1); !errors.Is(err, ErrNotAccountExport) {
			t.Errorf("OpenRange(%q): %v", object, err)
		}
		if err := store.Delete(ctx, object); !errors.Is(err, ErrNotAccountExport) {
			t.Errorf("Delete(%q): %v", object, err)
		}
	}
	if !m.has(exportTestContainer, "library/u1/f1/report.pdf") {
		t.Fatal("a refused delete reached storage")
	}
}

func TestAccountExportOpenRangeAndDelete(t *testing.T) {
	ctx := context.Background()
	m := newMemoryBlobService()
	const object = "exports/accounts/exp-3/account.memqlx"
	m.put(exportTestContainer, object, []byte("0123456789"))
	store := exportTestClient(t, m).AccountExports()

	r, err := store.OpenRange(ctx, object, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != "3456" {
		t.Fatalf("range: %q", got)
	}
	if _, err := store.OpenRange(ctx, object, -1, 4); err == nil {
		t.Fatal("a negative offset was accepted")
	}
	if _, err := store.OpenRange(ctx, object, 0, 0); err == nil {
		t.Fatal("an empty range was accepted")
	}
	if err := store.Delete(ctx, object); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, object); !IsBlobAbsent(err) {
		t.Fatalf("open after delete: %v", err)
	}
	// A second delete finds nothing and says so as success: the file is gone.
	if err := store.Delete(ctx, object); err != nil {
		t.Fatalf("delete of an absent export: %v", err)
	}
}

// The places a row's bytes may live in, as the export run derives them from a
// concept's @bytes(under=...) and the row's own values.
var (
	libraryPlace = []string{"library/u1"}
	spacesPlace  = []string{"spaces/s1"}
	sitePlace    = []string{"sites/site-1"}
)

func TestAccountBytesStat(t *testing.T) {
	ctx := context.Background()
	m := newMemoryBlobService()
	m.put(exportTestContainer, "library/u1/f1/report.pdf", []byte("pdf-bytes"))
	m.put(exportTestContainer, "library/u1/f1/v2/report.pdf", []byte("v2"))
	m.put(exportTestContainer, "library/u1/f1/old.pdf", []byte("retired"))
	m.retired[exportTestContainer+"/library/u1/f1/old.pdf"] = true
	m.put(exportTestContainer, "library/u2/f9/theirs.pdf", []byte("someone else's"))
	m.put(exportTestContainer, "spaces/s1/attachments/a1/note.txt", []byte("note"))
	m.put(exportTestContainer, "exports/accounts/e1/account.memqlx", []byte("an export"))
	m.put("elsewhere", "library/u1/f1/report.pdf", []byte("other container"))
	reader := exportTestClient(t, m).AccountBytes()

	for _, tc := range []struct {
		name, ref string
		under     []string
		key       string
		size      int64
		ok        bool
		shipped   bool
		wantErr   bool
	}{
		{name: "stored URL", ref: "https://blob.test/memql/library/u1/f1/report.pdf", under: libraryPlace, key: "library/u1/f1/report.pdf", size: 9, ok: true},
		{name: "bare library key", ref: "library/u1/f1/report.pdf", under: libraryPlace, key: "library/u1/f1/report.pdf", size: 9, ok: true},
		{name: "bare attachment key", ref: "spaces/s1/attachments/a1/note.txt", under: spacesPlace, key: "spaces/s1/attachments/a1/note.txt", size: 4, ok: true},
		{name: "missing object", ref: "library/u1/f2/gone.pdf", under: libraryPlace},
		{name: "missing via URL", ref: "https://blob.test/memql/library/u1/f2/gone.pdf", under: libraryPlace},
		{name: "retired object", ref: "library/u1/f1/old.pdf", under: libraryPlace},
		{name: "shipped in the image", ref: "file:///app/os", under: sitePlace, shipped: true},
		{name: "tree skips a retired object", ref: "blob://library/u1/f1/", under: libraryPlace, key: "library/u1/f1/", size: 11, ok: true},
		{name: "tree without its slash", ref: "blob://library/u1/f1", under: libraryPlace, key: "library/u1/f1/", size: 11, ok: true},
		{name: "empty tree", ref: "blob://sites/site-1/v000000000000/", under: sitePlace, key: "sites/site-1/v000000000000/"},

		{name: "outside its place", ref: "library/u2/f9/theirs.pdf", under: libraryPlace, wantErr: true},
		{name: "its place itself as a tree", ref: "blob://library/u1", under: libraryPlace, wantErr: true},
		{name: "a sibling sharing a prefix string", ref: "library/u10/f1/x", under: libraryPlace, wantErr: true},
		{name: "no place given", ref: "library/u1/f1/report.pdf", wantErr: true},
		{name: "an empty place", ref: "library/u1/f1/report.pdf", under: []string{""}, wantErr: true},
		{name: "another container", ref: "https://blob.test/elsewhere/library/u1/f1/report.pdf", under: libraryPlace, wantErr: true},
		{name: "escape", ref: "library/u1/../../etc/passwd", under: libraryPlace, wantErr: true},
		{name: "dot segment", ref: "library/u1/./f1/report.pdf", under: libraryPlace, wantErr: true},
		{name: "empty segment", ref: "library/u1//f1/report.pdf", under: libraryPlace, wantErr: true},
		{name: "leading slash", ref: "/library/u1/f1/report.pdf", under: libraryPlace, wantErr: true},
		{name: "encoded slash in a key", ref: "library/u1%2Ff1/report.pdf", under: libraryPlace, wantErr: true},
		{name: "encoded slash in a URL", ref: "https://blob.test/memql/library/u1%2Ff1%2Freport.pdf", under: libraryPlace, wantErr: true},
		{name: "encoded dot segment in a URL", ref: "https://blob.test/memql/library/u1/%2E%2E/u2/f9/theirs.pdf", under: libraryPlace, wantErr: true},
		{name: "escape in a tree", ref: "blob://sites/../library/", under: sitePlace, wantErr: true},
		{name: "an export key", ref: "exports/accounts/e1/account.memqlx", under: []string{"exports/accounts"}, wantErr: true},
		{name: "an export tree", ref: "blob://exports", under: []string{"exports"}, wantErr: true},
		{name: "an export tree with its slash", ref: "blob://exports/", under: []string{"exports"}, wantErr: true},
		{name: "exports spelled otherwise", ref: "EXPORTS/accounts/e1/account.memqlx", under: []string{"EXPORTS"}, wantErr: true},
		{name: "exports climbed into", ref: "blob://EXPORTS/..", under: []string{"EXPORTS"}, wantErr: true},
		{name: "exports by URL", ref: "https://blob.test/memql/exports/accounts/e1/account.memqlx", under: []string{"exports/accounts"}, wantErr: true},
		{name: "a top-level tree", ref: "blob://library", under: []string{"library"}, wantErr: true},
		{name: "a top-level tree with its slash", ref: "blob://sites/", under: []string{"sites"}, wantErr: true},
		{name: "blank", ref: "  ", under: libraryPlace, wantErr: true},
		{name: "unknown scheme", ref: "gs://bucket/library/u1/f1/report.pdf", under: libraryPlace, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, ok, shipped, err := reader.Stat(ctx, tc.ref, tc.under)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Stat(%q, %q) resolved: %+v ok=%v", tc.ref, tc.under, info, ok)
				}
				return
			}
			if err != nil || ok != tc.ok || shipped != tc.shipped || (ok && (info.Key != tc.key || info.Size != tc.size)) {
				t.Fatalf("Stat(%q) = %+v ok=%v shipped=%v err=%v", tc.ref, info, ok, shipped, err)
			}
		})
	}
}

func TestAccountBytesListAndOpen(t *testing.T) {
	ctx := context.Background()
	m := newMemoryBlobService()
	const tree = "sites/site-1/v0123456789ab/"
	m.put(exportTestContainer, tree+"index.html", []byte("<html>"))
	m.put(exportTestContainer, tree+"assets/app.js", []byte("js"))
	m.put(exportTestContainer, tree+"stale.js", []byte("retired"))
	m.retired[exportTestContainer+"/"+tree+"stale.js"] = true
	m.put(exportTestContainer, "sites/site-1/v0123456789abc/other.html", []byte("a sibling prefix"))
	m.put(exportTestContainer, "exports/accounts/e1/account.memqlx", []byte("an export"))
	m.put("elsewhere", tree+"index.html", []byte("other container"))
	// A listing that answers beyond the tree: the reader passes none of it on.
	m.listExtra = []string{"exports/accounts/e1/account.memqlx", "library/u2/f9/theirs.pdf", tree + "../escape"}
	reader := exportTestClient(t, m).AccountBytes()

	got, err := reader.List(ctx, "blob://"+strings.TrimSuffix(tree, "/"), sitePlace)
	if err != nil {
		t.Fatal(err)
	}
	want := []BlobInfo{{Key: tree + "assets/app.js", Size: 2}, {Key: tree + "index.html", Size: 6}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	r, err := reader.Open(ctx, got[1].Key, sitePlace)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r)
	_ = r.Close()
	if string(body) != "<html>" {
		t.Fatalf("Open = %q", body)
	}

	for _, tc := range []struct {
		ref   string
		under []string
	}{
		{"library/u1/f1/report.pdf", []string{"library/u1"}},
		{"file:///app/os", sitePlace},
		{"blob://", sitePlace},
		{"blob://../x/", sitePlace},
		{"https://blob.test/memql/sites/", sitePlace},
		{"blob://" + tree, nil},
		{"blob://" + tree, []string{"sites/site-2"}},
		{"blob://sites/site-1", sitePlace},
		{"blob://sites", []string{"sites"}},
		{"blob://sites/", []string{"sites"}},
		{"blob://exports", []string{"exports"}},
		{"blob://exports/", []string{"exports"}},
		{"blob://exports/accounts", []string{"exports"}},
		{"blob://Exports/accounts", []string{"Exports"}},
		{"blob://sites%2Fsite-1/v0123456789ab", sitePlace},
	} {
		if _, err := reader.List(ctx, tc.ref, tc.under); err == nil {
			t.Errorf("List(%q, %q) was accepted", tc.ref, tc.under)
		}
	}
	for _, tc := range []struct {
		key   string
		under []string
	}{
		{"blob://" + tree, sitePlace},
		{"https://blob.test/elsewhere/" + tree + "index.html", sitePlace},
		{"https://blob.test/memql/" + tree + "index.html", sitePlace},
		{"../x", sitePlace},
		{"exports/accounts/e1/account.memqlx", []string{"exports/accounts"}},
		{"", sitePlace},
		{tree + "index.html", nil},
		{tree + "index.html", []string{"sites/site-2"}},
		{"sites/site-1", sitePlace},
		{tree + "index.html/", sitePlace},
	} {
		if _, err := reader.Open(ctx, tc.key, tc.under); err == nil {
			t.Errorf("Open(%q, %q) was accepted", tc.key, tc.under)
		}
	}
	if _, err := reader.Open(ctx, "library/u1/none", []string{"library/u1"}); !IsBlobAbsent(err) {
		t.Fatalf("open of a missing object: %v", err)
	}
	if _, err := reader.Open(ctx, tree+"stale.js", sitePlace); !errors.Is(err, ErrBlobRetired) {
		t.Fatalf("open of a retired object: %v", err)
	}
}

func TestAccountBytesNeedsTheConfiguredContainer(t *testing.T) {
	m := newMemoryBlobService()
	u := exportTestClient(t, m)
	u.libraryContainer = ""
	if _, _, _, err := u.AccountBytes().Stat(context.Background(), "library/u1/f1/report.pdf", libraryPlace); err == nil {
		t.Fatal("a reader with no configured container resolved a key")
	}
	if _, err := u.AccountExports().Create(context.Background(), "exports/accounts/e/f"); err == nil {
		t.Fatal("an export store with no configured container accepted a write")
	}
}
