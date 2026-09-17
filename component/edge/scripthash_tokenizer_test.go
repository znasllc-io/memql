// component/edge/scripthash_tokenizer_test.go
package edge

import (
	"strings"
	"testing"
)

// THE RULE EVERY CASE BELOW TESTS: the hash must be of exactly the bytes the
// BROWSER will execute. A hash of anything else matches nothing, so the
// script is blocked -- which is the bug this whole change exists to fix,
// reintroduced by the fix itself and invisible in the HTML.
//
// Each case here is a place where a substring scan and an HTML tokenizer
// disagree. They were all found by adversarial review of the first
// implementation, which hand-rolled the scan; the scanner now delegates to
// golang.org/x/net/html, whose script-data state machine is the same one
// browsers implement.

const (
	// The LF-normalised body of a script written with CRLF line endings.
	hashCRLFNormalised = "'sha256-UXNer4npOCf/F4eCtHkKG99a5wMJMyYh4BeQWH3QeYQ='" // "\nvar a = 1;\n"
	hashEndTagInString = "'sha256-LdnK9TN2JX1aoxapXFMgd92Rq8vOg49A/iC3H5WCZBU='" // `var s = "</scriptx";`
	hashDup            = "'sha256-SWutTkqidY1WWe7tZaTPCReI1Zu8lfs57vBJNk1rRLA='" // "var d=1;"
)

// CR NORMALISATION. The HTML input-stream preprocessor converts every CRLF
// and lone CR to LF BEFORE tokenization (HTML Standard 13.2.3.5), so a
// browser never sees a CR in an element's text content. A file written with
// Windows line endings -- or by any tool that emits them -- therefore hashes
// differently on disk than in the browser.
func TestInlineScriptHashesNormalisesCarriageReturns(t *testing.T) {
	for name, doc := range map[string]string{
		"CRLF":    "<script>\r\nvar a = 1;\r\n</script>",
		"lone CR": "<script>\rvar a = 1;\r</script>",
	} {
		t.Run(name, func(t *testing.T) {
			if got := inlineScriptHashes([]byte(doc)); got != hashCRLFNormalised {
				t.Errorf("inlineScriptHashes = %q, want the LF-normalised %q", got, hashCRLFNormalised)
			}
		})
	}
}

// END-TAG NAME BOUNDARY. `</script` closes the element only when followed by
// whitespace, `/` or `>`. `</scriptx` does not, and a scan that stops there
// truncates the body.
func TestInlineScriptHashesDoesNotEndOnANonBoundaryEndTag(t *testing.T) {
	doc := `<script>var s = "</scriptx";</script>`
	if got := inlineScriptHashes([]byte(doc)); got != hashEndTagInString {
		t.Errorf("inlineScriptHashes = %q, want %q -- the body was truncated at `</scriptx`", got, hashEndTagInString)
	}
}

// A `</script >` with a space, and `</SCRIPT>`, both close the element.
func TestInlineScriptHashesAcceptsEveryValidEndTagSpelling(t *testing.T) {
	for name, doc := range map[string]string{
		"space before gt": "<script>alert(1)</script >",
		"uppercase":       "<SCRIPT>alert(1)</SCRIPT>",
		"mixed case":      "<ScRiPt>alert(1)</ScRiPt>",
	} {
		t.Run(name, func(t *testing.T) {
			if got := inlineScriptHashes([]byte(doc)); got != hashAlert1 {
				t.Errorf("inlineScriptHashes = %q, want %q", got, hashAlert1)
			}
		})
	}
}

// QUOTED ATTRIBUTE VALUES ARE NOT ATTRIBUTES. An attribute whose VALUE
// contains " src=" must not make an inline script look external -- the
// script is real, it executes, and failing to hash it blocks it.
func TestInlineScriptHashesIsNotFooledBySrcInsideAnAttributeValue(t *testing.T) {
	doc := `<script data-note="load src=/x.js later">alert(1)</script>`
	if got := inlineScriptHashes([]byte(doc)); got != hashAlert1 {
		t.Errorf("inlineScriptHashes = %q, want %q -- an attribute VALUE was read as a src attribute", got, hashAlert1)
	}
}

