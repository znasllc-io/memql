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
// component/inbound's memqlString is now a one-line alias for this function. The
// two bodies were byte-identical as of bfc7c1d7, when this one was first written;
// naming the commit because "byte-identical" stopped being true of the CURRENT
// body the moment SetEscapeHTML(false) landed below. It was written
// independently for the same reason (memql#2957, a webhook body is arbitrary
// third-party text), and the two drifting apart is precisely how one of them
// ends up not matching readString.
//
// # The rest of the class, swept in memql#3192
//
// memql#3099 recorded that several other renderers still turned Go values into
// MemQL literals their own way, and deliberately declined to enumerate them --
// two successive attempts had both been incomplete, so the honest form was a
// pointer to the search rather than a count. That sweep is done. The reliable
// search is still `grep -rn 'Sprintf("%q"' --include=*.go` over the
// statement-building paths; what it now finds is error messages and the two
// deliberate exemptions named below.
//
// Converted to call this function (each with a real-lexer regression test):
//
//   - component/automations/steps: renderMemQLValue (function args ->
//     Engine.Execute), buildInsertQuery (concept / id / parent / aliasOf), and
//     jsonString (the step-execution record).
//   - component/language/compiler: expressionToString, valueToString and
//     mutationToString, the automation serialize/re-parse boundary. Reachable
//     from authored DSL, because readString DECODES \u00XX into a real control
//     byte that %q then re-encoded in a form readString rejects.
//   - component/language/parser: reconstructTokens, which rebuilt PayloadRaw
//     from lexed tokens -- so a literal this package accepted could not be
//     re-read by this package.
//   - core/liveknowledge's memqlQuote. That package is an L0 leaf with zero
//     in-repo imports (memql#3164), so it cannot import this one; the quoter is
//     injected by integrations/liveknowledge instead of copied down.
//   - sdk/go/client, fixed at the emitter (sdk/gen/emit_go.go) and regenerated,
//     plus support.go's renderMemQLValue.
//   - component/memql: encodeForMemqlSubstitution and renderDryRunMemQLValue,
//     and integrations/knowledge's hand-rolled escape table. None of those three
//     was a live defect -- json.Marshal escapes correctly and the lexer accepts
//     a raw control byte -- but each was a separate DEFINITION of the escape
//     set, which is what this file exists to prevent.
//   - component/memql's expressionToSource, which had no callers at all and was
//     deleted rather than converted.
//
// Two %q call sites are deliberately NOT this defect and must not be swept into
// it: component/memql's jsonschemaSingleQuoted builds a redaction needle that
// has to match text the jsonschema library printed with Go's %q, and
// integrations/knowledge's pgStringArray builds a PostgreSQL array literal.
// Neither goes near this lexer. Both say so at their definition.
//
// # The second layer: NUL and jsonb
//
// One caller of THIS function is on the wrong side of a related boundary, which
// belongs here rather than left for someone to rediscover: component/inbound
// stages a raw third-party webhook body through memqlString, and a NUL in that
// body renders as an escape the lexer accepts and PostgreSQL's jsonb then refuses,
// so the row cannot be staged at all. That is memql#3035's defect one file over.
// It predates the collapse -- the body this replaced emitted the same escape --
// and it is deliberately NOT fixed here: unlike an error message, a webhook body's
// bytes are load-bearing, so choosing between rejecting and substituting is a
// decision this helper cannot make on its caller's behalf. Filed as memql#3098.
//
// That is why this function quotes and nothing more. Every site converted in
// memql#3192 made its own call and recorded it where the code is: none
// substitute, because none of them is the boundary that stores the value --
// they are in-process function args, node identifiers, a read-path filter, an
// author's source text round-tripping, and the client side of the wire.
// Substitution is a storage decision, and a renderer is not where storage
// decisions belong.
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
// emitted literal is somewhere around 1.7x to 2x larger with escaping on. The
// range is deliberate: the ratio is a function of metacharacter density and
// nothing else, so any single measurement is a property of its fixture rather
// than of this code, and two reviewers measuring different excerpts got 1.69x and
// 1.96x. The one figure worth pinning is the ceiling, because it is exactly
// reproducible: strings.Repeat("<", 4096) gives 24578 bytes on against 4098 off,
// i.e. 6x, and only a degenerate all-metacharacter string reaches it.
//
// On the input this file is actually about -- control bytes -- the two settings are
// IDENTICAL, byte for byte, whatever the density: \u00XX is emitted regardless of
// this flag, and a string free of <, > and & cannot differ between the two. So the
// flag does nothing whatsoever for the memql#3035 case; it is kept for the HTML
// one.
//
// Two things it does NOT do, both of which an earlier draft of this comment
// asserted:
//
//   - It does not protect truncation budget. component/outbound's truncateError
//     caps the message -- after its own NUL substitution, so "RAW" would now be
//     wrong -- and QuoteString runs on its return value, so escaping cannot cost
//     one character of the 4096.
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
