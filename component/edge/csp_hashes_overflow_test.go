// component/edge/csp_hashes_overflow_test.go
package edge

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// capturingHandler records the slog records written through it, so a test can
// assert that a degradation was REPORTED and not merely survived. A silent
// fallback is the failure mode this whole path is designed against: the page
// renders, nothing hydrates, and no operator is told which page to look at.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}
func (c *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturingHandler) WithGroup(string) slog.Handler      { return c }

func (c *capturingHandler) find(level slog.Level, substr string) (slog.Record, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Level == level && strings.Contains(r.Message, substr) {
			return r, true
		}
	}
	return slog.Record{}, false
}

func attrsOf(r slog.Record) map[string]string {
	out := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.String()
		return true
	})
	return out
}

// manyScripts builds a document with n distinct inline scripts, so the hash
// list grows past any cap the test sets.
func manyScripts(n int) string {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < n; i++ {
		b.WriteString("<script>var x" + strconv.Itoa(i) + " = " + strconv.Itoa(i) + ";</script>")
	}
	b.WriteString("</body></html>")
	return b.String()
}

func serveCapturing(t *testing.T, site *Site, files map[string]string, path string) (*httptest.ResponseRecorder, *capturingHandler) {
	t.Helper()
	cap := &capturingHandler{}
	h := NewHandler(Options{
		Resolver: staticResolver{site: site},
		Opener:   datedOpener(files),
		Logger:   slog.New(cap),
	})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, cap
}

// OVER THE CAP, FAIL CLOSED: no hashes at all rather than a longer header or
// a weaker policy. That page renders un-hydrated -- exactly the behaviour
// this cluster has today -- while every other page on the site works, and the
// security posture is unchanged either way.
func TestOverTheCapNoHashesAreEmitted(t *testing.T) {
	t.Setenv(cspHashMaxBytesEnv, "200")

	rec, _ := serveCapturing(t, testSite(), map[string]string{"index.html": manyScripts(40)}, "/")

	if got := scriptSrcOf(t, rec); got != "script-src 'self'" {
		t.Errorf("over-cap script-src = %q, want the base %q", got, "script-src 'self'")
	}
}

// ...and never by relaxing. A browser IGNORES 'unsafe-inline' when a hash is
// present, so the ONLY moment it would take effect is exactly this one.
func TestOverTheCapDoesNotFallBackToUnsafeInline(t *testing.T) {
	t.Setenv(cspHashMaxBytesEnv, "200")

	rec, _ := serveCapturing(t, testSite(), map[string]string{"index.html": manyScripts(40)}, "/")

	if got := scriptSrcOf(t, rec); strings.Contains(got, "unsafe-inline") {
		t.Errorf("over-cap script-src relaxed to unsafe-inline: %q", got)
	}
}

// The degradation must be REPORTED, naming the page, or it is invisible: the
// site looks fine in every manifest and simply does not work.
func TestOverTheCapLogsWhichPageAndWhy(t *testing.T) {
	t.Setenv(cspHashMaxBytesEnv, "200")

	_, cap := serveCapturing(t, testSite(), map[string]string{"index.html": manyScripts(40)}, "/")

	rec, ok := cap.find(slog.LevelWarn, "inline script hashes")
	if !ok {
		t.Fatal("no warning logged when the hash list exceeded the cap")
	}
	attrs := attrsOf(rec)
	for _, key := range []string{"site", "path", "scripts", "bytes", "limit"} {
		if _, has := attrs[key]; !has {
			t.Errorf("overflow warning is missing the %q attribute: %v", key, attrs)
		}
	}
}

// A page comfortably under the cap is untouched by any of this.
func TestUnderTheCapHashesAreEmittedNormally(t *testing.T) {
	t.Setenv(cspHashMaxBytesEnv, "8192")

	rec, cap := serveCapturing(t, testSite(), map[string]string{"index.html": nextish}, "/")

	if got := scriptSrcOf(t, rec); !strings.Contains(got, hashAlert1) {
		t.Errorf("under-cap page lost its hashes: %q", got)
	}
	if _, ok := cap.find(slog.LevelWarn, "inline script hashes"); ok {
		t.Error("under-cap page logged an overflow warning")
	}
}

