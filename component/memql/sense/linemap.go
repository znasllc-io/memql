package sense

// linemap.go maps positions in the LOWERED (rewritten) MemQL source that
// Diagnose feeds to the lexer/parser back to positions in the AUTHORED
// source the user is editing.
//
// Why a map is needed: Diagnose runs the same lowering the runtime loaders
// run (parser.StripNonProceduralBlocks + parser.NormaliseAll, mirrored by
// compiler.ParseFileSource) so struct-form query / mutation / logic /
// automation files parse instead of false-erroring on the contextual
// keyword (memql#2359). That lowering replaces each struct-form construct
// block with procedural `func (...) { ... }` text of a DIFFERENT line
// count, so a lexer/parser position in the lowered source no longer lines
// up with the authored line the author sees.
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
// synthesized from -- so a diagnostic inside a lowered body still lands
// within that construct's authored lines, never on an unrelated line.

import "strings"

// lineMap resolves a 1-based rewritten line number to a 1-based authored
// line number. It is built once per Diagnose call.
type lineMap struct {
	authored []int  // authored[i] = authored line (1-based) for rewritten line i+1
	exact    []bool // exact[i] = rewritten line i+1 was preserved verbatim
}

// lcsMaxCells caps the LCS dynamic-programming matrix. 12M int32 cells is
// ~48MB, a transient per-call allocation that comfortably covers the
// largest DSL file in the tree (~1.8k lines -> ~3.3M cells). Beyond the cap
// the map falls back to a clamped identity mapping (see newLineMap); no
// authored .memql file approaches it.
const lcsMaxCells = 12_000_000

// newLineMap builds the rewritten->authored line map for one lowering.
func newLineMap(authored, rewritten string) *lineMap {
	aLines := strings.Split(authored, "\n")
	rLines := strings.Split(rewritten, "\n")

	lm := &lineMap{
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

// remap rewrites a diagnostic's start/end positions from rewritten
// coordinates to authored coordinates.
func (lm *lineMap) remap(d Diagnostic) Diagnostic {
	d.Range.Start = lm.pos(d.Range.Start)
	d.Range.End = lm.pos(d.Range.End)
	// A remap can invert the span (e.g. start/end fell on the same lowered
	// line but different authored anchors); normalise so End >= Start.
	if d.Range.End.Line < d.Range.Start.Line ||
		(d.Range.End.Line == d.Range.Start.Line && d.Range.End.Column < d.Range.Start.Column) {
		d.Range.End = Position{Line: d.Range.Start.Line, Column: d.Range.Start.Column + 1}
	}
	return d
}

// pos maps a single rewritten position to authored coordinates. For a line
// that was preserved verbatim the column is meaningful and kept; for a
// synthesized line the column no longer corresponds to anything the author
// wrote, so it collapses to column 1 (point at the line, not a bogus
// offset).
func (lm *lineMap) pos(p Position) Position {
	idx := p.Line - 1
	if len(lm.authored) == 0 {
		return Position{Line: 1, Column: 1}
	}
	if idx < 0 {
		idx = 0
	} else if idx >= len(lm.authored) {
		idx = len(lm.authored) - 1
	}
	line := lm.authored[idx]
	col := p.Column
	if !lm.exact[idx] {
		col = 1
	}
	if col < 1 {
		col = 1
	}
	return Position{Line: line, Column: col}
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
