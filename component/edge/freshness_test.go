package edge

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the real handler and blob opener while promoting two bundles under
// the same public URLs. Browser validators from release one cannot hide two.
func TestNewBundleAtSameURLCannotReturnStale304(t *testing.T) {
	for _, kind := range []string{"static", "spa"} {
		t.Run(kind, func(t *testing.T) {
			objects := map[string][]byte{}
			paths := map[string]string{"/": "index.html", "/about": "about.html", "/nested/": "nested/index.html", "/legacy.htm": "legacy.htm", "/app.js": "app.js", "/styles.css": "styles.css", "/service-worker.js": "service-worker.js", "/config.json": "config.json"}
			if kind == "spa" {
				paths["/client-route"] = "index.html"
			}
			for _, name := range paths {
				objects["release-one/"+name] = []byte("release one")
				objects["release-two/"+name] = []byte("release two")
			}
			site := &Site{ID: "freshness", Hostname: "shop.example.test", Status: "live", Kind: kind, BundleRef: "blob://release-one/"}
			h := NewHandler(Options{Resolver: staticResolver{site: site}, Opener: NewBlobOpener(newCountingBlobClient(objects))})
			tags := map[string]string{}
			for url := range paths {
				r := get(t, h, url, nil)
				if r.Code != 200 || r.Body.String() != "release one" || !isNoCache(r.Header().Get("Cache-Control")) {
					t.Fatalf("first %s: %d %q %v", url, r.Code, r.Body.String(), r.Header())
				}
				tags[url] = r.Header().Get("ETag")
			}
			site.BundleRef = "blob://release-two/"
			for url, oldTag := range tags {
				r := get(t, h, url, map[string]string{"If-None-Match": oldTag, "If-Modified-Since": time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)})
				if r.Code != 200 || r.Body.String() != "release two" || r.Header().Get("ETag") == oldTag {
					t.Fatalf("second %s: %d %q %v", url, r.Code, r.Body.String(), r.Header())
				}
				if same := get(t, h, url, map[string]string{"If-None-Match": r.Header().Get("ETag")}); same.Code != 304 {
					t.Fatalf("unchanged %s: %d", url, same.Code)
				}
			}
		})
	}
}

func TestFileReleaseWithPreservedSizeAndTimestampUsesContentValidator(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "app.js")
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(Options{})
	request := func(method string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/app.js", nil)
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		h.serveFile(w, r, os.DirFS(dir), "app.js", "file://"+dir)
		return w
	}
	write("release one")
	first := request("GET", nil)
	write("release two")
	for _, method := range []string{"GET", "HEAD"} {
		for _, headers := range []map[string]string{{"If-None-Match": first.Header().Get("ETag")}, {"If-Modified-Since": stamp.UTC().Format(http.TimeFormat)}} {
			second := request(method, headers)
			if second.Code != 200 || second.Header().Get("ETag") == first.Header().Get("ETag") {
				t.Fatalf("%s: %d %v", method, second.Code, second.Header())
			}
			if method == "GET" && second.Body.String() != "release two" {
				t.Fatal(second.Body.String())
			}
			if method == "HEAD" && second.Body.Len() != 0 {
				t.Fatal("HEAD returned a body")
			}
		}
	}
}

func TestOnlyVerifiedContentAddressedAssetsEarnImmutableCaching(t *testing.T) {
	body := "console.log('fresh')"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	site := &Site{ID: "freshness", Hostname: "shop.example.com", Status: "live", Kind: "static"}
	for _, tc := range []struct {
		name      string
		immutable bool
	}{
		{"app." + digest + ".js", true}, {"app-" + digest[:12] + ".js", true},
		{"app.123456789012.js", false}, {"app." + digest[:8] + ".js", false},
		{"app.js", false}, {"page." + digest + ".html", false}, {"page." + digest + ".htm", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := serve(t, site, map[string]string{tc.name: body}, "/"+tc.name)
			if r.Code != 200 || isNoCache(r.Header().Get("Cache-Control")) == tc.immutable {
				t.Fatalf("status=%d headers=%v", r.Code, r.Header())
			}
		})
	}
}
