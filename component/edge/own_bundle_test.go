package edge

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// own_bundle_test.go -- a site is served its own bundle, through the whole read:
// the engine's row, the resolver, the scheme dispatch and the handler, with only
// the storage underneath faked. mapOpener and staticResolver would skip the two
// seams this is about, so neither is used here.

// routedEngine answers each named query from its own rows, for the lookups a
// custom domain walks through; fakeEngine answers every query the same.
type routedEngine map[string][]map[string]any

func (r routedEngine) Execute(_ context.Context, query string) (any, error) {
	name := strings.TrimPrefix(query, "query ")
	if i := strings.Index(name, "("); i >= 0 {
		name = name[:i]
	}
	return r[name], nil
}

// recordingBlobs is object storage that remembers what it was asked for.
type recordingBlobs struct {
	objects map[string][]byte
	asked   []string
}

func (b *recordingBlobs) Get(_ context.Context, key string) ([]byte, error) {
	b.asked = append(b.asked, key)
	if data, ok := b.objects[key]; ok {
		return data, nil
	}
	return nil, ErrNotFound
}

// shippedFiles stands in for the file opener at the directories the edge image
// ships, which are not on a test machine's disk, and hands every other reference
// to the real one. It remembers what it was asked for.
type shippedFiles struct {
	trees  map[string]fstest.MapFS
	opened []string
}

func (s *shippedFiles) Open(ref string) (fs.FS, error) {
	s.opened = append(s.opened, ref)
	if tree, ok := s.trees[ref]; ok {
		return tree, nil
	}
	return NewFileOpener().Open(ref)
}

const ownBundleHost = "shop.example.test"

type ownBundleRig struct {
	blobs *recordingBlobs
	files *shippedFiles
	// directory is a tree on disk holding a page, for a reference that names it.
	directory string
}

