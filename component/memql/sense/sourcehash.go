package sense

// sourcehash.go defines the CANONICAL content hash of one construct's authored
// source. It lives in Sense because both sides of the drift question have to
// compute it: the engine stamps it on every entry of the construct catalog
// (memql#3749), and the language server computes it for each construct in the
// open document to decide whether that construct is untrained, drifted or
// trained (memql#3758).
//
// One definition, one implementation, imported by both. Two implementations
// that agreed today would disagree on the first edge case, and the failure mode
// is uniquely bad: every construct in the tree reads as DRIFTED at once, which
// looks like a broken cluster rather than a bug in a hash.
//
// # What is normalized away, and why
//
// The hash answers "is this the same construct", not "is this the same file".
// So two edits that cannot change what the engine runs must not change it:
//
//   - Ordinary comments -- `//` line comments and `/* ... */` blocks -- are
//     REMOVED. They document the source for a reader and reach neither the AST
//     nor the registry.
//   - `///` DOC comments are KEPT. They are the arg-description channel
//     (memql#3336 rejected an args-field @description outright, leaving `///`
//     as the only one), so their text is part of what the construct declares.
//     The three slashes are kept in the normalized form so `/// active` cannot
//     collide with a bare identifier `active`.
//   - Insignificant whitespace collapses to a single space, and leading and
//     trailing whitespace is dropped. Re-indenting a body is not a change.
//
// And two things that DO change meaning are preserved byte-for-byte:
//
//   - String literal contents, including interior newlines and runs of spaces.
//     `@description("a  b")` and `@description("a b")` are different
//     descriptions, and a filter comparing against a literal is a different
//     filter.
//   - Every other byte, in order.
//
// # Where a string ends
//
// A string literal ends at its closing quote and nowhere else: it may span
// newlines, and an unterminated one runs to end of input. That is the lexer's
// reading (scanString takes an interior newline as an ordinary rune), and
// component/language/parser's blanker settled on the same answer for the same
// reason -- a scanner that ends a literal at the newline desyncs in BOTH
// directions, exposing string content as code and then swallowing the code
// after the real closing quote.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ConstructSourceHash returns the canonical content hash of one construct's
// authored source: lowercase hex SHA-256 over the normalized form described in
// this file's header. The input is the construct's own declaration text --
// preamble (`///` docs and `@` annotations) through the closing brace -- which
// is exactly what parser.ExtractDeclarationSlices emits on the engine side and
// what Sense's own span scan cuts on the editor side.
//
// Empty (or whitespace-only) input hashes to the empty string rather than to
// the hash of "": a construct with no source is a missing answer, and a missing
// answer must not compare equal to another missing answer as though both were
// known.
func ConstructSourceHash(source string) string {
	normalized := NormalizeConstructSource(source)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// NormalizeConstructSource returns the normalized form ConstructSourceHash
// hashes. Exported so the parity gate (memql#3758) can report WHAT diverged
// when two implementations disagree, instead of only that two hex strings
// differ -- a hash mismatch with no visible input is the least debuggable
// failure this contract can produce.
func NormalizeConstructSource(source string) string {
	var out strings.Builder
	out.Grow(len(source))

	// writeSpace collapses any run of insignificant whitespace to one space,
	// and never emits a leading one.
	writeSpace := func() {
		if out.Len() == 0 {
			return
		}
		if strings.HasSuffix(out.String(), " ") {
			return
		}
		out.WriteByte(' ')
	}

	for i := 0; i < len(source); {
		c := source[i]

		switch {
		case c == '"':
			// A string literal is copied verbatim, escapes included, to its
			// closing quote or to end of input.
			out.WriteByte(c)
			i++
			for i < len(source) {
				if source[i] == '\\' && i+1 < len(source) {
					out.WriteByte(source[i])
					out.WriteByte(source[i+1])
					i += 2
					continue
				}
				out.WriteByte(source[i])
				if source[i] == '"' {
					i++
					break
				}
				i++
			}

		case c == '/' && i+2 < len(source) && source[i+1] == '/' && source[i+2] == '/':
			// A `///` doc comment. Kept, marker included, with its own run of
			// whitespace collapsed like any other text.
			out.WriteString("///")
			i += 3
			for i < len(source) && source[i] != '\n' {
				if isSpace(source[i]) {
					writeSpace()
					i++
					continue
				}
				out.WriteByte(source[i])
				i++
			}

		case c == '/' && i+1 < len(source) && source[i+1] == '/':
			// An ordinary line comment. Dropped; it still separates tokens.
			for i < len(source) && source[i] != '\n' {
				i++
			}
			writeSpace()

		case c == '/' && i+1 < len(source) && source[i+1] == '*':
			// A block comment. Dropped, to its terminator or to end of input.
			i += 2
			for i < len(source) {
				if source[i] == '*' && i+1 < len(source) && source[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			writeSpace()

		case isSpace(c):
			writeSpace()
			i++

		default:
			out.WriteByte(c)
			i++
		}
	}

	return strings.TrimRight(out.String(), " ")
}

// isSpace reports the ASCII whitespace the DSL treats as insignificant between
// tokens. Deliberately narrower than unicode.IsSpace: a non-breaking space is
// not whitespace to the lexer either, so silently collapsing one here would
// make two sources that lex differently hash the same.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
