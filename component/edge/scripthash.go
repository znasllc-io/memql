// component/edge/scripthash.go
package edge

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/sync/singleflight"
)

// INLINE SCRIPT HASHES: why this file exists.
//
// policyForSite serves `script-src 'self'`, which admits scripts fetched as
// separate same-origin files and NOTHING written inline. That is exactly
// right for MemQL OS -- a Vite build, zero inline scripts -- and it is
// incompatible by construction with a Next.js static export, which ships its
// React hydration payload in inline `<script>` tags
// (`self.__next_f.push(...)`). Every one is refused, the RSC stream never
// completes, and NOTHING on the page hydrates: no event handlers, no
// client-side routing, no scroll effects. The page renders and is inert.
//
// The fix is the third thing CSP admits besides files and 'unsafe-inline': a
// hash per inline script. The browser runs only scripts whose body matches a
// listed digest, so this is STRICTLY STRONGER than 'unsafe-inline' rather
// than a relaxation -- injected script has a different body, so it still
// fails. That distinction is the whole reason this is the fix and
// 'unsafe-inline' is not: 'unsafe-inline' cannot tell a shipped script from
// an injected one, which is the XSS defence script-src exists to be.
//
// # Serve time, not publish time
//
// The hashes could be precomputed when a bundle is published. They are not,
// because `file:///app/*` bundles are baked into the edge image and never
// pass through the publisher at all -- MemQL OS is one. Computing here
// covers every bundle kind through one path.
//
// # A real tokenizer, and that is not a detail
//
// The first implementation of the scan was hand-rolled. Adversarial review
// found six ways it disagreed with a browser, and every one of them has the
// SAME consequence: the hash is taken over bytes that are not the element's
// text content, so it matches nothing, so the script is blocked -- which is
// precisely the bug this file exists to fix, reintroduced by the fix and
// invisible in the HTML. The cases were `</scriptx` truncating a body,
// ` src=` inside a quoted attribute VALUE reading as a src attribute, a
// stray apostrophe in an unquoted value swallowing the rest of the document,
// and a literal `<script` inside a comment, an attribute or a <textarea>
// starting a bogus element that then ate the next real script's end tag. A
// seventh was quadratic: a document of `<scriptx` repeated cost ~75s of CPU
// per megabyte on an anonymous GET.
//
// golang.org/x/net/html implements the tokenizer's script-data states, so
// all seven are properties of the library rather than of code maintained
// here -- as are the input-stream preprocessor's newline conversion (CRLF
// and lone CR to LF, HTML Standard 13.2.3.5) and its NUL replacement, both
// of which Tokenizer.Text() applies to raw script data before we see it.
//
// # Every type, including data blocks
//
// A `<script type="application/ld+json">` never executes, so CSP never
// blocks it and it strictly does not need a hash. It gets one anyway.
// Deciding which types execute means reimplementing a judgement browsers
// make differently from each other, and the whole prize is ~46 bytes per
// block. Being wrong in that direction breaks a page; being wrong in this
// one costs header bytes.
//
// # One known limit: a script inside foreign content
//
// The tokenizer enters script-data mode from the tag name alone, but a
// browser tokenizes a <script> inside <svg> or <math> in the DATA state,
// decoding character references first. So for
// `<svg><script>x&amp;y</script></svg>` a browser's text content is `x&y`
// while this hashes the literal `x&amp;y`, and that script is blocked.
// FAIL-CLOSED, exactly as it was before this change, and absent from the
// Next.js exports this was built for; fixing it needs the foreign-content
// insertion-mode tracking the tokenizer alone does not provide.
// TestForeignContentSvgScriptIsAKnownLimit pins the behaviour.

// setContentSecurityPolicy writes the site's policy for the response about to
// be sent. name is the resolved bundle file, or "" when nothing resolved (the
// 404 tail), and only a DOCUMENT contributes hashes.
//
// etag/hasETag are the asset's validator, ALREADY COMPUTED by the caller and
// threaded through rather than derived here. That is not tidiness: on the
// `file://` path assetETagFor falls through to contentETag, which reads the
// whole file and SHA-256s it, so deriving it independently would hash every
// served document a second time on every request, on top of the tokenizer
// pass.
//
// Every caller sits AFTER resolveAsset and BEFORE serveFile -- see the
// paragraph at that call site for why both halves matter.
func (h *Handler) setContentSecurityPolicy(w http.ResponseWriter, r *http.Request, site *Site, fsys fs.FS, name, etag string, hasETag bool) {
	w.Header().Set("Content-Security-Policy",
		policyForSite(r, site, os.Getenv, h.scriptHashesFor(fsys, name, site, r.URL.Path, etag, hasETag)))
}