func newOwnBundleRig(t *testing.T) *ownBundleRig {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte("DIRECTORY-PAGE"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &ownBundleRig{
		directory: directory,
		blobs: &recordingBlobs{objects: map[string][]byte{
			"sites/v1:platform:site:shop-1/v0123456789ab/index.html": []byte("OWN-CANONICAL-PAGE"),
			"sites/shop-1/v0123456789ab/index.html":                  []byte("OWN-BARE-PAGE"),
			"sites/other/v0123456789ab/index.html":                   []byte("OTHER-SITE-PAGE"),
			"sites/other/v0123456789ab/app.js":                       []byte("OTHER-SITE-ASSET"),
			"sites/shop-1x/v0123456789ab/index.html":                 []byte("OTHER-SITE-PAGE"),
			"elsewhere/v1/index.html":                                []byte("OTHER-PREFIX-PAGE"),
			"sites/shop-1/v1/nested/index.html":                      []byte("NESTED-PAGE"),
			"sites/shop-1/../other/v0123456789ab/index.html":         []byte("OTHER-SITE-PAGE"),
		}},
		files: &shippedFiles{trees: map[string]fstest.MapFS{
			"file:///app/os":          {"index.html": &fstest.MapFile{Data: []byte("SHIPPED-OS-PAGE")}},
			"file:///app/vscode-site": {"index.html": &fstest.MapFile{Data: []byte("SHIPPED-EXTENSION-PAGE")}},
		}},
	}
}

func (r *ownBundleRig) handler(engine Engine, exec PreviewExecutor) http.Handler {
	return NewHandler(Options{
		Resolver:    NewResolver(NewEngineExecutor(engine), time.Minute),
		Opener:      NewMuxOpener(map[string]BundleOpener{"file": r.files, "blob": NewBlobOpener(r.blobs)}),
		PreviewExec: exec,
	})
}

func ownBundleRow(id, bundleRef string) map[string]any {
	return map[string]any{"id": id, "hostname": ownBundleHost, "kind": "static", "status": "live", "bundleRef": bundleRef}
}

func ownBundleGet(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = ownBundleHost
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The reference forms a stored row holds, spelled as the publisher and the seeds
// spell them. A rule that refused one of these would take a working site down.
func TestASiteIsServedItsOwnBundle(t *testing.T) {
	for _, tc := range []struct {
		name, id, bundleRef, want string
	}{
		{"a version under the canonical spelling of its id", "v1:platform:site:shop-1", "blob://sites/v1:platform:site:shop-1/v0123456789ab/", "OWN-CANONICAL-PAGE"},
		{"a version under the bare spelling of its id", "v1:platform:site:shop-1", "blob://sites/shop-1/v0123456789ab/", "OWN-BARE-PAGE"},
		{"the row id arriving bare", "shop-1", "blob://sites/v1:platform:site:shop-1/v0123456789ab/", "OWN-CANONICAL-PAGE"},
		{"the shell, from the directory the image ships it in", "v1:platform:site:os", "file:///app/os", "SHIPPED-OS-PAGE"},
		{"the extension page, from the directory the image ships it in", "v1:platform:site:vscode", "file:///app/vscode-site", "SHIPPED-EXTENSION-PAGE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOwnBundleRig(t)
			rec := ownBundleGet(t, rig.handler(&fakeEngine{rows: []map[string]any{ownBundleRow(tc.id, tc.bundleRef)}}, nil), "/")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("got %d %q, want 200 with %q", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestASiteIsServedNoBundleThatIsNotItsOwn(t *testing.T) {
	rig := newOwnBundleRig(t)
	for _, tc := range []struct {
		name, id, bundleRef string
	}{
		{"another site's version", "v1:platform:site:shop-1", "blob://sites/other/v0123456789ab/"},
		{"the canonical spelling of another site's version", "v1:platform:site:shop-1", "blob://sites/v1:platform:site:other/v0123456789ab/"},
		{"an id the site's own is only a prefix of", "v1:platform:site:shop-1", "blob://sites/shop-1x/v0123456789ab/"},
		{"a prefix no site publishes under", "v1:platform:site:shop-1", "blob://elsewhere/v1/"},
		{"a path below one of its versions", "v1:platform:site:shop-1", "blob://sites/shop-1/v1/nested/"},
		{"a path that leaves its prefix", "v1:platform:site:shop-1", "blob://sites/shop-1/../other/v0123456789ab/"},
		{"a directory", "v1:platform:site:shop-1", "file://" + rig.directory},
		{"the shell's directory, on another site", "v1:platform:site:shop-1", "file:///app/os"},
		{"another directory, on the shell", "v1:platform:site:os", "file://" + rig.directory},
		{"the extension page's directory, on the shell", "v1:platform:site:os", "file:///app/vscode-site"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig.blobs.asked, rig.files.opened = nil, nil
			rec := ownBundleGet(t, rig.handler(&fakeEngine{rows: []map[string]any{ownBundleRow(tc.id, tc.bundleRef)}}, nil), "/")
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("got %d %q, want the 503 a site with nothing to serve answers", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "PAGE") {
				t.Errorf("served %q", rec.Body.String())
			}
			if len(rig.blobs.asked) != 0 || len(rig.files.opened) != 0 {
				t.Errorf("storage was asked for %v and %v; the reference should go no further than the resolver", rig.blobs.asked, rig.files.opened)
			}
		})
	}
}

// The versioned-asset path opens a bundle of its own, separately from the page.
func TestAVersionedAssetCannotComeFromAnotherSite(t *testing.T) {
	rig := newOwnBundleRig(t)
	other := "blob://sites/other/v0123456789ab/"
	target := assetPrefixFor(&Site{BundleRef: other}) + "app.js"
	if target == "app.js" {
		t.Fatal("the reference does not earn a versioned URL, so this asks nothing")
	}
	rec := ownBundleGet(t, rig.handler(&fakeEngine{rows: []map[string]any{ownBundleRow("v1:platform:site:shop-1", other)}}, nil), target)
	if strings.Contains(rec.Body.String(), "OTHER-SITE-ASSET") {
		t.Fatalf("served %d %q", rec.Code, rec.Body.String())
	}
}

// A custom domain reaches the row through a different pair of reads.
func TestACustomDomainIsHeldToTheSameRule(t *testing.T) {
	for _, tc := range []struct {
		name, bundleRef string
		wantCode        int
		wantBody        string
	}{
		{"its own version", "blob://sites/shop-1/v0123456789ab/", http.StatusOK, "OWN-BARE-PAGE"},
		{"another site's version", "blob://sites/other/v0123456789ab/", http.StatusServiceUnavailable, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOwnBundleRig(t)
			rec := ownBundleGet(t, rig.handler(routedEngine{
				"liveCustomDomainByHostname": {{"siteId": "v1:platform:site:shop-1", "hostname": ownBundleHost}},
				"siteById":                   {ownBundleRow("v1:platform:site:shop-1", tc.bundleRef)},
			}, nil), "/")
			if rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("got %d %q, want %d with %q", rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
		})
	}
}

// A candidate is a version like any other, reached through a grant.
func TestAPreviewServesOnlyAVersionOfTheSite(t *testing.T) {
	for _, tc := range []struct {
		name, candidateRef, want, not string
	}{
		{"its own candidate", "blob://sites/shop-1/v0123456789ab/", "OWN-BARE-PAGE", "OWN-CANONICAL-PAGE"},
		{"another site's version falls back to what the public sees", "blob://sites/other/v0123456789ab/", "OWN-CANONICAL-PAGE", "OTHER-SITE-PAGE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOwnBundleRig(t)
			row := ownBundleRow("v1:platform:site:shop-1", "blob://sites/v1:platform:site:shop-1/v0123456789ab/")
			row["candidateRef"] = tc.candidateRef
			exec := newStubPreviewExec()
			token := exec.issue(t, PreviewGrant{ID: "grant-1", SiteID: "shop-1", CandidateRef: tc.candidateRef})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = ownBundleHost
			req.AddCookie(&http.Cookie{Name: memql.PreviewCookieName, Value: token})
			rec := httptest.NewRecorder()
			rig.handler(&fakeEngine{rows: []map[string]any{row}}, exec).ServeHTTP(rec, req)
			if !strings.Contains(rec.Body.String(), tc.want) || strings.Contains(rec.Body.String(), tc.not) {
				t.Fatalf("got %d %q, want %q and not %q", rec.Code, rec.Body.String(), tc.want, tc.not)
			}
		})
	}
}

// No version yet is the state a package run's new draft holds. It is not a
// refusal, so it must not read as one in the log.
func TestASiteWithNoVersionYetIsNotWarnedAbout(t *testing.T) {
	var logged bytes.Buffer
	r := NewResolver(NewEngineExecutor(&fakeEngine{rows: []map[string]any{ownBundleRow("v1:platform:site:shop-1", "")}}), 0).(*resolver)
	r.logger = slog.New(slog.NewTextHandler(&logged, nil))
	site, err := r.Resolve(context.Background(), ownBundleHost)
	if err != nil || site == nil {
		t.Fatalf("Resolve: %v %v", site, err)
	}
	if logged.Len() != 0 {
		t.Fatalf("logged %q", logged.String())
	}
}

// The operator is told once per window, not once per cache fill, and the log
// line names the row and the field to repair.
func TestARefusedBundleIsToldAboutOncePerWindow(t *testing.T) {
	var logged bytes.Buffer
	row := ownBundleRow("v1:platform:site:shop-1", "blob://sites/other/v0123456789ab/")
	row["candidateRef"] = "blob://elsewhere/v1/"
	r := NewResolver(NewEngineExecutor(&fakeEngine{rows: []map[string]any{row}}), 0).(*resolver)
	r.logger = slog.New(slog.NewTextHandler(&logged, nil))
	for range 3 {
		site, err := r.Resolve(context.Background(), ownBundleHost)
		if err != nil || site == nil {
			t.Fatalf("Resolve: %v %v", site, err)
		}
		if site.BundleRef != "" || site.CandidateRef != "" {
			t.Fatalf("the resolved site still carries %q and %q", site.BundleRef, site.CandidateRef)
		}
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logged %d lines, want one per field: %q", len(lines), logged.String())
	}
	for i, field := range []string{"bundleRef", "candidateRef"} {
		if !strings.Contains(lines[i], "field="+field) || !strings.Contains(lines[i], "shop-1") {
			t.Errorf("line %d does not name the site and %s: %q", i, field, lines[i])
		}
	}
}
