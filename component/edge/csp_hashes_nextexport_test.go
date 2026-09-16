// component/edge/csp_hashes_nextexport_test.go
package edge

import (
	"net/http"
	"strings"
	"testing"
)

// THE DECISIVE TEST: a document shaped like a real Next.js static export,
// served through the handler, must name every one of its inline scripts.
//
// This is the case the whole change exists for, and it fails against the
// code that shipped before it. The shapes below are the ones a real export
// actually emits, not a simplification:
//
//   - a theme script reading localStorage inside try/catch, which runs before
//     paint and is why a blocked one shows as a flash of the wrong theme
//   - a `type="application/ld+json"` data block, which never executes and so
//     needs no hash, and gets one anyway (see scripthash.go)
//   - the RSC bootstrap `(self.__next_f=self.__next_f||[]).push([0])`
//   - a flight payload containing `\u003c/script\u003e` -- Next escapes the
//     closing tag precisely so it does NOT terminate the element, and a
//     scanner that stopped at the first `</script` substring would hash the
//     wrong bytes and produce a hash matching nothing, which presents as the
//     script being blocked: the exact bug, reintroduced by its own fix
//   - an external `<script src>`, which 'self' already covers
const (
	hashNextTheme  = "'sha256-nf0HWQH/eAW5JCgwbexWnYl6q7GZtlVyRJ53dpORaXA='"
	hashNextLdJSON = "'sha256-O2woq9plC4PragxC5ZtokK/3Wksuh8yso+50bew4jrs='"
	hashNextBoot   = "'sha256-OBTN3RiyCV4Bq7dFqZ5a2pAXjnCcCYeTJMO2I/LYKeo='"
	hashNextFlight = "'sha256-ZEhRDYd8IVcaEk5bM0XBmMa0g+1hoZR31H+VHfK+T8Y='"
)

const nextExport = `<!DOCTYPE html><html lang="en"><head>` +
	`<script>try{var t=localStorage.getItem("memql-theme");if(t==="light")document.documentElement.classList.add("light")}catch(e){}</script>` +
	`<script type="application/ld+json">{"@context":"https://schema.org","@type":"Organization","name":"Example"}</script>` +
	`<link rel="stylesheet" href="/_next/static/css/app.css"/>` +
	`</head><body>` +
	`<div id="__next"><main>server rendered</main></div>` +
	`<script src="/_next/static/chunks/webpack-abc123.js" async=""></script>` +
	`<script>(self.__next_f=self.__next_f||[]).push([0])</script>` +
	`<script>self.__next_f.push([1,"3:I[4707,[],\"\"]\n4:\"$Sreact.suspense\"\n5:\"\\u003c/script\\u003e\"\n"])</script>` +
	`</body></html>`

func TestNextJsExportNamesEveryInlineScript(t *testing.T) {
	rec := serveDated(t, testSite(), map[string]string{"index.html": nextExport}, "/", nil)

	scriptSrc := scriptSrcOf(t, rec)
	for name, want := range map[string]string{
		"theme script":   hashNextTheme,
		"ld+json block":  hashNextLdJSON,
		"RSC bootstrap":  hashNextBoot,
		"flight payload": hashNextFlight,
	} {
		if !strings.Contains(scriptSrc, want) {
			t.Errorf("script-src does not name the %s (%s):\n  %s", name, want, scriptSrc)
		}
	}
}

// Exactly four -- the four inline ones. A fifth would mean the external
// script was hashed, which in practice means an empty body was admitted.
func TestNextJsExportHashesOnlyTheInlineScripts(t *testing.T) {
	rec := serveDated(t, testSite(), map[string]string{"index.html": nextExport}, "/", nil)

	if got := strings.Count(scriptSrcOf(t, rec), "sha256-"); got != 4 {
		t.Errorf("script-src carries %d hashes, want 4 (the inline scripts, not the src'd one):\n  %s",
			got, scriptSrcOf(t, rec))
	}
}

// 'self' must survive alongside the hashes: the external chunks are still
// fetched as same-origin files and a policy of hashes alone would block
// every one of them.
func TestNextJsExportKeepsSelfAlongsideTheHashes(t *testing.T) {
	rec := serveDated(t, testSite(), map[string]string{"index.html": nextExport}, "/", nil)

	if got := scriptSrcOf(t, rec); !strings.HasPrefix(got, "script-src 'self' ") {
		t.Errorf("script-src lost 'self': %q", got)
	}
}

// The whole policy must still parse as a CSP: one script-src directive,
// semicolon separated, no doubled spaces or empty sources. A malformed
// header is ignored WHOLESALE by browsers, which would silently disable
// every other directive too.
func TestNextJsExportPolicyIsWellFormed(t *testing.T) {
	rec := serveDated(t, testSite(), map[string]string{"index.html": nextExport}, "/", nil)
	csp := rec.Header().Get("Content-Security-Policy")

	if strings.Contains(csp, "  ") {
		t.Errorf("policy contains a doubled space, which parses as an empty source: %q", csp)
	}
	if strings.Contains(csp, ";;") {
		t.Errorf("policy contains an empty directive: %q", csp)
	}
	if n := strings.Count(csp, "script-src"); n != 1 {
		t.Errorf("policy has %d script-src directives, want 1: %q", n, csp)
	}
	for _, d := range strings.Split(csp, ";") {
		if strings.TrimSpace(d) == "" && d != "" {
			t.Errorf("policy has a blank directive: %q", csp)
		}
	}
}

// And the same document through the 304 path, because that is the visit most
// people make. Belt and braces with
// TestNotModifiedRepeatsTheInlineScriptHashes -- that one proves the
// mechanism on a minimal document, this one proves it on the real shape.
func TestNextJsExportKeepsItsHashesOnRevalidation(t *testing.T) {
	files := map[string]string{"index.html": nextExport}

	first := serveDated(t, testSite(), files, "/", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag -- cannot reach the 304 path")
	}

	second := serveDated(t, testSite(), files, "/", http.Header{"If-None-Match": {etag}})
	if second.Code != http.StatusNotModified {
		t.Fatalf("second response = %d, want 304", second.Code)
	}
	if got := strings.Count(scriptSrcOf(t, second), "sha256-"); got != 4 {
		t.Errorf("304 carries %d hashes, want 4", got)
	}
}
