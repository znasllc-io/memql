package parser

// linemap.go maps a line of the REWRITTEN (lowered) source the parser reads
// back to the line of the AUTHORED source it came from.
//
// Why a map is needed: every whole-file parse runs the struct-form lowering
// first (StripNonProceduralBlocks + NormaliseAll, as compiler.ParseFileSource
// does), so struct-form query / mutation / logic / automation files parse
// instead of false-erroring on the contextual keyword (memql#2359). That
// lowering replaces each struct-form construct block with procedural
// `func (...) { ... }` text of a DIFFERENT line count, so a lexer/parser
// position in the lowered source no longer lines up with the line the author
// wrote. Sense remaps its diagnostics through this map, and
// compiler.ParseFileSource its parse errors (AtAuthoredLine, memql#5356), so
// an editor squiggle and a lint line name the same authored line.
//
// The lowering has one property that makes an accurate map cheap: every
// line OUTSIDE a rewritten construct body -- comments, `use` imports,
// blank lines, the `@`-annotation preamble, and even the args-block field
// lines (which NormaliseAll hoists verbatim) -- survives BYTE-IDENTICAL and
// IN ORDER. So a longest-common-subsequence over the two line sequences
// recovers an EXACT mapping for every preserved line. The only lines with
// no authored counterpart are the synthesized scaffolding (`func (...)`,
// `return`, injected `paginate(...)` / `asOf(...)`); those map, by their
// relative offset, into the authored line span of the construct they were
// synthesized from -- so a position inside a lowered body still lands
// within that construct's authored lines, never on an unrelated line.

import (
	"errors"
	"strings"
)

// AtAuthoredLine moves the parse error in err -- raised over rewritten, the
// lowering of authored -- onto the line the author wrote: its Line becomes the
// authored line, and its Column survives only when that line was preserved
// verbatim (on a synthesized line it points at nothing the author wrote, so it
// becomes 1). A construct-keyword refusal it carries as its Cause moves with
// it. Pos and Token stay the rewritten text's.
//
// The error is changed in place and returned: pass a parse error this parse
// raised and nobody else holds. An error carrying no positioned *ParseError is
// returned untouched, and no map is built for it -- the non-procedural files
// every load meets fail their parse with the unpositioned ErrEmptyInput.
func AtAuthoredLine(err error, authored, rewritten string) error {
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line <= 0 || authored == rewritten {
		return err
	}
	line, exact := NewLineMap(authored, rewritten).Authored(pe.Line)
	pe.Line = line
	if !exact {
		pe.Column = 1
	}
	var u *UnknownConstructKeyword
	if errors.As(pe.Cause, &u) {
		u.Line = line
	}
	return err
}

// LineMap resolves a 1-based rewritten line number to a 1-based authored
// line number. Build one per lowering with NewLineMap.
type LineMap struct {
	authored []int  // authored[i] = authored line (1-based) for rewritten line i+1
	exact    []bool // exact[i] = rewritten line i+1 was preserved verbatim
}

// lcsMaxCells caps the LCS dynamic-programming matrix. 12M int32 cells is
// ~48MB, a transient per-call allocation that comfortably covers the
// largest DSL file in the tree (~1.8k lines -> ~3.3M cells). Beyond the cap
// the map falls back to a clamped identity mapping (see NewLineMap); no
// authored .memql file approaches it.
const lcsMaxCells = 12_000_000

// NewLineMap builds the rewritten->authored line map for one lowering.
func NewLineMap(authored, rewritten string) *LineMap {
	aLines := strings.Split(authored, "\n")
	rLines := strings.Split(rewritten, "\n")

	lm := &LineMap{
		authored: make([]int, len(rLines)),
		exact:    make([]bool, len(rLines)),
	}

	// Degenerate / oversized inputs: clamped identity. Never produces a
	// line outside the authored range, so it is never worse than emitting
	// no position at all.
	if len(aLines) == 0 || len(aLines)*len(rLines) > lcsMaxCells {
		for i := range rLines {
			lm.authored[i] = clampLine(i+1, len(aLines))
			lm.exact[i] = false
		}
		return lm
	}

	pairs := lcsLinePairs(aLines, rLines)

	// Walk the matched pairs, filling the rewritten-only lines between
	// consecutive matches (a "hunk") by relative offset into the authored
	// side of that hunk. Virtual sentinels at (-1,-1) and (nAuth,nRew)
	// bound the first and last hunks.
	prevA, prevR := -1, -1
	fillHunk := func(nextA, nextR int) {
		aStart := prevA + 1 // first authored-only line of the hunk (0-based)
		aEnd := nextA - 1   // last authored-only line (inclusive); < aStart if none
		for r := prevR + 1; r < nextR; r++ {
			k := r - (prevR + 1) // offset within the rewritten hunk
			var a int            // 0-based authored line to anchor on
			switch {
			case aEnd >= aStart:
				a = aStart + k
				if a > aEnd {
					a = aEnd // rewritten hunk longer than authored: clamp to its end
				}
			case prevA >= 0:
				a = prevA // no authored lines here: anchor on the line before
			default:
				a = 0 // hunk before the first match
			}
			lm.authored[r] = clampLine(a+1, len(aLines))
			lm.exact[r] = false
		}
	}
	for _, p := range pairs {
		fillHunk(p.a, p.r)
		lm.authored[p.r] = clampLine(p.a+1, len(aLines))
		lm.exact[p.r] = true
		prevA, prevR = p.a, p.r
	}
	fillHunk(len(aLines), len(rLines))

	return lm
}

// Authored returns the authored line for a 1-based rewritten line, clamped
// into range, and whether that line was preserved verbatim -- in which case
// a column on it is still meaningful; on a synthesized line it is not.
func (lm *LineMap) Authored(line int) (int, bool) {
	if len(lm.authored) == 0 {
		return 1, false
	}
	idx := line - 1
	if idx < 0 {
		idx = 0
	} else if idx >= len(lm.authored) {
		idx = len(lm.authored) - 1
	}
	return lm.authored[idx], lm.exact[idx]
}

func clampLine(line, n int) int {
	if n <= 0 {
		return 1
	}
	if line < 1 {
		return 1
	}
	if line > n {
		return n
	}
	return line
}

// linePair is one matched (authored, rewritten) line index in the LCS,
// both 0-based.
type linePair struct{ a, r int }

// lcsLinePairs returns the longest common subsequence of the two line
// slices as matched index pairs in increasing order. Standard O(n*m)
// dynamic programming; the caller caps n*m via lcsMaxCells.
func lcsLinePairs(a, b []string) []linePair {
	n, m := len(a), len(b)
	// dp[i][j] = LCS length of a[i:] and b[j:].
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		row, next := dp[i], dp[i+1]
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				row[j] = next[j+1] + 1
			} else if next[j] >= row[j+1] {
				row[j] = next[j]
			} else {
				row[j] = row[j+1]
			}
		}
	}

	var pairs []linePair
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			pairs = append(pairs, linePair{a: i, r: j})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return pairs
}
