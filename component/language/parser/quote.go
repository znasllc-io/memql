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
// component/inbound's memqlString, which had a byte-identical body at the time
// it was written, is now a one-line alias for this function. It was written
// independently for the same reason (memql#2957, a webhook body is arbitrary
// third-party text), and the two drifting apart is precisely how one of them
// ends up not matching readString.
//
// That is two collapsed into one, NOT the end of the class, and the difference
// is worth stating rather than claiming a repo-wide collapse that did not
// happen. Still live, and still rendering MemQL string values their own way:
//
//   - component/automations/steps/function.go renders string function args with
//     %q and hands the result to Engine.Execute. That is this exact defect on
//     the automation step path, not an analogue -- the lexer refuses its output
//     for the same bytes, verified.
//   - sdk/go/client does the same across support.go and its generated builders.
//
// Both are separate stories; a helper cannot fix a caller by existing.
// integrations/knowledge and component/automations/steps carry two further
// renderers which are duplicate definitions but NOT defects: they pass control
// bytes through raw, and the lexer accepts those.
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
// encoding/json escapes <, > and & as \u003c, \u003e and \u0026 by default, so that
// JSON can be embedded in a <script> tag. That hazard does not exist here: this
// output goes to the MemQL lexer and nowhere near a browser. The lexer accepts
// all three RAW (verified against readString) and the parsed value is identical
// either way.
//
// What turning it off buys is size, and only size. On an HTML-bearing error the
// emitted literal is roughly 1.7x larger with escaping on -- 7158 bytes against
// 4228, measured on one 4096-byte error-page excerpt. Treat that as an order of
// magnitude and not a constant: the ratio tracks metacharacter density, and a
// differently-shaped excerpt measures differently. A 6x figure is reachable only
// for a degenerate all-metacharacter string.
//
// On the input this file is actually about -- control bytes -- the two settings
// are IDENTICAL: 22018 bytes from 4096 either way, because \u00XX is emitted
// regardless of this flag. So the flag does nothing whatsoever for the memql#3035
// case, and is kept for the HTML one.
//
// Two things it does NOT do, both of which an earlier draft of this comment
// asserted:
//
//   - It does not protect truncation budget. component/outbound's truncateError
//     caps the RAW message and this runs on its return value, so escaping
//     cannot cost one character of the 4096.
//   - It does not make lastError readable. The lexer decodes the escapes, so
//     the persisted value is byte-identical either way, and the statement
//     itself is never logged -- stamp() logs only the error.
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