// cspHashMaxBytesEnv caps the SIZE of the hash list a policy may carry.
const cspHashMaxBytesEnv = "MEMQL_EDGE_CSP_HASH_MAX_BYTES"

// defaultCSPHashMaxBytes is set by what THIS PRODUCT'S OWN INGRESS will
// carry, not by a round number.
//
// The cloud overlays route through ingress-nginx and this repo configures no
// buffer tuning, so the controller default applies: `proxy-buffer-size: 4k`
// bounds the whole upstream RESPONSE HEADER BLOCK. Over it nginx answers
// 502, which is strictly worse than the un-hydrated page the cap exists to
// guarantee -- a cap set above the limit it is protecting does not protect
// anything.
//
// Measured by serving through the real Handler and counting the wire header
// block: it is the hash bytes plus ~700 (the CSP's own base is ~230, the
// other security headers and the status line are the rest). 3072 therefore
// lands the whole block near 3.8KB, leaving ~300 bytes of margin for the
// longer hostnames a real cluster has in connect-src.
//
// RAISING THIS ALONE IS NOT ENOUGH. A larger value needs the ingress buffer
// raised to match (`proxy-buffer-size`), or the page it was raised for
// answers 502 instead of rendering. site-hosting.md says so where operators
// will look.
const defaultCSPHashMaxBytes = 3072

// cspHashMaxBytes resolves the configured cap.
//
// A zero, negative or unparseable value DISABLES hashing rather than meaning
// "unlimited" -- the same reading bundleCacheBytes takes of a number an
// operator did not think hard about, and for a sharper reason here.
// Disabling costs hydration on hosted Next.js sites: visible and exactly the
// state this cluster is in today. "Unlimited" costs an oversized header that
// the ingress rejects, which presents as an intermittent,
// environment-dependent failure with nothing in any log.
func cspHashMaxBytes() int {
	raw := strings.TrimSpace(os.Getenv(cspHashMaxBytesEnv))
	if raw == "" {
		return defaultCSPHashMaxBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// scriptHashesFor is the hash list for one resolved bundle file, or "" when
// the file is not a document, hashing is disabled, the file cannot be read,
// it has no inline scripts, or it needs more header bytes than the cap
// allows.
//
// A read failure answers "" rather than erroring: the response is already
// committed to serving this file, and the honest degradation is the policy
// this cluster served before hashes existed. serveFile reports the failure on
// its own terms a moment later.
func (h *Handler) scriptHashesFor(fsys fs.FS, name string, site *Site, urlPath, etag string, hasETag bool) string {
	if !isDocumentName(name) {
		return ""
	}

	// A ZERO CAP IS THE OFF SWITCH, and returning here is what makes it one.
	// Falling through would take the over-cap branch for every document with
	// any inline script and log "over the header cap" against a limit of 0 --
	// so an operator who deliberately disabled hashing could not tell their
	// own choice from a misconfiguration, and a genuine overflow elsewhere
	// would drown in the noise.
	limit := cspHashMaxBytes()
	if limit <= 0 {
		return ""
	}

	// THE CACHE KEY IS THE ASSET'S OWN VALIDATOR, computed once by the caller
	// and threaded through. Reusing it is the point rather than a shortcut:
	// assetETagFor already answers "these exact bytes" for both bundle
	// schemes, so a republish is a new key, there is no invalidation path to
	// get wrong, and the hash list can never outlive the document it
	// describes. A key of our own would be a second opinion about identity,
	// free to disagree with the one the 304 already trusts.
	hashes := h.scanDocument(fsys, name, etag, hasETag)
	if hashes == "" {
		return ""
	}

	// OVER THE CAP, FAIL CLOSED AND SAY SO. Dropping the hashes leaves this
	// one page inert while every other page on the site works, and changes
	// the security posture not at all. The alternative -- relaxing to
	// 'unsafe-inline' -- would turn XSS protection off on a page chosen by a
	// size threshold rather than by a person, and this is the ONE moment a
	// browser would honour it, since a policy carrying any hash ignores it.
	//
	// The warning is the whole value of failing closed: without it the page
	// renders, nothing hydrates, and every manifest looks correct.
	if len(hashes) > limit {
		h.logger.Warn("edge: dropping inline script hashes over the header cap -- this page will render but will not hydrate",
			"component", "edge",
			"site", site.ID,
			"path", urlPath,
			"file", name,
			"scripts", strings.Count(hashes, "sha256-"),
			"bytes", len(hashes),
			"limit", limit)
		return ""
	}
	return hashes
}

// scanDocument reads and scans one document, answering from the cache when it
// can.
//
// Concurrent misses for the same document are COALESCED. The miss path is a
// whole-file read plus a tokenizer pass -- ~45ms on the largest real page --
// and after a republish (a new key) or a replica restart (an empty cache)
// every concurrent visitor to one URL would otherwise run it independently.
// Both sibling caches in this package already guard their cold paths the same
// way; resolve.go names the herd explicitly.
//
// The EMPTY answer is cached too. A document with no inline scripts would
// otherwise pay a full read and scan on every request -- MemQL OS is exactly
// such a bundle -- which is the cost this cache exists to remove. It needs no
// sentinel: bundleCache charges every entry its key plus a fixed overhead
// (entryCost), so a zero-length value still occupies the budget and evicts
// like any other.
func (h *Handler) scanDocument(fsys fs.FS, name, etag string, hasETag bool) string {
	// No validator means no cache key, and inventing one would be a second
	// opinion about identity that the 304 path would not share. Scan without
	// caching instead.
	if !hasETag {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return ""
		}
		return inlineScriptHashes(data)
	}

	if cached, hit := h.hashCache.Get(etag); hit {
		return string(cached)
	}
	v, _, _ := h.hashSF.Do(etag, func() (any, error) {
		// Re-check inside the group: a concurrent leader may have filled the
		// cache between our miss above and our turn here.
		if cached, hit := h.hashCache.Get(etag); hit {
			return string(cached), nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return "", nil
		}
		hashes := inlineScriptHashes(data)
		h.hashCache.Put(etag, []byte(hashes))
		return hashes, nil
	})
	hashes, _ := v.(string)
	return hashes
}

// isDocumentName reports whether a resolved bundle path is an HTML document --
// the only kind of response whose inline scripts a policy needs to name.
func isDocumentName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm")
}

