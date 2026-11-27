package edge

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const canonicalIndexing = `{"version":1,"mode":"canonical","canonicalOrigin":"https://public.example","publicPaths":["/","/about"]}`

func TestBundleIndexingHTMLResponses(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, host, target string
		noindex                      bool
	}{
		{"legacy", "", "public.example", "/account", false},
		{"disabled root", `{"version":1,"mode":"disabled"}`, "public.example", "/", true},
		{"canonical root", canonicalIndexing, "public.example", "/", false},
		{"case equivalent host", canonicalIndexing, "PUBLIC.EXAMPLE", "/", false},
		{"declared document", canonicalIndexing, "public.example", "/about", false},
		{"private fallback", canonicalIndexing, "public.example", "/account", true},
		{"query root", canonicalIndexing, "public.example", "/?probe=1", true},
		{"empty query", canonicalIndexing, "public.example", "/?", true},
		{"alias host", canonicalIndexing, "alias.example", "/", true},
		{"host suffix", canonicalIndexing, "public.example.attacker.test", "/", true},
		{"host trailing dot", canonicalIndexing, "public.example.", "/", true},
		{"host noncanonical port", canonicalIndexing, "public.example:8443", "/", true},
		{"encoded root", canonicalIndexing, "public.example", "/%2f", true},
		{"encoded path", canonicalIndexing, "public.example", "/%61bout", true},
		{"dot alias", canonicalIndexing, "public.example", "/private/../about", true},
		{"slash alias", canonicalIndexing, "public.example", "//about", true},
		{"document alias", canonicalIndexing, "public.example", "/about.html", true},
		{"invalid declared policy", `{"version":2,"mode":"canonical"}`, "public.example", "/", true},
		{"static asset unchanged", `{"version":1,"mode":"disabled"}`, "public.example", "/app.js", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := mapOpener{"index.html": "<html><script>globalThis.example=1</script></html>", "about.html": "<html>about</html>", "app.js": "example"}
			if tc.metadata != "" {
				files[bundleIndexingFile] = tc.metadata
			}
			h := NewHandler(Options{Resolver: staticResolver{site: &Site{ID: "site", Hostname: tc.host, Status: "live", Kind: "spa"}}, Opener: files})
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				req := httptest.NewRequest(method, "https://"+tc.host+tc.target, nil)
				req.Header.Set("X-Forwarded-Host", "public.example")
				req.Header.Set("Forwarded", "host=public.example;proto=https")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("%s status=%d", method, rec.Code)
				}
				if got := strings.Contains(strings.Join(rec.Header().Values("X-Robots-Tag"), ","), "noindex"); got != tc.noindex {
					t.Fatalf("%s noindex=%v want %v", method, got, tc.noindex)
				}
				etag := rec.Header().Get("ETag")
				if etag == "" {
					t.Fatal("expected document/asset validator")
				}
				req = httptest.NewRequest(method, "https://"+tc.host+tc.target, nil)
				req.Header.Set("If-None-Match", etag)
				conditional := httptest.NewRecorder()
				h.ServeHTTP(conditional, req)
				if conditional.Code != 304 {
					t.Fatalf("conditional %s=%d", method, conditional.Code)
				}
				if strings.Join(conditional.Header().Values("X-Robots-Tag"), ",") != strings.Join(rec.Header().Values("X-Robots-Tag"), ",") {
					t.Fatal("304 lost indexing policy")
				}
				if conditional.Header().Get("Content-Security-Policy") != rec.Header().Get("Content-Security-Policy") {
					t.Fatal("304 changed CSP")
				}
			}
		})
	}
}

func TestBundleIndexingPreservesPreviewAndRestrictiveHeaders(t *testing.T) {
	h := NewHandler(Options{})
	for _, raw := range []string{canonicalIndexing, `{"version":1,"mode":"disabled"}`} {
		files := fstest.MapFS{bundleIndexingFile: &fstest.MapFile{Data: []byte(raw)}}
		rec := httptest.NewRecorder()
		previewHeaders(rec)
		rec.Header().Add("X-Robots-Tag", "examplebot: nosnippet")
		h.setBundleIndexing(rec, httptest.NewRequest("HEAD", "https://public.example/", nil), &Site{}, files, "index.html", "")
		joined := strings.Join(rec.Header().Values("X-Robots-Tag"), ",")
		if !strings.Contains(joined, "noindex, nofollow") || !strings.Contains(joined, "examplebot: nosnippet") || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("preview/existing restrictions changed")
		}
	}
}

