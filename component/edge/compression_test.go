package edge

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func TestPublicStaticGzipNegotiatesRepresentationValidators(t *testing.T) {
	body := strings.Repeat("static text with enough repetition to compress\n", 100)
	_, h := publishedFixture(t, Bundle{"index.html": []byte("<html><body>" + body + "</body></html>"), "app.js": []byte(body), "style.css": []byte(body), "data.json": []byte(body), "icon.svg": []byte(body)})
	for _, target := range []string{"/", "/app.js", "/style.css", "/data.json", "/icon.svg"} {
		t.Run(target, func(t *testing.T) {
			plain := get(t, h, target, nil)
			encoded := get(t, h, target, map[string]string{"Accept-Encoding": "gzip, br"})
			if encoded.Code != 200 || encoded.Header().Get("Content-Encoding") != "gzip" || encoded.Header().Get("Vary") != "Accept-Encoding" || plain.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatalf("negotiation: %d %v", encoded.Code, encoded.Header())
			}
			reader, err := gzip.NewReader(bytes.NewReader(encoded.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil || !bytes.Equal(decoded, plain.Body.Bytes()) || encoded.Body.Len() >= plain.Body.Len() {
				t.Fatalf("invalid or ineffective gzip: %v identity=%d gzip=%d", err, plain.Body.Len(), encoded.Body.Len())
			}
			if encoded.Header().Get("Content-Type") != plain.Header().Get("Content-Type") || encoded.Header().Get("Content-Length") != strconv.Itoa(encoded.Body.Len()) {
				t.Fatalf("representation metadata wrong: %v", encoded.Header())
			}
			if encoded.Header().Get("ETag") == plain.Header().Get("ETag") {
				t.Fatal("different encoded bytes share a strong ETag")
			}
			conditional := get(t, h, target, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": encoded.Header().Get("ETag")})
			if conditional.Code != 304 || conditional.Body.Len() != 0 || conditional.Header().Get("Content-Encoding") != "gzip" || conditional.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatalf("gzip revalidation: %d %v", conditional.Code, conditional.Header())
			}
			if mixed := get(t, h, target, map[string]string{"If-None-Match": encoded.Header().Get("ETag")}); mixed.Code != 200 || mixed.Header().Get("Content-Encoding") != "" {
				t.Fatalf("gzip validator hid identity bytes: %d %v", mixed.Code, mixed.Header())
			}
			head := httptest.NewRecorder()
			request := httptest.NewRequest("HEAD", target, nil)
			request.Header.Set("Accept-Encoding", "gzip")
			h.ServeHTTP(head, request)
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != encoded.Header().Get("Content-Length") || head.Header().Get("ETag") != encoded.Header().Get("ETag") {
				t.Fatalf("gzip HEAD: %d %v", head.Code, head.Header())
			}
		})
	}
}

func TestGzipQualityRefusalsAndIdentityRanges(t *testing.T) {
	_, h := publishedFixture(t, Bundle{"index.html": []byte("HOME"), "app.js": []byte("0123456789")})
	for header, want := range map[string]bool{"": false, "br": false, "gzip": true, "GZip; q=0.5": true, "gzip;q=0": false, "*;q=1, gzip;q=0": false, "*;q=0.5": true, "gzip;q=NaN": false, "gzip;q=2": false, "gzip;q=bad": false} {
		got := get(t, h, "/app.js", map[string]string{"Accept-Encoding": header})
		if compressed := got.Header().Get("Content-Encoding") == "gzip"; compressed != want {
			t.Errorf("Accept-Encoding %q: %v", header, got.Header())
		}
	}
	compressed := get(t, h, "/app.js", map[string]string{"Accept-Encoding": "gzip"})
	partial := get(t, h, "/app.js", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=2-5"})
	if partial.Code != 206 || partial.Body.String() != "2345" || partial.Header().Get("Content-Encoding") != "" || partial.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("identity range: %d %v %q", partial.Code, partial.Header(), partial.Body.String())
	}
	changedRepresentation := get(t, h, "/app.js", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=2-5", "If-Range": compressed.Header().Get("ETag")})
	if changedRepresentation.Code != 200 || changedRepresentation.Body.String() != "0123456789" {
		t.Fatalf("gzip If-Range applied to identity: %d %q", changedRepresentation.Code, changedRepresentation.Body.String())
	}
	refused := get(t, h, "/app.js", map[string]string{"Accept-Encoding": "gzip", "If-Match": `"stale"`})
	if refused.Code != 412 || refused.Header().Get("Content-Length") != "" || refused.Header().Get("Content-Encoding") != "" || refused.Body.Len() != 0 {
		t.Fatalf("failed precondition promised absent gzip bytes: %d %v", refused.Code, refused.Header())
	}
}

func TestCompressionExcludesPrivateConfigProxyAndBinaryResponses(t *testing.T) {
	site, h := publishedFixture(t, Bundle{"index.html": []byte("HOME"), "photo.webp": []byte("binary"), "_astro/app.js": []byte("script")})
	for _, target := range []string{runtimeConfigPath, "/photo.webp", siteRefreshPath, "/missing.css"} {
		got := get(t, h, target, map[string]string{"Accept-Encoding": "gzip"})
		if got.Header().Get("Content-Encoding") != "" || got.Header().Get("Vary") != "" {
			t.Fatalf("non-public-text response compressed: %s %v", target, got.Header())
		}
	}
	site.Kind = "spa"
	site.CandidateRef = site.BundleRef
	exec := newStubPreviewExec()
	token := exec.issue(t, PreviewGrant{ID: "grant", SiteID: site.ID, CandidateRef: site.BundleRef})
	h.previewExec = exec
	for _, target := range []string{"/", assetPrefixFor(site) + "_astro/app.js"} {
		req := httptest.NewRequest("GET", target, nil)
		req.AddCookie(&http.Cookie{Name: memql.PreviewCookieName, Value: token})
		req.Header.Set("Accept-Encoding", "gzip")
		got := httptest.NewRecorder()
		h.ServeHTTP(got, req)
		if got.Code != 200 || !strings.Contains(got.Header().Get("Cache-Control"), "no-store") || got.Header().Get("Content-Encoding") != "" || strings.Contains(got.Header().Get("Vary"), "Accept-Encoding") {
			t.Fatalf("private preview compressed: %d %v", got.Code, got.Header())
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "authenticated upstream body")
	}))
	defer upstream.Close()
	site.APIProxy = true
	h.apiTarget = upstream.URL
	got := get(t, h, "/_memql/ws", map[string]string{"Accept-Encoding": "gzip"})
	if got.Code != 200 || got.Header().Get("Content-Encoding") != "" || got.Header().Get("Vary") != "" || got.Body.String() != "authenticated upstream body" {
		t.Fatalf("proxy response changed: %d %v %q", got.Code, got.Header(), got.Body.String())
	}
}