// inlineScriptHashes returns the space-separated `'sha256-...'` source list
// for every DISTINCT inline script in doc, in first-appearance order, ready
// to append to a script-src directive. It returns "" when there are none --
// which is what keeps policyForSite byte-identical to its old output for
// every bundle with no inline scripts.
//
// doc is bytes from a published bundle -- arbitrary input the edge did not
// author -- and the tokenizer terminates on every one of them.
func inlineScriptHashes(doc []byte) string {
	z := html.NewTokenizer(bytes.NewReader(doc))

	var out []string
	seen := make(map[string]struct{})
	for {
		switch z.Next() {
		case html.ErrorToken:
			// io.EOF or a parse error; either way there is nothing further to
			// read. The tokenizer never blocks on malformed input.
			return strings.Join(out, " ")

		// A SELF-CLOSING <script/> IS STILL A SCRIPT. `script` is not a void
		// element, so a browser ignores the slash and runs the body; the
		// tokenizer reports SelfClosingTagToken while still entering
		// script-data mode, so matching only StartTagToken would discard that
		// body and emit no hash -- blocking a script the page really runs,
		// which is this file's own bug in one spelling.
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if !bytes.Equal(name, scriptTagName) {
				continue
			}
			// An external script's bytes are fetched as a same-origin file,
			// which `self` already admits. Reading the attribute through the
			// tokenizer is what keeps a VALUE containing " src=" from being
			// mistaken for the attribute.
			external := false
			for hasAttr {
				var key []byte
				key, _, hasAttr = z.TagAttr()
				if bytes.Equal(key, srcAttrName) {
					external = true
				}
			}

			// A script element's content is one raw-text token. An EMPTY
			// script has no text token at all and yields the end tag here,
			// which falls through harmlessly.
			if z.Next() != html.TextToken {
				continue
			}
			body := z.Text()
			if external || len(bytes.TrimSpace(body)) == 0 {
				continue
			}

			// DEDUPLICATED. A repeated identical script is one digest; a
			// second copy of the same source expression is inert to the
			// browser and consumes the byte cap, which would lower the
			// ceiling from "distinct scripts that fit" to "script
			// occurrences that fit".
			sum := sha256.Sum256(body)
			hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if _, dup := seen[hash]; dup {
				continue
			}
			seen[hash] = struct{}{}
			out = append(out, hash)
		}
	}
}

var (
	scriptTagName = []byte("script")
	srcAttrName   = []byte("src")
)

// hashGroup is the singleflight type scanDocument coalesces on. Declared here
// beside its only user rather than inline on Handler, so the import and the
// reason sit together.
type hashGroup = singleflight.Group

// cspHashCacheBytes sizes the per-document hash cache. A hash list is at
// most cspHashMaxBytes, so this holds at least several hundred documents and
// in practice every page of a normal site. It is a constant rather than an
// env var deliberately: unlike the bundle byte cache it holds kilobytes
// rather than megabytes, so there is no budget for an operator to tune and
// one fewer knob to set wrong.
const cspHashCacheBytes = 4 << 20