func TestBundleIndexingClosedPolicy(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"mode":"disabled"}`, canonicalIndexing,
		` {"mode":"canonical","version":1,"publicPaths":["/"],"canonicalOrigin":"https://[::1]:8443"} `,
	} {
		if _, ok := parseBundleIndexing([]byte(raw)); !ok {
			t.Fatalf("valid policy rejected: %s", raw)
		}
	}
	bad := []string{
		"", `null`, `[]`, `{}`, `{"version":1,"mode":null}`, `{"version":null,"mode":"disabled"}`,
		`{"version":1.0,"mode":"disabled"}`, `{"version":"1","mode":"disabled"}`, `{"version":2,"mode":"disabled"}`,
		`{"version":1,"mode":"disabled","unknown":true}`, `{"version":1,"mode":"disabled","publicPaths":[]}`,
		`{"version":1,"mode":"disabled","mode":"canonical"}`, `{"version":1,"mode":"disabled","version":1}`,
		`{"version":1,"mode":"disabled"} {}`, `{"version":1,"mode":"disabled"} null`, `{"version":1,"mode":"other"}`,
		strings.Repeat(" ", 4096) + `{}`, string([]byte{0xff}),
	}
	for _, origin := range []string{"", "http://public.example", "https://PUBLIC.example", "https://user@public.example", "https://public.example/", "https://public.example?", "https://public.example#", "https://public.example:443", "https://public.example:0", "https://public.example:65536", "https://public.example:0443", "https://public.example:", "https://public.example.", "https://bad..example", "https://-bad.example", "https://bad_name.example", "https://[bad]", "https://public.example/path"} {
		raw, _ := json.Marshal(map[string]any{"version": 1, "mode": "canonical", "canonicalOrigin": origin, "publicPaths": []string{"/"}})
		bad = append(bad, string(raw))
	}
	for _, paths := range []any{nil, []string{}, []string{"/", "/"}, []string{"about"}, []string{"//about"}, []string{"/a/../b"}, []string{"/about/"}, []string{"/%61bout"}, []string{"/a?b"}, []string{"/a#b"}, []string{"/a\\b"}, []string{"/a\nb"}, []string{"/é"}, []string{"/" + strings.Repeat("a", 1024)}, make([]string, 33), []any{1}, "/"} {
		raw, _ := json.Marshal(map[string]any{"version": 1, "mode": "canonical", "canonicalOrigin": "https://public.example", "publicPaths": paths})
		bad = append(bad, string(raw))
	}
	for i, raw := range bad {
		if _, ok := parseBundleIndexing([]byte(raw)); ok {
			t.Errorf("invalid policy %d accepted", i)
		}
	}
}

type indexingFile struct {
	io.Reader
	read     int
	closeErr error
}

func (f *indexingFile) Read(p []byte) (int, error) {
	n, err := f.Reader.Read(p)
	f.read += n
	return n, err
}
func (f *indexingFile) Close() error               { return f.closeErr }
func (f *indexingFile) Stat() (fs.FileInfo, error) { return nil, errors.New("unexpected stat") }

type indexingFS struct {
	file *indexingFile
	err  error
}

func (f indexingFS) Open(name string) (fs.File, error) {
	if name != bundleIndexingFile {
		return nil, errors.New("unexpected path")
	}
	return f.file, f.err
}

type indexingBrokenReader struct{}

func (indexingBrokenReader) Read([]byte) (int, error) { return 0, fs.ErrNotExist }

func TestBundleIndexingReadFailureAndBound(t *testing.T) {
	for _, tc := range []struct {
		name              string
		fsys              fs.FS
		absent, cacheable bool
	}{
		{"true absence", indexingFS{err: fs.ErrNotExist}, true, true},
		{"permission", indexingFS{err: fs.ErrPermission}, false, false},
		{"other open error", indexingFS{err: errors.New("transient")}, false, false},
		{"read disappeared", indexingFS{file: &indexingFile{Reader: indexingBrokenReader{}}}, false, false},
		{"close failed", indexingFS{file: &indexingFile{Reader: strings.NewReader(canonicalIndexing), closeErr: errors.New("close")}}, false, false},
		{"empty present", indexingFS{file: &indexingFile{Reader: strings.NewReader("")}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, cacheable := readBundleIndexingMetadata(tc.fsys)
			if (data[0] == 0) != tc.absent || cacheable != tc.cacheable {
				t.Fatalf("state=%d cacheable=%v", data[0], cacheable)
			}
		})
	}
	f := &indexingFile{Reader: strings.NewReader(strings.Repeat("x", 8192))}
	data, _ := readBundleIndexingMetadata(indexingFS{file: f})
	if data[0] != 1 || f.read != 4097 {
		t.Fatalf("oversize state/read=%d/%d", data[0], f.read)
	}
}

type indexingBlob struct {
	mu        sync.Mutex
	objects   map[string][]byte
	gets      map[string]int
	transient bool
}

func (b *indexingBlob) Get(_ context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gets[key]++
	if b.transient && strings.HasSuffix(key, bundleIndexingFile) {
		b.transient = false
		return nil, errors.New("transient")
	}
	if data, ok := b.objects[key]; ok {
		return data, nil
	}
	return nil, ErrNotFound
}

func TestBundleIndexingImmutableCacheIsNotRequestDecision(t *testing.T) {
	const ref = "blob://sites/example/v0123456789ab/"
	key := "sites/example/v0123456789ab/" + bundleIndexingFile
	client := &indexingBlob{objects: map[string][]byte{key: []byte(canonicalIndexing)}, gets: map[string]int{}}
	opener := NewBlobOpener(client)
	h := NewHandler(Options{})
	for _, tc := range []struct {
		target  string
		blocked bool
	}{{"https://public.example/", false}, {"https://alias.example/", true}, {"https://public.example/?x=1", true}, {"https://public.example/about", false}} {
		fsys, _ := opener.Open(ref)
		rec := httptest.NewRecorder()
		h.setBundleIndexing(rec, httptest.NewRequest("GET", tc.target, nil), &Site{BundleRef: ref}, fsys, "index.html", "")
		if (rec.Header().Get("X-Robots-Tag") != "") != tc.blocked {
			t.Fatal("cached a request-specific decision")
		}
	}
	if client.gets[key] != 1 {
		t.Fatalf("policy reads=%d", client.gets[key])
	}
	// An immutable version's true absence is negatively cached, not repeatedly fetched.
	missing := "blob://sites/example/v1123456789ab/"
	for i := 0; i < 2; i++ {
		fsys, _ := opener.Open(missing)
		data := h.bundleIndexingMetadata(&Site{BundleRef: missing}, fsys)
		if data[0] != 0 {
			t.Fatal("missing not preserved")
		}
	}
	if client.gets["sites/example/v1123456789ab/"+bundleIndexingFile] != 1 {
		t.Fatal("absence not cached")
	}
	// A publication changes the key and cannot inherit the old allow policy.
	next := "blob://sites/example/v2123456789ab/"
	client.objects["sites/example/v2123456789ab/"+bundleIndexingFile] = []byte(`{"version":1,"mode":"disabled"}`)
	fsys, _ := opener.Open(next)
	if h.bundleIndexingMetadata(&Site{BundleRef: next}, fsys)[0] != 1 {
		t.Fatal("republish reused old metadata")
	}
}

func TestBundleIndexingTransientFailureRecoversAndFilePolicyRereads(t *testing.T) {
	const ref = "blob://sites/example/v0123456789ab/"
	key := "sites/example/v0123456789ab/" + bundleIndexingFile
	client := &indexingBlob{objects: map[string][]byte{key: []byte(canonicalIndexing)}, gets: map[string]int{}, transient: true}
	opener := NewBlobOpener(client)
	h := NewHandler(Options{})
	fsys, _ := opener.Open(ref)
	if h.bundleIndexingMetadata(&Site{BundleRef: ref}, fsys)[0] != 1 {
		t.Fatal("transient read did not deny")
	}
	if h.bundleIndexingMetadata(&Site{BundleRef: ref}, fsys)[0] != 2 || client.gets[key] != 2 {
		t.Fatal("transient error persisted")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, bundleIndexingFile)
	fsys = os.DirFS(dir)
	for _, raw := range []string{canonicalIndexing, `{"version":1,"mode":"disabled"}`, canonicalIndexing} {
		if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		data := h.bundleIndexingMetadata(&Site{BundleRef: "file://" + dir}, fsys)
		if (data[0] == 2) != (raw == canonicalIndexing) {
			t.Fatal("mutable file metadata cached")
		}
	}
}

func (b *indexingBlob) GetWithLimit(ctx context.Context, key string, limit int64) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok || limit != 4096 {
		return nil, errors.New("bounded metadata contract missing")
	}
	data, err := b.Get(ctx, key)
	if int64(len(data)) > limit {
		return nil, errors.New("too large")
	}
	return data, err
}

func TestBundleIndexingPolicyOnlyFileEditInvalidatesHTML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>unchanged</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, bundleIndexingFile)
	h := NewHandler(Options{Resolver: staticResolver{site: &Site{ID: "site", Hostname: "public.example", Status: "live", Kind: "spa", BundleRef: "file://" + dir}}, Opener: NewFileOpener()})
	previous := ""
	for _, raw := range []string{`{"version":1,"mode":"disabled"}`, canonicalIndexing, `{"version":1,"mode":"disabled"}`, ""} {
		if raw == "" {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "https://public.example/", nil)
		req.Header.Set("If-None-Match", previous)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Header().Get("ETag") == previous {
			t.Fatal("policy-only edit answered304, which retains the prior cached header")
		}
		if (rec.Header().Get("X-Robots-Tag") != "") != (raw == `{"version":1,"mode":"disabled"}`) {
			t.Fatal("wrong new policy")
		}
		previous = rec.Header().Get("ETag")
	}
}

type indexingDownload struct {
	calls          int
	ctx            context.Context
	container, key string
	limit          int64
	err            error
}

func (d *indexingDownload) Download(context.Context, string, string) ([]byte, error) {
	panic("unbounded download must not be used")
}
func (d *indexingDownload) DownloadWithLimit(ctx context.Context, container, key string, limit int64) ([]byte, error) {
	d.calls++
	d.ctx = ctx
	d.container = container
	d.key = key
	d.limit = limit
	return []byte(canonicalIndexing), d.err
}

func TestBundleIndexingUsesBoundedAzureRead(t *testing.T) {
	downloader := &indexingDownload{}
	opener := NewBlobOpener(NewAzureBlobClient(downloader, "bundle-container"))
	fsys, err := opener.Open("blob://sites/example/v0123456789ab/")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := readBundleIndexingMetadata(fsys)
	if data[0] != 2 || downloader.calls != 1 || downloader.container != "bundle-container" || downloader.key != "sites/example/v0123456789ab/"+bundleIndexingFile || downloader.limit != 4096 {
		t.Fatal("metadata did not use exact bounded storage read")
	}
	deadline, ok := downloader.ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		t.Fatal("metadata I/O deadline absent")
	}
	// No bounded method is not a missing file and cannot silently fall back to Get.
	fsys, _ = NewBlobOpener(&stubBlob{objects: map[string][]byte{}}).Open("blob://sites/example/v0123456789ab/")
	data, cacheable := readBundleIndexingMetadata(fsys)
	if data[0] != 1 || cacheable {
		t.Fatal("unsupported backend allowed or cached")
	}
}

func TestBundleIndexingExactByteBoundaryAndAzureAbsence(t *testing.T) {
	raw := canonicalIndexing + strings.Repeat(" ", 4096-len(canonicalIndexing))
	if _, ok := parseBundleIndexing([]byte(raw)); !ok {
		t.Fatal("exact4096-byte policy refused")
	}
	if _, ok := parseBundleIndexing([]byte(raw + " ")); ok {
		t.Fatal("4097-byte policy accepted")
	}
	for _, tc := range []struct {
		code   string
		absent bool
	}{{"BlobNotFound", true}, {"ResourceNotFound", true}, {"ContainerNotFound", false}, {"AuthorizationFailure", false}} {
		t.Run(tc.code, func(t *testing.T) {
			downloader := &indexingDownload{err: &azcore.ResponseError{ErrorCode: tc.code, StatusCode: 404}}
			fsys, _ := NewBlobOpener(NewAzureBlobClient(downloader, "bundle-container")).Open("blob://sites/example/v0123456789ab/")
			data, cacheable := readBundleIndexingMetadata(fsys)
			if (data[0] == 0) != tc.absent || cacheable != tc.absent {
				t.Fatalf("state=%d cacheable=%v", data[0], cacheable)
			}
		})
	}
}
