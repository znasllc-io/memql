// component/edge/scripthash_test.go
package edge

import "testing"

// The hashes are the exact base64 SHA-256 of the script BODY, hardcoded
// rather than recomputed here: a test that hashes with the same call the
// implementation uses asserts only that the function is deterministic, and
// would keep passing if both moved to the wrong digest together.
const (
	hashAlert1  = "'sha256-bhHHL3z2vDgxUt0W3dWQOrprscmda2Y5pLsLg4GF+pI='" // "alert(1)"
	hashConsole = "'sha256-UhmjnsS5y53J5R2rogJZ1Nssebi34s+ck3+meO17Kik='" // `console.log("x")`
)

func TestInlineScriptHashesFindsAnInlineScript(t *testing.T) {
	got := inlineScriptHashes([]byte(`<html><body><script>alert(1)</script></body></html>`))
	if got != hashAlert1 {
		t.Errorf("inlineScriptHashes = %q, want %q", got, hashAlert1)
	}
}

// Every inline script is hashed, in document order, space separated -- the
// shape script-src wants.
func TestInlineScriptHashesJoinsEveryScriptInOrder(t *testing.T) {
	got := inlineScriptHashes([]byte(`<script>alert(1)</script><p>x</p><script>console.log("x")</script>`))
	want := hashAlert1 + " " + hashConsole
	if got != want {
		t.Errorf("inlineScriptHashes = %q, want %q", got, want)
	}
}

// A script with src= is loaded from a URL, not inline. 'self' already covers
// it; hashing it would be hashing an empty body and admitting an empty inline
// script cluster-wide.
func TestInlineScriptHashesSkipsExternalScripts(t *testing.T) {
	got := inlineScriptHashes([]byte(`<script src="/app.js"></script><script>alert(1)</script>`))
	if got != hashAlert1 {
		t.Errorf("inlineScriptHashes = %q, want only the inline one (%q)", got, hashAlert1)
	}
}

// An attribute ORDER or spelling that puts src later in the tag must still
// count as external. The failure this prevents is admitting an empty hash.
func TestInlineScriptHashesSkipsExternalScriptWithOtherAttributesFirst(t *testing.T) {
	got := inlineScriptHashes([]byte(`<script type="module" defer src="/app.js"></script>`))
	if got != "" {
		t.Errorf("inlineScriptHashes = %q, want \"\" for an external script", got)
	}
}

// No inline scripts means NO hashes -- and specifically an empty string, so
// policyForSite reproduces today's policy byte for byte. MemQL OS is a Vite
// build with zero inline scripts and must be unaffected by this whole change.
func TestInlineScriptHashesReturnsEmptyWhenThereAreNone(t *testing.T) {
	if got := inlineScriptHashes([]byte(`<html><body><p>hi</p></body></html>`)); got != "" {
		t.Errorf("inlineScriptHashes = %q, want \"\"", got)
	}
}

// A JSON-LD block is not executed, so CSP never blocks it and it needs no
// hash -- but the scanner hashes it anyway. Making the scanner judge
// executability reimplements a decision browsers make differently, to save
// ~46 bytes. This test PINS that choice so a later "optimisation" is a
// deliberate change rather than a silent one.
func TestInlineScriptHashesHashesEveryTypeIncludingData(t *testing.T) {
	got := inlineScriptHashes([]byte(`<script type="application/ld+json">alert(1)</script>`))
	if got != hashAlert1 {
		t.Errorf("inlineScriptHashes = %q, want the data block hashed too (%q)", got, hashAlert1)
	}
}

// Malformed input reaches this function from a published bundle, which is
// arbitrary bytes the edge did not author. It must not panic and must not
// hang; an unterminated script contributes nothing.
func TestInlineScriptHashesSurvivesMalformedHtml(t *testing.T) {
	for name, in := range map[string]string{
		"unclosed script":  `<script>alert(1)`,
		"unclosed tag":     `<script`,
		"no body":          `<script></script>`,
		"empty input":      ``,
		"bare less-than":   `< script >alert(1)</script>`,
		"nested-ish":       `<script>var s = "</scr" + "ipt>";</script>`,
		"uppercase":        `<SCRIPT>alert(1)</SCRIPT>`,
		"trailing garbage": `<script>alert(1)</script><script`,
	} {
		t.Run(name, func(t *testing.T) {
			_ = inlineScriptHashes([]byte(in)) // must not panic
		})
	}
}

// An empty inline script must NOT be admitted: its hash is the hash of the
// empty string, and publishing that admits `<script></script>` everywhere,
// which is harmless on its own but is a source nobody asked for.
func TestInlineScriptHashesSkipsEmptyScriptBodies(t *testing.T) {
	if got := inlineScriptHashes([]byte(`<script></script>`)); got != "" {
		t.Errorf("inlineScriptHashes = %q, want \"\" for an empty body", got)
	}
}
