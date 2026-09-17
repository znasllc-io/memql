// component/edge/scripthash_review_test.go
package edge

import (
	"log/slog"
	"strings"
	"testing"
)

// A SELF-CLOSING <script/> IS STILL A SCRIPT. `script` is not a void element,
// so a browser ignores the slash and runs the body; x/net/html emits
// SelfClosingTagToken for it while still entering script-data mode, so a loop
// matching only StartTagToken drops the body and emits no hash -- and the
// enforcing policy then blocks a script the page really runs. That is the
// exact "renders but is inert" symptom this file exists to prevent.
func TestInlineScriptHashesHandlesASelfClosingScriptTag(t *testing.T) {
	if got := inlineScriptHashes([]byte(`<script/>alert(1)</script>`)); got != hashAlert1 {
		t.Errorf("inlineScriptHashes = %q, want %q -- a self-closing <script/> was skipped", got, hashAlert1)
	}
}

// DISABLING THE FEATURE IS A CHOICE, NOT AN OVERFLOW. With the cap at 0 every
// document with any inline script would otherwise take the over-cap branch and
// log "over the header cap", once per (bundle version, document) -- so an
// operator who deliberately turned hashing off cannot tell their own decision
// from a misconfiguration, and any genuine overflow drowns in it.
func TestCapOfZeroDisablesQuietlyRatherThanWarningPerDocument(t *testing.T) {
	t.Setenv(cspHashMaxBytesEnv, "0")

	rec, cap := serveCapturing(t, testSite(), map[string]string{"index.html": nextish}, "/")

	if got := scriptSrcOf(t, rec); got != "script-src 'self'" {
		t.Errorf("cap=0 script-src = %q, want the base policy", got)
	}
	if _, ok := cap.find(slog.LevelWarn, "over the header cap"); ok {
		t.Error("cap=0 logged an OVERFLOW warning; disabling is a choice, not an overflow")
	}
}

// A genuine overflow must still warn -- the quiet path above must not silence
// the case the cap exists to report.
func TestAGenuineOverflowStillWarns(t *testing.T) {
	t.Setenv(cspHashMaxBytesEnv, "200")

	_, cap := serveCapturing(t, testSite(), map[string]string{"index.html": manyScripts(40)}, "/")

	if _, ok := cap.find(slog.LevelWarn, "over the header cap"); !ok {
		t.Error("a real overflow stopped warning")
	}
}

// KNOWN LIMIT, PINNED SO IT CANNOT CHANGE SILENTLY.
//
// x/net/html enters script-data mode from the tag name alone, but a browser
// tokenizes a <script> inside <svg> or <math> in the DATA state, decoding
// character references first. So the text content a browser hashes for
// `<svg><script>x&amp;y</script></svg>` is `x&y`, and this scanner hashes the
// literal `x&amp;y`.
//
// The consequence is FAIL-CLOSED -- that script is blocked, exactly as it was
// before this change -- and the shape is absent from a Next.js export, which
// is what this was built for. Fixing it means tracking foreign-content
// insertion modes, which the tokenizer alone does not give us. Recorded here
// rather than discovered later.
func TestForeignContentSvgScriptIsAKnownLimit(t *testing.T) {
	got := inlineScriptHashes([]byte(`<svg><script>x&amp;y</script></svg>`))
	if got == "" {
		t.Fatal("expected a hash (of the literal form) to be emitted")
	}
	if strings.Count(got, "sha256-") != 1 {
		t.Errorf("expected exactly one hash, got %q", got)
	}
	// Documented behaviour: the LITERAL is hashed, not the decoded text.
	// If this ever starts matching the browser, delete this test and the
	// limitation note in scripthash.go.
}
