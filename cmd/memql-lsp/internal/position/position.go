// Package position converts between LSP and MemQL Sense coordinate systems --
// the single most likely source of bugs in the language server (handoff §4), so
// it is isolated here and gated by table-driven round-trip tests.
//
// The two systems disagree on BOTH axes:
//
//   - Origin. LSP positions are 0-based (line and character); MemQL Sense
//     positions are 1-based (line and column). So a line/column crosses by +-1.
//   - Column unit. An LSP `character` counts UTF-16 code units on the line
//     (LSP 3.16 default). A Sense column counts runes: the lexer scans a
//     []rune and does one column++ per rune (component/language/parser/lexer.go).
//     The two agree for every character in the Basic Multilingual Plane (1 rune
//     == 1 UTF-16 code unit) and diverge only for supplementary-plane runes
//     (U+10000 and above), which are one rune but two UTF-16 code units (a
//     surrogate pair). Converting the column therefore needs the line's text,
//     not just arithmetic.
//
// The UTF-16 rounding here matches glsp's own Position.IndexIn: an LSP character
// offset that falls in the middle of a surrogate pair rounds DOWN to the start
// of that rune, and an offset past the end of the line clamps to the line end
// (per the LSP spec). Lines are delimited by "\n", matching glsp and the Sense
// lexer; a trailing "\r" is treated as an ordinary rune.
package position

import (
	"strings"
	"unicode/utf8"

	protocol "github.com/tliron/glsp/protocol_3_16"
)

// FromLSP converts a 0-based LSP (line, character-in-UTF-16-code-units) into a
// 1-based Sense (line, column-in-runes), resolving the line's runes from
// content. Out-of-range inputs clamp to the nearest valid position.
func FromLSP(content string, lspLine, lspChar int) (senseLine, senseCol int) {
	lines := strings.Split(content, "\n")
	li := clamp(lspLine, 0, len(lines)-1)
	runeOff := utf16OffsetToRune(lines[li], atLeastZero(lspChar))
	return li + 1, runeOff + 1
}

// ToLSP converts a 1-based Sense (line, column-in-runes) into a 0-based LSP
// (line, character-in-UTF-16-code-units). It is the inverse of FromLSP for any
// position that lands on a rune boundary within the line.
func ToLSP(content string, senseLine, senseCol int) (lspLine, lspChar int) {
	lines := strings.Split(content, "\n")
	li := clamp(senseLine-1, 0, len(lines)-1)
	u16 := runeOffsetToUTF16(lines[li], atLeastZero(senseCol-1))
	return li, u16
}

// FromLSPPosition is the glsp-typed form of FromLSP.
func FromLSPPosition(content string, p protocol.Position) (senseLine, senseCol int) {
	return FromLSP(content, int(p.Line), int(p.Character))
}

// ToLSPPosition is the glsp-typed form of ToLSP.
func ToLSPPosition(content string, senseLine, senseCol int) protocol.Position {
	l, c := ToLSP(content, senseLine, senseCol)
	return protocol.Position{Line: protocol.UInteger(l), Character: protocol.UInteger(c)}
}

// ByteOffsetForLSP converts an LSP position (0-based line, 0-based UTF-16
// character) to a byte offset into content, for splicing the raw buffer during
// incremental sync. It clamps an out-of-range line to the last line and an
// out-of-range character to the target line's end, matching FromLSP and the LSP
// spec ("a character past the line length defaults back to the line length").
//
// This is deliberately NOT glsp's Position.IndexIn: that helper returns 0 -- not
// the line end -- for a character past the final line of a document with no
// trailing newline (and for any line past EOF), which, spliced through a range,
// silently corrupts the buffer. .memql files routinely lack a trailing newline,
// so end-of-file edits hit exactly that case.
func ByteOffsetForLSP(content string, p protocol.Position) int {
	return byteOffsetForLSP(content, int(p.Line), int(p.Character))
}

func byteOffsetForLSP(content string, line, char int) int {
	line = atLeastZero(line)
	char = atLeastZero(char)

	// Walk to the byte where `line` begins.
	lineStart, cur := 0, 0
	for i := 0; i < len(content); i++ {
		if cur == line {
			break
		}
		if content[i] == '\n' {
			lineStart = i + 1
			cur++
		}
	}
	if cur < line {
		// `line` is past the last line: clamp to the document end so an
		// out-of-range range endpoint extends to (never under-selects) the end.
		return len(content)
	}

	// Within the line, advance `char` UTF-16 code units, clamping at the line
	// end ('\n' or EOF) and rounding a mid-surrogate offset down to the rune.
	byteOff, units := lineStart, 0
	for byteOff < len(content) {
		r, w := utf8.DecodeRuneInString(content[byteOff:])
		if r == '\n' {
			break
		}
		u := utf16Width(r)
		if units+u > char {
			break
		}
		units += u
		byteOff += w
		if units == char {
			break
		}
	}
	return byteOff
}

// utf16OffsetToRune returns the 0-based rune index on line that corresponds to
// the 0-based UTF-16 code-unit offset u16Off. An offset that splits a surrogate
// pair rounds down to the start of that rune; an offset past the line's UTF-16
// length clamps to the line's rune count (one past the last rune).
func utf16OffsetToRune(line string, u16Off int) int {
	if u16Off <= 0 {
		return 0
	}
	units, runeIdx := 0, 0
	for _, r := range line {
		w := utf16Width(r)
		if units+w > u16Off {
			// u16Off lands strictly inside this rune (mid-surrogate); stop
			// before consuming it so it rounds down to the rune start.
			break
		}
		units += w
		runeIdx++
		if units == u16Off {
			break
		}
	}
	return runeIdx
}

// runeOffsetToUTF16 returns the 0-based UTF-16 code-unit offset on line that
// corresponds to the 0-based rune index runeOff. A runeOff past the line's rune
// count clamps to the line's UTF-16 length.
func runeOffsetToUTF16(line string, runeOff int) int {
	if runeOff <= 0 {
		return 0
	}
	units, idx := 0, 0
	for _, r := range line {
		if idx >= runeOff {
			break
		}
		units += utf16Width(r)
		idx++
	}
	return units
}

// utf16Width is the number of UTF-16 code units a rune encodes to: two for a
// supplementary-plane rune (a surrogate pair), one otherwise. A rune decoded
// from a Go string is never a lone surrogate, so this never needs a -1 case.
func utf16Width(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func atLeastZero(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
