package parser

import (
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
// A file-level annotation block (`@version` / `@namespace` at the top of a
// file) is not a declaration preamble and is not reported, because it is not
// followed by a block comment in this shape.

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

	// Block-comment state per line, tracked forwards. inBlock[i] reports
	// whether line i BEGINS inside a block comment; opens[i] whether it starts
	// one at top level.
	inBlock := make([]bool, len(lines))
	opens := make([]bool, len(lines))
	depth := 0
	for i, line := range lines {
		inBlock[i] = depth > 0
		trimmed := strings.TrimSpace(line)
		if depth == 0 && strings.HasPrefix(trimmed, "/*") {
			opens[i] = true
		}
		// A line-comment or a string literal can carry `/*` or `*/` as text.
		// Neither is worth a second state machine here: this detector reports,
		// it does not slice, so a miss costs a diagnostic rather than a
		// mis-parsed declaration. `//` lines are skipped for that reason.
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		depth += strings.Count(line, "/*") - strings.Count(line, "*/")
		if depth < 0 {
			depth = 0
		}
	}

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
		if !somethingFollows(lines, i, inBlock) {
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

// somethingFollows reports whether any real (non-blank, non-comment) line
// appears after the block comment opened at openIdx.
func somethingFollows(lines []string, openIdx int, inBlock []bool) bool {
	for k := openIdx + 1; k < len(lines); k++ {
		if inBlock[k] {
			continue
		}
		trimmed := strings.TrimSpace(lines[k])
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*/") {
			continue
		}
		// A line that merely closes the comment and carries nothing else.
		if after := strings.TrimSpace(strings.TrimPrefix(trimmed, "*/")); after == "" && strings.HasPrefix(trimmed, "*/") {
			continue
		}
		return true
	}
	return false
}