// AN UNQUOTED VALUE CONTAINING A QUOTE is a recoverable parse error that
// browsers still terminate at `>`. Treating the quote as opening a quoted
// region runs to end of document and, in the first implementation, abandoned
// hashing for EVERY remaining script in the page.
func TestInlineScriptHashesSurvivesAStrayQuoteInAnUnquotedAttribute(t *testing.T) {
	doc := `<script data-owner=it's>alert(1)</script>`
	if got := inlineScriptHashes([]byte(doc)); got != hashAlert1 {
		t.Errorf("inlineScriptHashes = %q, want %q -- a stray apostrophe swallowed the document", got, hashAlert1)
	}
}

// ...and specifically must not take the REST OF THE PAGE with it. This is the
// severe form: one stray quote anywhere silently un-hydrates everything below.
func TestAStrayQuoteDoesNotAbandonTheRestOfTheDocument(t *testing.T) {
	doc := `<div title=it's></div><script>alert(1)</script>`
	if got := inlineScriptHashes([]byte(doc)); !strings.Contains(got, hashAlert1) {
		t.Errorf("inlineScriptHashes = %q, want it to still find the later script", got)
	}
}

// `<script` INSIDE A COMMENT OR AN ATTRIBUTE VALUE IS NOT AN ELEMENT. A
// substring scan starts a bogus element there and then consumes the NEXT real
// script's end tag, dropping its hash.
func TestInlineScriptHashesIgnoresScriptTextThatIsNotAnElement(t *testing.T) {
	for name, doc := range map[string]string{
		"in a comment":        `<!-- <script> --><script>alert(1)</script>`,
		"in an attribute":     `<div data-x="<script>"></div><script>alert(1)</script>`,
		"in a title (RCDATA)": `<title>&lt;script&gt;</title><script>alert(1)</script>`,
		"in a textarea":       `<textarea><script></textarea><script>alert(1)</script>`,
	} {
		t.Run(name, func(t *testing.T) {
			got := inlineScriptHashes([]byte(doc))
			if got != hashAlert1 {
				t.Errorf("inlineScriptHashes = %q, want exactly %q", got, hashAlert1)
			}
		})
	}
}

// DEDUPLICATION. A repeated identical script is one digest; repeating the
// source expression only consumes the byte cap, lowering the ceiling from
// "distinct scripts that fit" to "script occurrences that fit".
func TestInlineScriptHashesEmitsEachDistinctHashOnce(t *testing.T) {
	doc := `<script>var d=1;</script><script>var d=1;</script><script>var d=1;</script>`
	if got := inlineScriptHashes([]byte(doc)); got != hashDup {
		t.Errorf("inlineScriptHashes = %q, want the single %q", got, hashDup)
	}
}

// Dedup must not reorder or drop distinct scripts.
func TestDeduplicationKeepsEveryDistinctScript(t *testing.T) {
	doc := `<script>alert(1)</script><script>var d=1;</script><script>alert(1)</script>`
	got := inlineScriptHashes([]byte(doc))
	if want := hashAlert1 + " " + hashDup; got != want {
		t.Errorf("inlineScriptHashes = %q, want %q", got, want)
	}
}

// The scanner is fed bytes from a published bundle -- arbitrary input the
// edge did not author -- so no input may panic or fail to terminate.
func TestInlineScriptHashesTerminatesOnHostileInput(t *testing.T) {
	for name, doc := range map[string]string{
		"unterminated quote":  `<script data-x="` + strings.Repeat("a", 5000),
		"unterminated script": `<script>` + strings.Repeat("a", 5000),
		"many open tags":      strings.Repeat("<script", 2000),
		"nul bytes":           "<script>\x00alert(1)\x00</script>",
		"deep nesting":        strings.Repeat("<div>", 5000) + "<script>alert(1)</script>",
		"lone lt":             strings.Repeat("<", 5000),
	} {
		t.Run(name, func(t *testing.T) {
			_ = inlineScriptHashes([]byte(doc)) // must return
		})
	}
}