// The DEFAULT cap must admit an ordinary page. A typical document across the
// two sites this fixes carries under ten inline scripts.
func TestDefaultCapAdmitsATypicalPage(t *testing.T) {
	rec, cap := serveCapturing(t, testSite(), map[string]string{"index.html": manyScripts(20)}, "/")

	if got := scriptSrcOf(t, rec); !strings.Contains(got, "sha256-") {
		t.Errorf("default cap refused a 20-script page: %q", got)
	}
	if _, ok := cap.find(slog.LevelWarn, "inline script hashes"); ok {
		t.Error("default cap logged an overflow for a 20-script page")
	}
}

// headerBlockBytes is the size of the response header block on the wire:
// what nginx measures against proxy-buffer-size.
func headerBlockBytes(rec *httptest.ResponseRecorder) int {
	total := len("HTTP/1.1 200 OK\r\n") + len("\r\n")
	for k, vs := range rec.Header() {
		for _, v := range vs {
			total += len(k) + len(v) + len(": ") + len("\r\n")
		}
	}
	return total
}

// THE CAP'S REASON FOR EXISTING, asserted rather than assumed.
//
// The cloud overlays route through ingress-nginx with no buffer tuning in
// this repo, so `proxy-buffer-size: 4k` bounds the whole upstream response
// header block. Over it nginx answers 502 -- WORSE than the un-hydrated page
// the cap promises, because the page does not render at all.
//
// So the default must be low enough that a page sitting exactly AT the cap
// still produces a header block nginx will carry. A default above that limit
// would make the fail-closed guarantee false for every page between the two
// numbers.
func TestAtTheDefaultCapTheHeaderBlockStillFitsTheIngressBuffer(t *testing.T) {
	const nginxProxyBufferSize = 4096

	// Enough scripts to sit just under the default cap, so the emitted
	// header is as large as the default ever allows.
	var biggest int
	for n := 1; n <= 200; n++ {
		rec, _ := serveCapturing(t, testSite(), map[string]string{"index.html": manyScripts(n)}, "/")
		if !strings.Contains(scriptSrcOf(t, rec), "sha256-") {
			break // this page fell over the cap; the previous one was the largest admitted
		}
		biggest = headerBlockBytes(rec)
	}
	if biggest == 0 {
		t.Fatal("no page was admitted at the default cap")
	}
	if biggest >= nginxProxyBufferSize {
		t.Errorf("the largest header block the default cap admits is %d bytes, which nginx "+
			"(proxy-buffer-size %d) answers 502 for. The cap must leave the whole block under that, "+
			"or pages between the two limits 502 instead of degrading.", biggest, nginxProxyBufferSize)
	}
	t.Logf("largest admitted header block at the default cap: %d bytes (nginx limit %d)", biggest, nginxProxyBufferSize)
}

// An unparseable or non-positive cap DISABLES hashing rather than meaning
// "unlimited" -- the same reading bundleCacheBytes takes of a number an
// operator did not think hard about. Disabling costs hydration on hosted
// Next.js sites, loudly and visibly; "unlimited" costs an oversized header
// that some proxy silently drops, which is the failure nobody can diagnose.
func TestUnparseableCapDisablesHashesRatherThanUnbounding(t *testing.T) {
	for name, value := range map[string]string{"garbage": "banana", "zero": "0", "negative": "-1"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(cspHashMaxBytesEnv, value)

			rec, _ := serveCapturing(t, testSite(), map[string]string{"index.html": nextish}, "/")
			if got := scriptSrcOf(t, rec); got != "script-src 'self'" {
				t.Errorf("cap=%q gave script-src %q, want the base policy", value, got)
			}
		})
	}
}
