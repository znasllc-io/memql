package parser

import (
	"sort"
	"strings"
)

// orphaned_preamble.go -- memql#2965.
//
// preambleStartOf walks backwards from a declaration header over contiguous
// `@` and `//` lines and stops at anything else. A `*/` closing a block comment
// is neither, so the walk stops there and the annotations above the comment are
// left OUT of the slice:
//
//	@executor("integration.workbench.dispatchHost")
//	@description("does real work")
//	/*
//	builtin zzParked { a string }
//	*/
//	builtin zzLive { b string }
//
// `zzLive` is registered with no executor at all. It cannot be dispatched, and
// nothing says so -- the silent-absence class memql#2830 outlawed.
//
// # Why this reports rather than reattaches
//
// memql#2965 offered three options; this is option 2, "keep the behaviour and
// make it loud", and the reason is that option 1 -- reattaching the
// annotations across the comment -- has to GUESS.
//
// In the source above the annotations sit directly on top of `zzParked`. They
// are at least as likely to have been written for the declaration that was
// parked as for the one below it, and reattaching them would hand `zzLive` an
// executor its author never wrote. That is over-attribution, and an earlier
// revision of the memql#2896 PR was caught in review encoding exactly it: the
// scanner would report an executor the engine can never dispatch.
//
// Reporting has neither failure mode. The author is told the annotations are
// attached to nothing, and decides which declaration they belong to -- which is
// a question only they can answer.
//
// # Scope
//
// Deliberately narrow: an `@` run immediately above a block comment, where
// something follows the comment. That is the measured defect and nothing wider.
//
// A file-level annotation block (`@version` / `@namespace` at the top of a
// file) IS reported when a block comment separates it from the first
// declaration -- the `@version("1.0.0")` / `@namespace(...)` / `/* ---- */`
// banner shape. That is deliberate, not a false positive: conceptSlices drops
// those annotations exactly as the builtin path drops an `@executor`, so the
// concept registers at the default version with nothing logged. Only the
// conventional blank line after a file header keeps such a file quiet, because
// a blank line ends the run before the comment does.
//
// # Agreeing with the lexer about what a comment is
//
// Block-comment state comes from CommentSpans, not from counting `/*` and `*/`
// per line. The two disagree in four reachable ways, and every one of them is a
// FALSE NEGATIVE rather than a false alarm: MemQL block comments do NOT nest
// (baseparser leaves the block state at the first `*/`), while a counter does,
// so `/* ... /* ... */` leaves it stuck open; and a `/*` inside a string
// literal, a backtick literal, or a trailing `//` comment opens nothing at all
// while a counter believes it did. Because a run is only reported when the
// comment above it is at top level, a counter stuck open suppresses every
// remaining orphan IN THE WHOLE FILE. CommentSpans is string- and
// backtick-aware and non-nesting by construction, so it cannot drift from the
// lexer -- which is the same argument #2872 makes for not keeping two
// implementations of one lexical answer.

// OrphanedPreamble is one `@`-attribute run that a block comment separates from
// whatever declaration follows it.
type OrphanedPreamble struct {
	// Line is the 1-based line of the FIRST `@` line in the run -- where an
	// author looks to fix it.
	Line int
	// Attributes is the run's text, trimmed, newline-separated. Carried so a
	// diagnostic can name what was orphaned rather than only where.
	Attributes string
	// CommentLine is the 1-based line of the `/*` that broke the run.
	CommentLine int
}

// OrphanedPreambles reports every `@`-attribute run in source that a block
// comment separates from the declaration below it.
//
// It scans the ORIGINAL source, not the blanked view, for the same reason
// preambleStartOf does (rule 3 in declaration_slices.go): BlankComments blanks
// `//` lines too, and a `//` line is part of a preamble.
func OrphanedPreambles(source string) []OrphanedPreamble {
	lines := strings.Split(source, "\n")

	// Byte offset at which each line starts. strings.Split consumed one "\n"
	// per boundary, so the running total adds it back.
	lineStart := make([]int, len(lines))
	off := 0
	for i, line := range lines {
		lineStart[i] = off
		off += len(line) + 1
	}

	// Block-comment state per line, taken from the lexer's own view. inBlock[i]
	// reports whether line i BEGINS inside a block comment; opens[i] whether a
	// block comment opens the line; openEnd[i] is the offset just past the `*/`
	// that closes it.
	inBlock := make([]bool, len(lines))
	opens := make([]bool, len(lines))
	openEnd := make([]int, len(lines))
	for _, span := range CommentSpans(source) {
		// `//` line comments are not separators -- preambleStartOf accepts
		// them inside a run -- so only block comments matter here.
		if span.End-span.Start < 2 || source[span.Start:span.Start+2] != "/*" {
			continue
		}
		openLine := sort.SearchInts(lineStart, span.Start+1) - 1
		// Only a comment that OPENS its line. A `/*` trailing real code does
		// not end a preamble run, because that line is code rather than an
		// annotation, and preambleStartOf would have stopped on it anyway.
		if strings.TrimSpace(lines[openLine][:span.Start-lineStart[openLine]]) == "" {
			opens[openLine] = true
			openEnd[openLine] = span.End
		}
		for i := openLine + 1; i < len(lines) && lineStart[i] < span.End; i++ {
			inBlock[i] = true
		}
	}

	// The comment-blanked view answers "is there real code after this
	// comment" without a second scan: blanking preserves byte offsets and
	// total length, so an offset into source indexes the same place here.
	blanked := BlankComments(source)

	var out []OrphanedPreamble
	for i := range lines {
		if !opens[i] {
			continue
		}
		// Walk back over the contiguous preamble run, exactly as
		// preambleStartOf does: `@` and `//` lines, nothing else.
		start := -1
		sawAttribute := false
		for k := i - 1; k >= 0; k-- {
			if inBlock[k] {
				break
			}
			trimmed := strings.TrimSpace(lines[k])
			if strings.HasPrefix(trimmed, "@") {
				sawAttribute = true
				start = k
				continue
			}
			if strings.HasPrefix(trimmed, "//") {
				start = k
				continue
			}
			break
		}
		if !sawAttribute || start < 0 {
			continue
		}
		// Only a run the comment separates from something. A block comment at
		// the END of a file orphans nothing, and reporting it would be noise.
		if !somethingFollows(blanked, openEnd[i]) {
			continue
		}

		var attrs []string
		for k := start; k < i; k++ {
			if t := strings.TrimSpace(lines[k]); t != "" {
				attrs = append(attrs, t)
			}
		}
		out = append(out, OrphanedPreamble{
			Line:        start + 1,
			Attributes:  strings.Join(attrs, "\n"),
			CommentLine: i + 1,
		})
	}
	return out
}

// somethingFollows reports whether any real code appears after the block
// comment that ended at closeOff, given the comment-blanked view of the source.
//
// Working off the blanked view rather than walking lines is what makes the
// SAME-LINE close work: in `*/ builtin zzLive {`, the declaration begins on the
// line that closes the comment, and any line-oriented test that skips a
// `*/`-prefixed line skips the declaration with it -- reporting nothing for a
// run the loader really does orphan. Blanking leaves comments as spaces and
// preserves offsets, so "is there anything left after closeOff" is the whole
// question, and trailing comments and blank lines answer it correctly for free.
func somethingFollows(blanked string, closeOff int) bool {
	if closeOff < 0 || closeOff >= len(blanked) {
		return false
	}
	return strings.TrimSpace(blanked[closeOff:]) != ""
}
