package parser

import (
	"fmt"
	"strings"
	"testing"
)

// quote_test.go -- memql#3035.
//
// The property that matters is not "QuoteString produces the bytes I expect".
// It is **whatever QuoteString emits, this package's own lexer accepts, and the
// value survives the round trip.** So these drive the real lexer rather than
// comparing against a hand-written expected string: a table of expected escapes
// is a second definition of the escape set, and two definitions disagreeing is
// the entire defect.

// lexOneString runs src through the lexer and returns the single string
// token's value.
func lexOneString(t *testing.T, src string) (string, error) {
	t.Helper()
	toks, err := NewLexer(src).Tokenize()
	if err != nil {
		return "", err
	}
	for _, tok := range toks {
		if tok.Type == TokenString {
			return tok.Literal, nil
		}
	}
	return "", fmt.Errorf("no string token in %q", src)
}

// The inputs that matter, plus the ones that already worked -- kept together so
// a change that fixes the control bytes by breaking quoting fails here.
var quoteRoundTripInputs = map[string]string{
	"plain":              "a simple message",
	"double quote":       `he said "no"`,
	"backslash":          `C:\path\to\thing`,
	"tab and newline":    "tab\tnewline\n",
	"carriage return":    "cr\r",
	"NUL":                "control \x00 byte",
	"bell and vtab":      "bell \a vtab \v",
	"low control run":    "\x00\x01\x02\x03\x04\x05\x06\x07",
	"DEL":                "del \x7f",
	"unicode":            "héllo → 世界",
	"emoji":              "ship it 🚀",
	"quote and control":  "\"quoted\" and \x01",
	"empty":              "",
	"only a control":     "\x00",
	"newline in the mid": "line one\nline two",
}

func TestQuoteString_RoundTripsThroughTheLexer(t *testing.T) {
	for name, in := range quoteRoundTripInputs {
		t.Run(name, func(t *testing.T) {
			got, err := lexOneString(t, QuoteString(in))
			if err != nil {
				t.Fatalf("the lexer REFUSED what QuoteString produced.\n"+
					"  input:  %q\n  quoted: %s\n  error:  %v\n"+
					"A renderer whose output this package's own lexer rejects makes every "+
					"statement interpolating that value fail to parse (memql#3035).",
					in, QuoteString(in), err)
			}
			if got != in {
				t.Errorf("value did not survive the round trip.\n  want: %q\n  got:  %q", in, got)
			}
		})
	}
}

// TestQuoteString_FixesWhatPercentQBreaks is the reproduction, stated as the
// contrast rather than in isolation.
//
// %q is what component/outbound used, and it is fine for most strings -- which
// is exactly why this survived. The test asserts BOTH halves: %q genuinely
// breaks on these inputs, and QuoteString genuinely does not. Asserting only
// the second would keep passing if the lexer later learned `\x`, and the test
// would then be pinning nothing.
func TestQuoteString_FixesWhatPercentQBreaks(t *testing.T) {
	for name, in := range map[string]string{
		"NUL":       "control \x00 byte",
		"bell":      "bell \a",
		"vtab":      "vtab \v",
		"low bytes": "\x01\x02\x03",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := lexOneString(t, fmt.Sprintf("%q", in)); err == nil {
				t.Errorf("%%q output for %q now LEXES. If the lexer's escape set grew to cover "+
					"Go's, this test is pinning a defect that no longer exists and should be "+
					"revisited -- but QuoteString must still be the renderer, since it is defined "+
					"against the lexer rather than against Go (memql#3035).", in)
			}
			if _, err := lexOneString(t, QuoteString(in)); err != nil {
				t.Errorf("QuoteString output for %q does not lex: %v", in, err)
			}
		})
	}
}

// TestQuoteString_InvalidUTF8IsReplacedNotRefused pins the documented
// trade-off, so it is a decision rather than inherited behaviour.
//
// json.Marshal rewrites invalid UTF-8 to U+FFFD. For the diagnostic text this
// renders, a mangled byte beats an unparseable statement and a permanently
// stuck row. A caller whose bytes are load-bearing should reject at the
// boundary instead (memql#2957 does that for an inbound body).
func TestQuoteString_InvalidUTF8IsReplacedNotRefused(t *testing.T) {
	in := "bad \xff\xfe bytes"

	quoted := QuoteString(in)
	got, err := lexOneString(t, quoted)
	if err != nil {
		t.Fatalf("invalid UTF-8 must not produce something the lexer refuses -- that would "+
			"reintroduce the stuck-row failure through a different door: %v", err)
	}
	if !strings.Contains(got, "\uFFFD") {
		t.Errorf("expected the invalid bytes to become U+FFFD, got %q", got)
	}
	if !strings.Contains(got, "bad ") || !strings.Contains(got, " bytes") {
		t.Errorf("the surrounding valid text must survive, got %q", got)
	}
}

// TestQuoteString_EmitsItsOwnQuotes guards the shape callers depend on: it is
// a complete literal, so a caller writes `lastError: %s` and not
// `lastError: "%s"`. Getting that wrong yields doubled quotes, which lexes as
// an empty string followed by garbage -- a silent wrong value rather than an
// error.
func TestQuoteString_EmitsItsOwnQuotes(t *testing.T) {
	got := QuoteString("x")
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("QuoteString must return a complete literal including quotes, got %s", got)
	}
	if got != `"x"` {
		t.Errorf("got %s, want %q", got, `"x"`)
	}
}
