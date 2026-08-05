package parser

import (
	"bytes"
	"encoding/json"
	"strings"
)

// QuoteString renders s as a MemQL string literal -- quotes included -- using
// an escape set this package's own lexer accepts.
//
// It lives beside the lexer deliberately. The only correct definition of "a
// MemQL string literal" is "what readString accepts", and a renderer that
// lives anywhere else drifts from it the moment the escape set changes.
//
// # Why not fmt.Sprintf("%q", s)
//
// Go's %q and the MemQL lexer do not agree on the escape set, and the
// disagreement is a hard error rather than a fallback. readString implements
// the JSON escapes and only those -- `" \ / b f n r t u` -- and anything else
// returns `invalid escape character %q at position %d`. %q emits `\x00`, `\a`
// and `\v`, none of which the lexer knows:
//
//	fmt.Sprintf("%q", "control \x00 byte")  ->  "control \x00 byte"
//	lexer:  invalid escape character 'x' at position 10
//
// So a single control byte in an interpolated value makes the whole statement
// fail to parse. That is memql#3035: component/outbound stamped a delivery's
// lastError with %q, and an error string carrying a control byte -- which for
// a webhook delivery can come from a remote server's response or a TLS/DNS
// error naming a hostile hostname -- silently broke the stamping mutation. The
// row then kept whatever status it had, so a request mid-delivery stayed
// `sending` forever: never retried, never reported failed, and with no error
// recorded, because recording the error was the thing that failed.
//
// encoding/json emits exactly the escapes readString implements, including
// `\u00XX` for control characters, which is why it is the encoder here rather
// than a hand-rolled table -- a hand-rolled one would be another definition of
// the escape set, and the defect above is what having two already cost.
//
// There are no longer two: component/inbound's memqlString, which had a
// byte-identical body, is now a one-line alias for this function. It was
// written independently for the same reason (memql#2957, a webhook body is
// arbitrary third-party text), and the two drifting apart is precisely how one
// of them ends up not matching readString.
//
// # Invalid UTF-8
//
// json.Marshal replaces invalid UTF-8 with U+FFFD rather than failing. That is
// accepted here, and it is a deliberate choice rather than an inherited one:
// this renders diagnostic text (an error message) where a mangled byte is far
// better than an unparseable statement and a permanently stuck row. Callers
// rendering data whose bytes are load-bearing should reject invalid UTF-8 at
// the boundary instead -- which is what memql#2957 does for an inbound body.
//
// # HTML escaping is OFF
//
// encoding/json escapes <, > and & to < / > / & by default, so
// that JSON can be embedded in a <script> tag. That hazard does not exist here:
// this output goes to the MemQL lexer and nowhere near a browser. The lexer
// accepts all three RAW (verified against readString), and the parsed value is
// identical either way, so the escaping bought nothing and cost two things --
// up to 6x expansion on the realistic input, an HTML error-page excerpt from a
// webhook, against a 4096-byte truncation cap; and an unreadable lastError for
// something as ordinary as a URL with a query string.
//
// Encoding cannot fail for a string input, so there is no error to return.
func QuoteString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// Unreachable for a string: the only failing kinds are chan, func,
		// complex and cyclic structures. Fall back to an empty literal rather
		// than emitting an unquoted fragment that would corrupt the statement
		// around it.
		return `""`
	}
	// Encode appends a newline that Marshal does not; a literal must not carry
	// one.
	return strings.TrimSuffix(buf.String(), "\n")
}
